package assetname

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// 语料是**真的**：`testdata/real-assets.txt` 是从 GitHub 上 9 个知名仓库的最新
// release 抓下来的资产名，每个仓库带它自己的 TAG=。
//
// 这个文件存在的理由是：我们自己编的测试数据（`tool_1.2.0_linux_amd64.tar.gz`，
// 版本号后面紧跟一个 `_`）**永远碰不到真实世界里最常见的那个形状** ——
// `ripgrep-15.2.0-aarch64-apple-darwin.tar.gz`，版本号后面跟的是 `-` 加一个
// 架构名。就是这一个形状让 4 个真仓库里 3 个的 [artifact] 指向了不存在的文件，
// 而全部单元测试是绿的。**加新规则之前先往语料里加一行真的资产名。**

type corpusRepo struct {
	Repo, Tag string
	Files     []string
}

func loadCorpus(t *testing.T) map[string]corpusRepo {
	t.Helper()
	data, err := os.ReadFile("testdata/real-assets.txt")
	if err != nil {
		t.Fatalf("读不了语料：%v", err)
	}
	out := map[string]corpusRepo{}
	cur := ""
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "### "):
			cur = strings.TrimSpace(line[4:])
			out[cur] = corpusRepo{Repo: cur}
		case strings.HasPrefix(line, "TAG="):
			r := out[cur]
			r.Tag = strings.TrimSpace(line[4:])
			out[cur] = r
		case strings.TrimSpace(line) != "":
			r := out[cur]
			r.Files = append(r.Files, strings.TrimSpace(line))
			out[cur] = r
		}
	}
	if len(out) == 0 {
		t.Fatal("语料是空的")
	}
	return out
}

func inferRepo(t *testing.T, repo string) Result {
	t.Helper()
	r, ok := loadCorpus(t)[repo]
	if !ok {
		t.Fatalf("语料里没有 %s", repo)
	}
	return InferTag(r.Files, r.Tag)
}

