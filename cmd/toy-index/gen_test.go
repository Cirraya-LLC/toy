package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// fakeRepo 是假 API 上一个仓库的全部事实。
type fakeRepo struct {
	desc, license, homepage string
	stars                   int
	tag                     string
	assets                  []string
	missing                 bool // 仓库本身 404
	noRelease               bool // 仓库在，但 releases/latest 404
	rateLimited             bool // 403 + 剩余配额 0
}

// fakeAPI 起一个假的 GitHub API。测试一律走它，不联网。
func fakeAPI(t *testing.T, repos map[string]fakeRepo) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/repos/")
		latest := strings.HasSuffix(p, "/releases/latest")
		id := strings.TrimSuffix(p, "/releases/latest")

		repo, ok := repos[id]
		w.Header().Set("X-RateLimit-Limit", "60")
		switch {
		case !ok || repo.missing:
			w.Header().Set("X-RateLimit-Remaining", "59")
			w.WriteHeader(http.StatusNotFound)
			return
		case repo.rateLimited:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "59")

		if !latest {
			json.NewEncoder(w).Encode(map[string]any{
				"description":      repo.desc,
				"homepage":         repo.homepage,
				"stargazers_count": repo.stars,
				"license":          map[string]any{"spdx_id": repo.license},
			})
			return
		}
		if repo.noRelease {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assets := make([]map[string]any, 0, len(repo.assets))
		for _, a := range repo.assets {
			assets = append(assets, map[string]any{"name": a})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": repo.tag, "assets": assets})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runFakeGen(t *testing.T, repos map[string]fakeRepo, list []string, out string, update bool) error {
	t.Helper()
	srv := fakeAPI(t, repos)
	return gen(context.Background(), genConfig{
		repos: list, out: out, update: update,
		prov: newGitHub(srv.URL, ""),
		host: host.Host{Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}"},
	})
}

func loadOut(t *testing.T, path string) *index.Index {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不出生成的索引：%v", err)
	}
	idx, skipped, err := index.Load(data, "out")
	if err != nil {
		t.Fatalf("生成的索引自己 Load 不了：%v", err)
	}
	if skipped != 0 {
		t.Fatalf("生成的索引里有 %d 条不合法的记录", skipped)
	}
	return idx
}

var tinyserve = fakeRepo{
	desc: "一个很小的静态文件服务器", license: "MIT", stars: 42, tag: "v1.2.3",
	assets: []string{
		"tinyserve_1.2.3_linux_amd64.tar.gz",
		"tinyserve_1.2.3_linux_arm64.tar.gz",
		"tinyserve_1.2.3_darwin_arm64.tar.gz",
		"tinyserve_1.2.3_windows_amd64.zip",
		"checksums.txt",
	},
}

