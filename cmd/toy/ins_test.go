package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/platform"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

func insTestResolved(t *testing.T, s, tag string) *resolved {
	t.Helper()
	r, err := ref.Parse(s)
	if err != nil {
		t.Fatalf("ref.Parse(%q)：%v", s, err)
	}
	res := &resolved{Ref: r, Owner: r.Owner, Repo: r.Repo, Tag: tag, Trust: store.TrustHost}
	if r.Kind == ref.KindHost {
		res.HostName = r.Host
		res.Host = host.Builtin()[r.Host]
		res.Version = semver.V{Raw: tag}
	}
	return res
}

// 升级判断错了就是装错版本，或者永远不升级。
func TestInsIsNewer(t *testing.T) {
	cases := []struct {
		old, remote string
		want        bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.3.0", "v1.2.0", false}, // 远端更旧就别动，不然一次降级
		{"v1.2.0", "v1.2.1", true},
		// 有一边不是 semver 就只能按「tag 变了」判断，不能当成 0.0.0
		// —— 那会把 release-2024 一路"升级"成谁也没要的东西。
		{"release-2024", "release-2025", true},
		{"release-2024", "release-2024", false},
	}
	for _, c := range cases {
		got := insIsNewer(store.Manifest{Tag: c.old}, &resolved{Tag: c.remote})
		if got != c.want {
			t.Errorf("insIsNewer(%q → %q) = %v，想要 %v", c.old, c.remote, got, c.want)
		}
	}
}

// 铁律 6：钉住的是某个 ref@version 的哈希。id 里不带 tag 的话，正常升级
// 会被报成「哈希变了」，而那句警告天天出现就没人再当回事。
func TestInsTrustIDCarriesTheTag(t *testing.T) {
	res := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insPackageID(res), "github:Derrity/tinyserve"; got != want {
		t.Fatalf("insPackageID = %q，想要 %q", got, want)
	}
	if got, want := insTrustID(res), "github:Derrity/tinyserve@v1.2.0"; got != want {
		t.Fatalf("insTrustID = %q，想要 %q", got, want)
	}
	// 裸地址没有 tag，id 就是那个地址本身 —— 同一个地址内容变了必须拦下来。
	u := insTestResolved(t, "https://example.com/x/toy.toml", "")
	if got := insTrustID(u); got != "https://example.com/x/toy.toml" {
		t.Fatalf("裸地址的 trust id 是 %q", got)
	}
}