// 每一条都钉住整个模板，不只钉住"有没有 {version}"：这个 bug 的症状恰恰是
// 模板里有 {version}、看起来一切正常，只是中间少了一段架构名。
func TestCorpusArtifacts(t *testing.T) {
	cases := []struct {
		repo string
		want map[string]string
	}{{
		// Rust 的 target triple + 版本号后面紧跟 `-aarch64`：就是这一条把
		// `15.2.0-aarch64` 整个当成了版本号，替换完 aarch64 就没了。
		repo: "BurntSushi/ripgrep",
		want: map[string]string{
			"darwin/amd64":     "ripgrep-{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "ripgrep-{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/musl": "ripgrep-{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "ripgrep-{version}-aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm64/musl": "ripgrep-{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/armv7":      "ripgrep-{version}-armv7-unknown-linux-gnueabihf.tar.gz",
		},
	}, {
		// goreleaser 的 `-` 连 `_` 混排：丢的是 `linux` 那一段。
		repo: "junegunn/fzf",
		want: map[string]string{
			"linux/amd64":  "fzf-{version}-linux_amd64.tar.gz",
			"linux/arm64":  "fzf-{version}-linux_arm64.tar.gz",
			"linux/armv6":  "fzf-{version}-linux_armv6.tar.gz",
			"linux/armv7":  "fzf-{version}-linux_armv7.tar.gz",
			"darwin/amd64": "fzf-{version}-darwin_amd64.tar.gz",
			"darwin/arm64": "fzf-{version}-darwin_arm64.tar.gz",
		},
	}, {
		// `fd-v10.5.0-` 这种 `-v` 连写：v 必须留在模板里（{version} 不带 v），
		// 而且不能被当成工具名的一部分（见下面 TestCorpusNames）。
		repo: "sharkdp/fd",
		want: map[string]string{
			"darwin/amd64":     "fd-v{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "fd-v{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/gnu":  "fd-v{version}-x86_64-unknown-linux-gnu.tar.gz",
			"linux/amd64/musl": "fd-v{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "fd-v{version}-aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm64/musl": "fd-v{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/arm":        "fd-v{version}-arm-unknown-linux-gnueabihf.tar.gz",
			// i686 = 32 位 Linux。少了 archWords 里那一行，这两条会被静默丢掉。
			"linux/386/gnu":  "fd-v{version}-i686-unknown-linux-gnu.tar.gz",
			"linux/386/musl": "fd-v{version}-i686-unknown-linux-musl.tar.gz",
		},
	}, {
		// 资产名里**本来就没有版本号**（cargo-dist 那类，yq 也是这个形状）：
		// 不产出 {version} 是对的，模板跨版本天然稳定。
		repo: "eza-community/eza",
		want: map[string]string{
			"linux/amd64/gnu":  "eza_x86_64-unknown-linux-gnu.tar.gz",
			"linux/amd64/musl": "eza_x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "eza_aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm":        "eza_arm-unknown-linux-gnueabihf.tar.gz",
		},
	}, {
		// **Android 陷阱**：`aarch64-linux-android` / `armv7-linux-androideabi`
		// 里有 `linux` 这个词，先到先得就会被认成 linux/arm64、linux/armv7 ——
		// 而 armv7 那个 key 上本该是 armv7-unknown-linux-musleabihf，
		// 也就是说一个 bionic 构建**抢走了真正 Linux 构建的位置**。
		// 症状是下载地址 200、字节完好、跑起来 Exec format error。
		repo: "ajeetdsouza/zoxide",
		want: map[string]string{
			"darwin/amd64":     "zoxide-{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "zoxide-{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/musl": "zoxide-{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm":        "zoxide-{version}-arm-unknown-linux-musleabihf.tar.gz",
			"linux/arm64/musl": "zoxide-{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/armv7":      "zoxide-{version}-armv7-unknown-linux-musleabihf.tar.gz",
			"linux/386/musl":   "zoxide-{version}-i686-unknown-linux-musl.tar.gz",
		},
	}, {
		// **裸二进制**（没有压缩后缀）。install 支持单文件形态，所以这些要认。
		// 同一个 release 里的 `jq-1.8.2.tar.gz` 是**源码**，没有 os/arch 词，
		// 不会被认成资产 —— 把一个源码包装进 PATH 是灾难。
		repo: "jqlang/jq",
		want: map[string]string{
			"linux/amd64":  "jq-linux-amd64",
			"linux/arm64":  "jq-linux-arm64",
			"linux/armv7":  "jq-linux-armhf",
			"linux/386":    "jq-linux-i386",
			"darwin/amd64": "jq-macos-amd64", // macos 别名
			"darwin/arm64": "jq-macos-arm64",
		},
	}}

	for _, c := range cases {
		res := inferRepo(t, c.repo)
		for plat, want := range c.want {
			if got := res.Artifact[plat]; got != want {
				t.Errorf("%s [%s] = %q，想要 %q", c.repo, plat, got, want)
			}
		}
		for plat, got := range res.Artifact {
			if _, ok := c.want[plat]; !ok {
				t.Errorf("%s 多出一个平台 [%s] = %q", c.repo, plat, got)
			}
		}
	}
}

// 源码包混在同一个 release 里（jq-1.8.2.tar.gz / .zip）。它们没有 os/arch 词，
// 所以本来就进不了 [artifact] —— 这条断言是防止以后为了认裸二进制把它们一起放进来。
func TestCorpusNeverInstallsASourceTarball(t *testing.T) {
	res := inferRepo(t, "jqlang/jq")
	for plat, tmpl := range res.Artifact {
		if strings.HasPrefix(tmpl, "jq-1.8.2") {
			t.Errorf("[%s] 指向源码包 %q", plat, tmpl)
		}
	}
}

// 伴生文件（校验和、签名、.deb/.rpm、各家安装器、证明文件）和真资产同名同前缀，
// os/arch 词一个不少 —— 认平台之前不挑掉的话，它们会去和真资产抢同一个平台 key
// （.deb 常常排在 .tar.gz 前面，抢赢了就是一条装下来是个 deb 包的条目），
// 抢不赢也会各报一行把真正该看的那条淹掉。
func TestCorpusIgnoresCompanionFiles(t *testing.T) {
	noisy := []string{".sha256", ".deb", ".rpm", ".msi", ".pkg", "attestation.json", "checksums.txt", "sha256sum.txt"}
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		var seen []string
		for _, a := range res.Assets {
			seen = append(seen, a.File)
		}
		for _, s := range res.Skipped {
			seen = append(seen, "（报了一行）"+s.File)
		}
		for _, f := range seen {
			for _, n := range noisy {
				if strings.HasSuffix(strings.ToLower(strings.TrimPrefix(f, "（报了一行）")), n) {
					t.Errorf("%s：伴生文件 %s 不该出现在结果里", repo, f)
				}
			}
		}
	}
}

// 语料里的资产名带版本号时，模板里就不能再留着它 —— 留着就是一条钉死在这一版、
// 上游一发新版就 404 的条目。反过来，本来就不带版本号的（eza）不许被报成异常。
func TestCorpusLeavesNoVersionBehind(t *testing.T) {
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		if res.Unreplaced != "" {
			t.Errorf("%s 的模板里还留着版本号 %q", repo, res.Unreplaced)
		}
		for _, a := range res.Assets {
			if res.Version != "" && strings.Contains(a.Template, res.Version) {
				t.Errorf("%s：%q 里还留着 %q", repo, a.Template, res.Version)
			}
		}
	}
	// eza 的资产名里没有版本号，所以既不该有 {version}，也不该有任何抱怨。
	eza := inferRepo(t, "eza-community/eza")
	if eza.Unreplaced != "" {
		t.Errorf("eza 的资产名里本来就没版本号，不该报 %q", eza.Unreplaced)
	}
	for _, a := range eza.Assets {
		if strings.Contains(a.Template, "{version}") {
			t.Errorf("eza 不该产出 {version}：%q", a.Template)
		}
	}
}

// 工具名是 [bin] 左边那个命令名 —— 认成 `fd-v` 的话，装完 PATH 上多出来的
// 是一条叫 fd-v 的命令。
func TestCorpusNames(t *testing.T) {
	for repo, want := range map[string]string{
		"BurntSushi/ripgrep": "ripgrep",
		"junegunn/fzf":       "fzf",
		"sharkdp/fd":         "fd",
		"sharkdp/bat":        "bat",
		"cli/cli":            "gh",
		"jqlang/jq":          "jq",
	} {
		if got := inferRepo(t, repo).Name; got != want {
			t.Errorf("%s 的名字 = %q，想要 %q", repo, got, want)
		}
	}
}

// tag 是已知答案，正则只是猜 —— 已知答案的时候不该去猜。
//
// 这一条单靠语料测不出来（语料里正则收紧之后也能给出对的版本），所以用一个
// 正则**注定**猜错的形状：`-alpha` 是个合法的预发布标签写法，但这批资产的
// tag 是 v1.2.3，`alpha` 是文件名里的一段。正则会把 `1.2.3-alpha` 整个当成
// 版本号，于是模板里丢掉 `-alpha`，install 展开 {version} = 1.2.3 之后 404。
func TestKnownTagBeatsTheRegex(t *testing.T) {
	files := []string{
		"tool-1.2.3-alpha-x86_64-unknown-linux-gnu.tar.gz",
		"tool-1.2.3-alpha-aarch64-apple-darwin.tar.gz",
	}
	got := InferTag(files, "v1.2.3").Artifact["linux/amd64/gnu"]
	if want := "tool-{version}-alpha-x86_64-unknown-linux-gnu.tar.gz"; got != want {
		t.Errorf("模板 = %q，想要 %q", got, want)
	}
	if v := InferTag(files, "v1.2.3").Version; v != "1.2.3" {
		t.Errorf("版本 = %q，想要 1.2.3（tag 说了算）", v)
	}
}

// tag 只有在【作为一个完整的数字段】出现时才拿来做字面替换。
// tag `v2.0` 会在 `tool-2.0.1-…` 里命中，替换出来的
// `tool-{version}.1-…` 是一条谁都装不上的模板。
func TestKnownTagNeedsAWholeMatch(t *testing.T) {
	files := []string{"tool-2.0.1-x86_64-unknown-linux-gnu.tar.gz"}
	got := InferTag(files, "v2.0").Artifact["linux/amd64/gnu"]
	if want := "tool-{version}-x86_64-unknown-linux-gnu.tar.gz"; got != want {
		t.Errorf("模板 = %q，想要 %q（该由正则认出 2.0.1，不是拿 2.0 咬进去）", got, want)
	}
}

// 带包名前缀的 tag（jq-1.8.2）不能拿去做字面替换：install 那边 {version}
// 展开的是 `TrimPrefix(tag,"v")`，替换回来就少了 `jq-` 那一截。
func TestKnownTagRejectsAPackageNamePrefix(t *testing.T) {
	res := inferRepo(t, "jqlang/jq")
	for _, a := range res.Assets {
		if strings.Contains(a.Template, "{version}") {
			t.Errorf("jq-1.8.2 这种 tag 不该被拿来替换：%q", a.Template)
		}
	}
}

// 预发布标签和架构名长得一模一样，判据是白名单（preWords），不是"排除 os/arch 词"。
func TestTrimFalsePre(t *testing.T) {
	for in, want := range map[string]string{
		"15.2.0-aarch64": "15.2.0",
		"0.74.3-linux":   "0.74.3",
		"0.10.0-riscv64": "0.10.0",
		"15.2.0-s390x":   "15.2.0",
		"15.2.0-x86":     "15.2.0", // x86_64 被 '_' 切开之后剩下的
		"1.0.0-rc.1":     "1.0.0-rc.1",
		"1.0.0-rc1":      "1.0.0-rc1",
		"1.0.0-beta.2":   "1.0.0-beta.2",
		"1.0.0-alpha":    "1.0.0-alpha",
		"1.2.3+20240101": "1.2.3+20240101", // 构建元数据是 tag 的一部分，不能削
		"1.2.3":          "1.2.3",
	} {
		if got := trimFalsePre(in); got != want {
			t.Errorf("trimFalsePre(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 语料里每个仓库都要真的产出点东西 —— 一个"认出 0 个平台"的仓库等于收录不了。
func TestCorpusEveryRepoYieldsSomething(t *testing.T) {
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		if len(res.Artifact) == 0 {
			t.Errorf("%s 一个平台都没认出来", repo)
		}
		plats := make([]string, 0, len(res.Artifact))
		for p := range res.Artifact {
			plats = append(plats, p)
		}
		sort.Strings(plats)
		t.Logf("%s（%s）→ %s", repo, r.Tag, strings.Join(plats, " "))
	}
}

// Unreplaced 是那条「上游一发新版就 404」的警告的判据 —— 它必须真的会响。
// 场景：给的版本号和资产名里的对不上（--version 敲错，或者 tag 和资产名不一致）。
func TestUnreplacedFiresWhenAVersionIsLeftBehind(t *testing.T) {
	res := Infer([]string{"tool-1.2.3-x86_64-unknown-linux-gnu.tar.gz"}, "9.9.9")
	if res.Unreplaced != "1.2.3" {
		t.Fatalf("Unreplaced = %q，想要 1.2.3", res.Unreplaced)
	}
}