// 产出的必须是一条能被索引接受的内联条目，而且【绝不能有 [sha256]】——
// 内联条目跟着上游最新 tag 走，钉死某一版的字节会让上游一发新版就变成
// 「哈希对不上，拒绝安装」，读起来完全像投毒警告。
func TestGenProducesInlineEntryAndNeverWritesSHA256(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	if err := runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, false); err != nil {
		t.Fatalf("gen 失败：%v", err)
	}

	idx := loadOut(t, out)
	e, ok := idx.Find("Derrity/tinyserve")
	if !ok {
		t.Fatal("索引里没有这条")
	}
	if e.Source != "github:Derrity/tinyserve" {
		t.Errorf("source = %q", e.Source)
	}
	if e.Toyfile == "" {
		t.Fatal("条目没有内联 toyfile —— 那就产出了一条谁也装不上的空指针")
	}
	if e.Stars != 42 || e.Desc == "" {
		t.Errorf("stars/desc 没带上：%d %q", e.Stars, e.Desc)
	}
	if e.Key != "" || e.Sig != "" {
		t.Error("生成器不该凭空产出 key/sig —— 那个勾说的是「作者本人签的」")
	}

	// 只看非注释行：文件头那段注释里就写着「故意没有 [sha256]」，那句话本身不是内容。
	for _, line := range strings.Split(e.Toyfile, "\n") {
		if t2 := strings.TrimSpace(line); !strings.HasPrefix(t2, "#") && strings.Contains(t2, "sha256") {
			t.Fatalf("内联条目里写了 sha256：%q —— 上游一发新版，所有人都会拿到「哈希对不上」", line)
		}
	}
	tf, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("内联的 toyfile 解析不了：%v", err)
	}
	if len(tf.SHA256) != 0 {
		t.Fatal("解析出来居然有 sha256")
	}
	// Windows 不做（规格 §11），checksums.txt 不是资产。
	want := map[string]string{
		"linux/amd64":  "tinyserve_{version}_linux_amd64.tar.gz",
		"linux/arm64":  "tinyserve_{version}_linux_arm64.tar.gz",
		"darwin/arm64": "tinyserve_{version}_darwin_arm64.tar.gz",
	}
	if len(tf.Artifact) != len(want) {
		t.Fatalf("[artifact] = %v", tf.Artifact)
	}
	for k, v := range want {
		if tf.Artifact[k] != v {
			t.Errorf("[artifact] %s = %q，想要 %q", k, tf.Artifact[k], v)
		}
	}
	if tf.Bin["tinyserve"] != "tinyserve" {
		t.Errorf("[bin] = %v", tf.Bin)
	}
}

// 默认不覆盖已存在的条目：索引里的条目可能被人手工修正过（尤其 [bin] 右边）。
func TestGenDoesNotOverwriteWithoutUpdate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	seed.Upsert(index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "人工修正过的描述",
		Toyfile: "name = \"tinyserve\"\n\n[bin]\ntinyserve = \"nested/tinyserve\"\n\n" +
			"[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n",
	})
	data, _ := seed.Marshal()
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}

	repos := map[string]fakeRepo{"Derrity/tinyserve": tinyserve}
	if err := runFakeGen(t, repos, []string{"Derrity/tinyserve"}, out, false); err != nil {
		t.Fatalf("gen 失败：%v", err)
	}
	e, _ := loadOut(t, out).Find("Derrity/tinyserve")
	if !strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("没加 --update 却把人工修正过的 [bin] 冲掉了")
	}

	if err := runFakeGen(t, repos, []string{"Derrity/tinyserve"}, out, true); err != nil {
		t.Fatalf("gen --update 失败：%v", err)
	}
	e, _ = loadOut(t, out).Find("Derrity/tinyserve")
	if strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("--update 应该覆盖，实际没覆盖")
	}
}

// 签过名的条目连 --update 都不许动：重新生成必然换掉 toyfile 原文，
// 而签名签的就是那份原文 —— 覆盖等于把一个「✓ 已签名」悄悄弄没。
func TestGenNeverTouchesSignedEntries(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	// 故意也带上 toyfile：这样只有「签过名」这一条守卫能救它 ——
	// 否则「指针条目不降档」那条会顺手挡住，这个测试就测不到自己要测的东西。
	seed.Upsert(index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve",
		Key: "a3f9", Sig: "sig", Desc: "签过名的内联条目",
		Toyfile: "name = \"tinyserve\"\n\n[bin]\ntinyserve = \"nested/tinyserve\"\n\n" +
			"[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n",
	})
	data, _ := seed.Marshal()
	os.WriteFile(out, data, 0o644)

	// 全部被有意跳过不算失败（没有一条出错），所以这里退 0。
	if err := runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, true); err != nil {
		t.Fatalf("跳过不是失败：%v", err)
	}
	e, _ := loadOut(t, out).Find("Derrity/tinyserve")
	if e.Sig != "sig" || !strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("--update 把一条签过名的条目改掉了 —— 重新生成会让那个「✓」悄悄失效")
	}
}