// {tag} 是上游给的，展开之后才是真正拼进下载地址的那一串（铁律 7、8）。
func TestInsSafeAsset(t *testing.T) {
	if err := insSafeAsset("gitwip", "scripts/gitwip.sh"); err != nil {
		t.Fatalf("正常的相对路径不该被拒：%v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../../x.sh", "a.sh?raw=1", "a.sh#frag"} {
		if err := insSafeAsset("gitwip", bad); err == nil {
			t.Errorf("%q 应该被拒", bad)
		}
	}
}

// 裸地址那条路没有托管方，资产只能相对 toy.toml 自己的地址解析。
func TestInsAssetURL(t *testing.T) {
	u := insTestResolved(t, "https://example.com/tools/toy.toml", "")
	got, err := insAssetURL(u, "tinyserve.tar.gz", false)
	if err != nil || got != "https://example.com/tools/tinyserve.tar.gz" {
		t.Fatalf("裸地址的资产地址 = %q（err=%v）", got, err)
	}

	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	rel, err := insAssetURL(h, "tinyserve_1.2.0_linux_amd64.tar.gz", false)
	if err != nil || !strings.Contains(rel, "/releases/download/v1.2.0/") {
		t.Fatalf("release 地址 = %q（err=%v）", rel, err)
	}
	// [script] 的值是仓库里的路径，走 raw 模板 —— 走 release 的话带斜杠的
	// 路径会被转义成 %2F，取回来的是一个不存在的资产。
	raw, err := insAssetURL(h, "scripts/gitwip.sh", true)
	if err != nil || !strings.Contains(raw, "scripts/gitwip.sh") {
		t.Fatalf("raw 地址 = %q（err=%v）", raw, err)
	}
}

// 铁律 8：信任等级那一行必须说清楚信的是谁。
func TestInsTrustDetail(t *testing.T) {
	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insTrustDetail(h), "github.com/Derrity"; got != want {
		t.Fatalf("托管方那一档的说明 = %q，想要 %q", got, want)
	}
	// 已签名那一档只能说「哪把钥匙、哪个索引登记的」。签名消息是
	// id + source + toy.toml，里面没有任何东西把公钥和仓库 owner 绑起来，
	// 所以 "Derrity (key …)" 这种写法是在替一把无关的钥匙认领一个名字。
	h.Trust, h.Signer, h.IndexName = store.TrustSigned, "a3f91b2c…", "default"
	got := insTrustDetail(h)
	if !strings.Contains(got, "a3f91b2c…") || !strings.Contains(got, "default") {
		t.Fatalf("已签名那一档要说清楚是哪把钥匙、谁登记的，得到 %q", got)
	}
	if strings.HasPrefix(got, "Derrity") {
		t.Fatalf("不能把仓库 owner 印在勾后面，那读起来就是「他签的」：%q", got)
	}
}

// TestInsTrustDowngradeNeedsForce 钉住「签名不能悄悄消失」：
// 验签名本身拦不住这件事 —— 把 sig 从索引条目里删掉，客户端验都没得验，
// 那个勾就没了，而没人会注意到少了一个勾（规格 §4 的 TOFU、铁律 8）。
func TestInsTrustDowngradeNeedsForce(t *testing.T) {
	signed := &store.Manifest{Name: "tinyserve", Trust: store.TrustSigned, Signer: "a3f9…"}
	for _, c := range []struct {
		name string
		old  *store.Manifest
		res  *resolved
		want bool
	}{
		{"签名没了要拦", signed, &resolved{Trust: store.TrustHost}, true},
		{"降到未验证更要拦", signed, &resolved{Trust: store.TrustUnverified}, true},
		{"还是已签名就放行", signed, &resolved{Trust: store.TrustSigned}, false},
		{"第一次装没得比", nil, &resolved{Trust: store.TrustHost}, false},
		{"本来就没签过不算降级",
			&store.Manifest{Trust: store.TrustHost}, &resolved{Trust: store.TrustHost}, false},
	} {
		if got := insTrustDowngraded(c.old, c.res); got != c.want {
			t.Errorf("%s：insTrustDowngraded = %v，想要 %v", c.name, got, c.want)
		}
	}
}

// 铁律 8：命中的不是第一个候选，就是装了一个不是为这台机器构建的东西。
// 这两种回退失败起来都不像回退的错 —— Rosetta 少装一次是「无法打开」，
// glibc 二进制丢进 Alpine 是「文件不存在」，说明必须在安装那一刻给出来。
func TestInsFallbackNote(t *testing.T) {
	cases := []struct {
		name     string
		cands    []string
		matched  string
		declared []string
		want     []string // 都要出现；空表示这次不该有任何说明
	}{
		{"命中第一个就不吭声", []string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64/gnu",
			[]string{"linux/amd64", "linux/amd64/gnu"}, nil},
		// 这条是这个函数最要紧的一条，也是它以前唯一没盖住的一条：绝大多数项目
		// 只发两段 key，而 linux 上首选候选永远是我们自己合成的 /gnu。判据写成
		// 「没命中首选就报警」的话，每一次普通安装都会多一句假警告 —— 而一条每次
		// 都响的警告会把下面那两条真的一起淹掉。
		{"glibc 机器装只发两段 key 的包：那就是对的那个，不许吭声",
			[]string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64", "linux/arm64"}, nil},
		{"作者按 libc 分了档但没有 glibc 那一档，要说一声",
			[]string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64", "linux/amd64/musl"},
			[]string{"libc", "linux/amd64"}},
		{"Apple Silicon 退回 Intel 要说 Rosetta",
			[]string{"darwin/arm64", "darwin/amd64"}, "darwin/amd64",
			[]string{"darwin/amd64"},
			[]string{"Rosetta", "Intel"}},
		// musl 反过来：裸 linux/amd64 几乎不可能是 musl 构建，所以作者有没有分档
		// 都要说 —— 沉默的代价是一句「文件不存在」，而文件明明就在那儿。
		{"musl 机器退回通用构建要说会像文件不存在（哪怕作者没分档）",
			[]string{"linux/amd64/musl", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64"},
			[]string{"musl", "file is missing"}},
		// glibc 机器兜底到 musl 那一档（候选链末位）。这条几乎总是好消息 ——
		// 只发 musl 一份的项目（ripgrep / zoxide 的 x86-64）靠它才装得上 ——
		// 但仍然要说：装的不是为这台机器构建的那个，而且万一那份 musl 构建是
		// 动态链接的，它在这里就起不来。
		{"glibc 机器只找到 musl 构建：要说它是静态链接所以能跑",
			[]string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}, "linux/amd64/musl",
			[]string{"linux/amd64/musl"},
			[]string{"musl", "statically linked", "linux/amd64"}},
		{"arm64 上同理", []string{"linux/arm64/gnu", "linux/arm64", "linux/arm64/musl"}, "linux/arm64/musl",
			[]string{"linux/arm64/musl"}, []string{"musl", "statically linked"}},
		// 三段候选链上命中中间那个（裸 key）时判据不能变：作者没按 libc 分档，
		// 通用构建就是对的那个，不许因为候选链变长了就开始报假警告。
		{"候选链末位加了 musl 之后，命中裸 key 仍然不许吭声",
			[]string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}, "linux/amd64",
			[]string{"linux/amd64", "linux/arm64"}, nil},
		{"别的回退也要说一声",
			[]string{"linux/armv7", "linux/arm"}, "linux/arm",
			[]string{"linux/arm"},
			[]string{"linux/armv7", "linux/arm"}},
		{"没有候选就无从说起", nil, "linux/amd64", []string{"linux/amd64"}, nil},
	}
	for _, c := range cases {
		got := insFallbackNote(c.cands, c.matched, c.declared)
		if len(c.want) == 0 {
			if got != "" {
				t.Errorf("%s：不该有说明，得到 %q", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s：说明里要有 %q，得到 %q", c.name, w, got)
			}
		}
	}
}

// 挑资产必须走候选顺序，不能只比一个精确的 GOOS/GOARCH。只比一个的话，
// Alpine 上装任何一个「只发了通用 linux 构建」的包都会被判成
// 「没有适配当前平台的构建」—— 而那个包在 Alpine 上很可能是装得上的。
func TestInsItemsFallsBackThroughCandidates(t *testing.T) {
	cands := platform.Candidates()
	if len(cands) < 2 {
		t.Skipf("这台机器只有一个候选（%v），没有回退可验", cands)
	}
	// 挑一个【不等于】GOOS/GOARCH 的候选：只有这样，"只比一个精确 key" 的写法
	// 才会失败而候选顺序会成功 —— 拿 GOOS/GOARCH 本身去测，两种写法都过。
	only := ""
	for _, c := range cands {
		if c != runtime.GOOS+"/"+runtime.GOARCH {
			only = c
			break
		}
	}
	if only == "" {
		t.Skipf("这台机器的候选全等于 GOOS/GOARCH（%v），没有回退可验", cands)
	}
	tf, err := toyfile.Parse(fmt.Appendf(nil,
		"name = \"probe\"\n[bin]\nprobe = \"probe\"\n[artifact]\n%q = \"probe_{version}_x.tar.gz\"\n", only))
	if err != nil {
		t.Fatal(err)
	}
	res := insTestResolved(t, "github:o/r", "v1.0.0")
	res.Toyfile = tf
	items, err := insItems(res)
	if err != nil {
		t.Fatalf("只声明了 %q 的包应该能靠候选顺序装上，得到：%v", only, err)
	}
	if len(items) != 1 || items[0].asset != "probe_1.0.0_x.tar.gz" {
		t.Fatalf("挑出来的资产不对：%+v", items)
	}
}