// 指针条目（上游自己有 toy.toml）换成内联是 TrustHost → TrustIndexed 的降档，
// 不是刷新。--update 也不放行。
func TestGenNeverDowngradesPointerEntries(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	seed.Upsert(index.Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "指针条目"})
	data, _ := seed.Marshal()
	os.WriteFile(out, data, 0o644)

	runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, true)
	if e, _ := loadOut(t, out).Find("Derrity/tinyserve"); e.Toyfile != "" {
		t.Fatal("把一条指针条目降档成了内联条目")
	}
}

// 一个仓库失败不影响其它，而且每种失败要说不同的人话。
func TestGenIsolatesFailuresAndClassifiesThem(t *testing.T) {
	repos := map[string]fakeRepo{
		"Derrity/tinyserve": tinyserve,
		"nobody/gone":       {missing: true},
		"nobody/norelease":  {noRelease: true},
		"nobody/sourceonly": {tag: "v1.0.0"},
		"nobody/bare":       {tag: "v1.0.0", assets: []string{"jq-linux-amd64", "jq-macos-arm64"}},
		"nobody/limited":    {rateLimited: true},
	}
	list := []string{"Derrity/tinyserve", "nobody/gone", "nobody/norelease",
		"nobody/sourceonly", "nobody/bare", "nobody/limited"}

	srv := fakeAPI(t, repos)
	out := filepath.Join(t.TempDir(), "idx.json")
	cfg := genConfig{repos: list, out: out, prov: newGitHub(srv.URL, "")}
	if err := gen(context.Background(), cfg); err != nil {
		t.Fatalf("有一条成功时不该整体失败：%v", err)
	}

	// 裸二进制的仓库（jqlang/jq 就是这个形状）也要产出条目：install 支持单文件
	// 形态，把它整个拒掉等于让这类仓库永远进不了索引。
	idx := loadOut(t, out)
	if len(idx.Packages) != 2 {
		t.Fatalf("产出了 %d 条，该有 tinyserve 和 bare 两条", len(idx.Packages))
	}

	// 每种失败的措辞各自检查一遍：一句笼统的「没有可用资产」会让人挨个
	// 去仓库页面上看到底是哪种。
	for _, tc := range []struct{ id, want string }{
		{"nobody/gone", "仓库不存在"},
		{"nobody/norelease", "还没发布过 release"},
		{"nobody/sourceonly", "纯源码"},
		{"nobody/limited", "限流"},
	} {
		r := genOne(context.Background(), cfg, &index.Index{Version: index.Version}, tc.id)
		if r.err == nil {
			t.Errorf("%s 应该失败", tc.id)
			continue
		}
		if !strings.Contains(r.err.Error(), tc.want) {
			t.Errorf("%s 的报错是 %q，里面应该有 %q", tc.id, r.err, tc.want)
		}
	}

	// 裸二进制条目照出，[bin] 右边是【工具名】而不是资产名。
	//
	// 这条断言翻转过：原来钉的是「右边该写资产名本身」，而那正好是一个 bug ——
	// 资产名按平台变（yq_linux_amd64 / yq_darwin_arm64）而 [bin] 只有一行，
	// 于是除了恰好写对的那一个平台，其余每个平台都软链到一个不存在的文件，
	// 且在下载和校验都成功之后才炸。索引里 mikefarah/yq 就是这么在 6 个平台
	// 里坏了 5 个。现在 install 把单文件落成 [bin] 指定的路径
	// （cmd/toy/install.go 的 insUnpack），所以这一行对每个平台都成立。
	r := genOne(context.Background(), cfg, &index.Index{Version: index.Version}, "nobody/bare")
	if r.err != nil {
		t.Fatalf("裸二进制的仓库该产出条目：%v", r.err)
	}
	if got := r.pack.binPath; got != "jq" {
		t.Errorf("[bin] 右边 = %q，该是工具名 jq —— 写资产名的话别的平台全是断掉的软链", got)
	}
	if !r.pack.bare {
		t.Error("该被认出是裸二进制形态（决定 [bin] 上面那句注释怎么写）")
	}
}

// 没 token 撞限流时要说人话：提示设 GITHUB_TOKEN，并说明剩余配额。
func TestRateLimitTellsYouToSetAToken(t *testing.T) {
	srv := fakeAPI(t, map[string]fakeRepo{"a/b": {rateLimited: true}})
	c := newGitHub(srv.URL, "")
	_, err := c.Fetch(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("应该报限流")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "配额") {
		t.Errorf("限流报错没说该怎么办：%v", err)
	}
	if !strings.Contains(c.Quota(), "还剩 0") {
		t.Errorf("Quota() = %q", c.Quota())
	}
}

// 版本号必须变成 {version} 占位符 —— 钉死这一版的话，上游一发新版这条就指向
// 一个不存在的文件了（"打个 tag 就是发新版"是这个产品的卖点）。
// tag 里认得出版本、文件名里认不出时（2024.09 这类），拿 tag 兜底。
func TestVersionPlaceholderFallsBackToTheTag(t *testing.T) {
	p, err := makePack("a", "b", repoInfo{
		Tag:    "2024.09",
		Assets: []string{"b_2024.09_linux_amd64.tar.gz", "b_2024.09_darwin_arm64.tar.gz"},
	})
	if err != nil {
		t.Fatalf("makePack 失败：%v", err)
	}
	got := p.res.Artifact["linux/amd64"]
	if !strings.Contains(got, "{version}") {
		t.Fatalf("没换成占位符：%q", got)
	}
}

func TestReadRepoListStripsCommentsAndDupes(t *testing.T) {
	f := filepath.Join(t.TempDir(), "repos.txt")
	os.WriteFile(f, []byte("# 头注释\n\nDerrity/tinyserve\n cli/cli  # 二进制叫 gh\nderrity/TINYSERVE\n"), 0o644)
	got, err := readRepoList(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Derrity/tinyserve", "cli/cli"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v，want %v", got, want)
	}
}

// 客户端挑版本靠 `git ls-remote --tags` + semver（规格 §2）。tag 长成
// `jq-1.8.2` 的仓库它一个版本都解析不出来 —— 条目照样生成得出来，而
// `toy install` 第一步就死掉，报错里连"是因为 tag 的形状"都看不出来。
// 装不下去的条目不该进索引，这和 unstableBinPath 是同一类判断。
func TestGenRefusesReposWhoseTagsAreNotSemver(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	err := runFakeGen(t, map[string]fakeRepo{
		"jqlang/jq": {
			desc: "Command-line JSON processor", tag: "jq-1.8.2",
			assets: []string{"jq-linux-amd64", "jq-macos-arm64"},
		},
	}, []string{"jqlang/jq"}, out, false)
	if err == nil {
		t.Fatal("想要整批失败：唯一那条产不出条目")
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("不该写出索引文件 —— 一条条目都没产出")
	}
}

// 报告里那一列印的必须是【真正会落到 PATH 上】的名字。
//
// name 是包目录名、binName 是命令名，ripgrep 这种仓库上两者不一样
// （name=ripgrep、binName=rg）。这一列是 §14.9 第 2 条留给人的唯一关口 ——
// 印成 name 的话，人核对的是一个根本不会出现在 PATH 上的字符串。
func TestReportListsTheNameThatActuallyLandsOnPATH(t *testing.T) {
	if got := reportBinNames([]genResult{{
		written: true, id: "o/r",
		pack: pack{name: "ripgrep", binName: "rg"},
	}}); len(got) != 1 || !strings.HasPrefix(got[0], "rg\t") {
		t.Fatalf("报告里那一列 = %v，想要以 \"rg\" 打头", got)
	}
}
