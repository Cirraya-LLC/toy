package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 内置模板必须拼出真实存在的地址 —— 这四条串写错了没有任何测试以外的地方会说。
func TestBuiltinURLs(t *testing.T) {
	tests := []struct {
		host        string
		raw         string
		clone       string
		release     string
		web         string
		placeholder bool // 内置表里有这个形状，但 Lookup 会拒（见 TestLookupRefusesPlaceholder）
	}{
		{
			host:    "github",
			raw:     "https://raw.githubusercontent.com/Derrity/tinyserve/v1.2.3/toy.toml",
			clone:   "https://github.com/Derrity/tinyserve",
			release: "https://github.com/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://github.com/Derrity/tinyserve",
		},
		{
			host:    "gitlab",
			raw:     "https://gitlab.com/Derrity/tinyserve/-/raw/v1.2.3/toy.toml",
			clone:   "https://gitlab.com/Derrity/tinyserve",
			release: "https://gitlab.com/Derrity/tinyserve/-/releases/v1.2.3/downloads/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://gitlab.com/Derrity/tinyserve",
		},
		{
			host:    "codeberg",
			raw:     "https://codeberg.org/Derrity/tinyserve/raw/tag/v1.2.3/toy.toml",
			clone:   "https://codeberg.org/Derrity/tinyserve",
			release: "https://codeberg.org/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://codeberg.org/Derrity/tinyserve",
		},
		{
			host:        "gitea",
			raw:         "https://gitea.example.com/Derrity/tinyserve/raw/tag/v1.2.3/toy.toml",
			clone:       "https://gitea.example.com/Derrity/tinyserve",
			release:     "https://gitea.example.com/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:         "https://gitea.example.com/Derrity/tinyserve",
			placeholder: true,
		},
	}
	const asset = "tinyserve_1.2.3_linux_amd64.tar.gz"
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			h, ok := Builtin()[tt.host]
			if !ok {
				t.Fatalf("内置表里没有 %q", tt.host)
			}
			// 真托管方还必须能从 Lookup 原样拿到；形状那条由 Lookup 拒掉。
			if !tt.placeholder {
				got, err := Lookup(tt.host, nil)
				if err != nil {
					t.Fatalf("Lookup(%q) 报错: %v", tt.host, err)
				}
				if got != h {
					t.Errorf("Lookup 和 Builtin 对不上:\n%+v\n%+v", got, h)
				}
			}
			if h.Name != tt.host {
				t.Errorf("Name = %q, 想要 %q", h.Name, tt.host)
			}
			if got := h.RawURL("Derrity", "tinyserve", "v1.2.3", "toy.toml"); got != tt.raw {
				t.Errorf("RawURL = %q\n想要      %q", got, tt.raw)
			}
			if got := h.CloneURL("Derrity", "tinyserve"); got != tt.clone {
				t.Errorf("CloneURL = %q\n想要      %q", got, tt.clone)
			}
			if got := h.ReleaseURL("Derrity", "tinyserve", "v1.2.3", asset); got != tt.release {
				t.Errorf("ReleaseURL = %q\n想要        %q", got, tt.release)
			}
			if got := h.WebURL("Derrity", "tinyserve"); got != tt.web {
				t.Errorf("WebURL = %q\n想要    %q", got, tt.web)
			}
		})
	}
}

// 模板里不能留下没换掉的占位符，也不能有第二个包名混进来。
func TestNoLeftoverPlaceholders(t *testing.T) {
	for name, h := range Builtin() {
		urls := []string{
			h.RawURL("o", "r", "ref", "toy.toml"),
			h.CloneURL("o", "r"),
			h.ReleaseURL("o", "r", "v1", "a.tar.gz"),
			h.WebURL("o", "r"),
		}
		for _, u := range urls {
			if strings.Contains(u, "{") || strings.Contains(u, "}") {
				t.Errorf("%s: %q 里还有没换掉的占位符", name, u)
			}
		}
	}
}

// 调用方拿到的是副本，改它不能污染下一次 Builtin()。
func TestBuiltinIsNotShared(t *testing.T) {
	m := Builtin()
	m["github"] = Host{Raw: "https://evil.example/{path}"}
	delete(m, "gitlab")
	if got := Builtin()["github"].Raw; !strings.HasPrefix(got, "https://raw.githubusercontent.com/") {
		t.Errorf("内置表被污染了: %q", got)
	}
	if _, ok := Builtin()["gitlab"]; !ok {
		t.Error("内置表被污染了: gitlab 没了")
	}
}

func TestLookup(t *testing.T) {
	overrides := map[string]Host{
		"mygit":  {Raw: "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}"},
		"github": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
	}
	tests := []struct {
		name      string
		lookup    string
		overrides map[string]Host
		wantRaw   string // 空表示期望报错
		wantName  string
	}{
		{name: "内置", lookup: "github", wantRaw: "https://raw.githubusercontent.com/o/r/ref/p", wantName: "github"},
		{name: "大小写不敏感", lookup: "GitHub", wantRaw: "https://raw.githubusercontent.com/o/r/ref/p", wantName: "github"},
		{name: "覆盖内置", lookup: "github", overrides: overrides, wantRaw: "https://mirror.example.com/o/r/ref/p", wantName: "github"},
		{name: "自建", lookup: "mygit", overrides: overrides, wantRaw: "https://git.example.com/o/r/raw/tag/ref/p", wantName: "mygit"},
		{name: "不认识", lookup: "sourcehut"},
		{name: "空名字", lookup: ""},
		{name: "只有空格", lookup: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := Lookup(tt.lookup, tt.overrides)
			if tt.wantRaw == "" {
				if !errors.Is(err, ErrUnknownHost) {
					t.Fatalf("想要 ErrUnknownHost，拿到 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup(%q) 报错: %v", tt.lookup, err)
			}
			if got := h.RawURL("o", "r", "ref", "p"); got != tt.wantRaw {
				t.Errorf("RawURL = %q, 想要 %q", got, tt.wantRaw)
			}
			if h.Name != tt.wantName {
				t.Errorf("Name = %q, 想要 %q", h.Name, tt.wantName)
			}
		})
	}
}

// 报错要能行动：说出现在认识哪些，以及自建怎么加。
func TestUnknownHostErrorIsActionable(t *testing.T) {
	_, err := Lookup("sourcehut", map[string]Host{"mygit": {}})
	if err == nil {
		t.Fatal("想要报错")
	}
	for _, want := range []string{"sourcehut", "github", "mygit", "config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
}

// 没配的模板返回空串，调用方据此报「这家不支持」而不是拼出个半截 URL。
func TestEmptyTemplate(t *testing.T) {
	var h Host
	if got := h.RawURL("o", "r", "ref", "p"); got != "" {
		t.Errorf("RawURL = %q, 想要空串", got)
	}
	if got := h.CloneURL("o", "r"); got != "" {
		t.Errorf("CloneURL = %q, 想要空串", got)
	}
	if got := h.ReleaseURL("o", "r", "v1", "a"); got != "" {
		t.Errorf("ReleaseURL = %q, 想要空串", got)
	}
	if got := h.WebURL("o", "r"); got != "" {
		t.Errorf("WebURL = %q, 想要空串", got)
	}
}

func TestParseLsRemote(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "空输入", out: ""},
		{name: "只有空白", out: "\n  \n\t\n"},
		{
			name: "普通",
			out: "8f1c0a2\trefs/tags/v0.9.0\n" +
				"3b2d4e6\trefs/tags/v1.0.0\n" +
				"9a7c1f0\trefs/tags/v1.10.0\n",
			want: []string{"v0.9.0", "v1.0.0", "v1.10.0"},
		},
		{
			// annotated tag 的解引用行：--refs 通常已经滤掉了，滤不掉时也不能变成
			// "v1.0.0^{}" 这种解析不了的版本号，更不能跟 v1.0.0 重复。
			name: "annotated 解引用行",
			out: "3b2d4e6\trefs/tags/v1.0.0\n" +
				"aa11bb2\trefs/tags/v1.0.0^{}\n" +
				"cc33dd4\trefs/tags/v2.0.0^{}\n",
			want: []string{"v1.0.0", "v2.0.0"},
		},
		{
			name: "非 tag 的 ref 全部忽略",
			out: "3b2d4e6\tHEAD\n" +
				"3b2d4e6\trefs/heads/main\n" +
				"77ee88f\trefs/pull/12/head\n" +
				"3b2d4e6\trefs/tags/v1.0.0\n",
			want: []string{"v1.0.0"},
		},
		{
			name: "CRLF 和残缺行",
			out:  "3b2d4e6\trefs/tags/v1.0.0\r\n没有制表符的一行\n\n5c6d7e8\trefs/tags/v1.1.0\n",
			want: []string{"v1.0.0", "v1.1.0"},
		},
		{
			name: "非 semver 的 tag 也原样带回去（挑版本是 semver 包的事）",
			out:  "3b2d4e6\trefs/tags/release-2024\n5c6d7e8\trefs/tags/nightly\n",
			want: []string{"release-2024", "nightly"},
		},
		{
			name: "带斜杠的 tag 名",
			out:  "3b2d4e6\trefs/tags/cli/v1.0.0\n",
			want: []string{"cli/v1.0.0"},
		},
		{
			name: "只剩 ^{} 的空 tag 名被丢掉",
			out:  "3b2d4e6\trefs/tags/^{}\n5c6d7e8\trefs/tags/v1.0.0\n",
			want: []string{"v1.0.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLsRemote(tt.out)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseLsRemote = %#v, 想要 %#v", got, tt.want)
			}
		})
	}
}

func TestTagsEmptyCloneURL(t *testing.T) {
	// 「而不是去跑 git」这半句得真的断言出来：光看 err != nil 的话，把这道闸拆了
	// 测试照样绿 —— git 会代替我们对着一个空地址报一句它自己的错。
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	writeFakeGit(t, dir, "#!/bin/sh\n: > %s\necho \"deadbee\trefs/tags/v1.0.0\"\n", marker)
	t.Setenv("PATH", dir)

	for _, empty := range []string{"", "  ", "\t\n"} {
		if _, err := Tags(context.Background(), empty); err == nil {
			t.Fatalf("clone 地址是 %q 时应该报错", empty)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("clone 地址是 %q，却还是把 git 跑起来了", empty)
		}
	}
}

// stderr 是远端服务器能任意往里写字的地方，而我们只用得到第一行。
func TestCappedBufferDropsTheTail(t *testing.T) {
	var c cappedBuffer
	chunk := []byte(strings.Repeat("x", 4096))
	for range 256 { // 1 MiB
		n, err := c.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatalf("Write = (%d, %v)，必须谎报全写完，否则 git 会提前拿到 EPIPE", n, err)
		}
	}
	if got := len(c.String()); got != maxStderrBytes {
		t.Errorf("缓冲区留了 %d 字节，上限是 %d", got, maxStderrBytes)
	}
}

// git 不在 PATH 上时，错误要告诉用户装 git，而不是甩一句 exec: not found。
func TestTagsWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Tags(context.Background(), "https://example.com/o/r")
	if err == nil {
		t.Fatal("想要报错")
	}
	if !strings.Contains(err.Error(), "git") || !strings.Contains(err.Error(), "install") {
		t.Errorf("报错不可行动: %v", err)
	}
}

// 铁律级的一条：私有仓库会让 git 停在交互式密码提示上永不返回。
// 用一个假的 git 把它收到的环境变量回显成 tag 名，确认真的设上了。
func TestTagsDisablesTerminalPrompt(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"echo \"deadbee\trefs/tags/prompt-$GIT_TERMINAL_PROMPT\"\n" +
		"echo \"deadbee\trefs/tags/askpass-[$GIT_ASKPASS]\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	tags, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags 报错: %v", err)
	}
	want := []string{"prompt-0", "askpass-[]"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("环境变量没设上: %#v, 想要 %#v", tags, want)
	}
}

// git 自己失败时，把它的第一行 stderr 带给用户 —— 那句话通常就是原因。
func TestTagsReportsGitStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: repository not found' >&2\nexit 128\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	_, err := Tags(context.Background(), "https://example.com/o/nope")
	if err == nil {
		t.Fatal("想要报错")
	}
	if !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("报错里没带 git 的原话: %v", err)
	}
}

// context 到期要说是超时，而不是把 "signal: killed" 甩给用户。
func TestTagsTimeout(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// dir 排在最前面，所以假的 git 仍然赢；后面那截是给脚本里的 sleep 用的。
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Tags(ctx, "https://example.com/o/r")
	if err == nil {
		t.Fatal("想要报错")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("没说是超时: %v", err)
	}
}

// 真跑一次 git（本地仓库，不联网）：证明命令行拼对了、annotated tag 也能剥干净。
func TestTagsAgainstLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("这台机器上没有 git")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "one")
	run("tag", "v1.0.0")                          // 轻量 tag
	run("tag", "-a", "v1.2.0", "-m", "annotated") // annotated：会多一行 ^{}
	run("tag", "release-2024")                    // 非 semver，照样带回去

	tags, err := Tags(context.Background(), repo)
	if err != nil {
		t.Fatalf("Tags 报错: %v", err)
	}
	got := map[string]bool{}
	for _, tag := range tags {
		if strings.Contains(tag, "^") || strings.HasPrefix(tag, "refs/") {
			t.Errorf("tag 没剥干净: %q", tag)
		}
		got[tag] = true
	}
	for _, want := range []string{"v1.0.0", "v1.2.0", "release-2024"} {
		if !got[want] {
			t.Errorf("少了 tag %q，拿到的是 %v", want, tags)
		}
	}
	if len(tags) != 3 {
		t.Errorf("有重复或多余的 tag: %v", tags)
	}
}

// gitea 那条是「形状」不是托管方：直接用会打到一个 RFC 2606 的保留域名上，
// 报出来只是一句 DNS 错误 —— 用户根本看不出自己该去配什么。必须在这里拦住，
// 并且把可以直接抄的模板给他。
//
// 这里只断言「模板里有 host 名字和四个键」。模板长什么样（[hosts.x] 段落还是
// hosts.x = {…} 行内表）是排版，不是这条测试要钉的东西 —— 真正该钉的是
// 「抄回去能不能用」，那条在 hint_test.go 里，它把这一行喂回 TOML 解析器。
func TestLookupRefusesPlaceholder(t *testing.T) {
	_, err := Lookup("gitea", nil)
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("想要 ErrUnknownHost，拿到 %v", err)
	}
	for _, want := range []string{"config.toml", "hosts.gitea", "raw", "clone", "release", "web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
	// 拒绝掉的东西不能出现在别处报错的「现在认识的是」名单里。
	_, err = Lookup("sourcehut", nil)
	if err == nil {
		t.Fatal("想要报错")
	}
	if i := strings.Index(err.Error(), "Known hosts are"); i >= 0 {
		list, _, _ := strings.Cut(err.Error()[i:], ";")
		if strings.Contains(list, "gitea") {
			t.Errorf("已知列表里不该有被拒绝的 gitea: %q", list)
		}
	}
	// 但用户自己在 config.toml 里指到真域名之后，同一个名字必须能用。
	h, err := Lookup("gitea", map[string]Host{"gitea": giteaPaths("https://git.example.com")})
	if err != nil {
		t.Fatalf("配了自己的域名还被拒: %v", err)
	}
	if got, want := h.CloneURL("o", "r"), "https://git.example.com/o/r"; got != want {
		t.Errorf("CloneURL = %q, 想要 %q", got, want)
	}
}

// 一个 [hosts.github] 打错了键名（clone_url 而不是 clone）会变成一段空配置，
// 而它盖住内置的 github —— 从此 github 整个坏掉，而报错发生在三个包以外。
func TestLookupRejectsEmptyOverride(t *testing.T) {
	_, err := Lookup("github", map[string]Host{"github": {}})
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("想要 ErrUnknownHost，拿到 %v", err)
	}
	for _, want := range []string{"[hosts.github]", "raw", "clone", "release", "web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
}

// 内置和 overrides 撞名时，「现在认识的是」不能把同一个名字念两遍。
func TestKnownHostsListedOnce(t *testing.T) {
	_, err := Lookup("sourcehut", map[string]Host{
		"github": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
		"GitHub": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
	})
	if err == nil {
		t.Fatal("想要报错")
	}
	i := strings.Index(err.Error(), "Known hosts are")
	if i < 0 {
		t.Fatalf("报错里没有已知列表: %v", err)
	}
	list, _, _ := strings.Cut(err.Error()[i:], ";")
	if n := strings.Count(strings.ToLower(list), "github"); n != 1 {
		t.Errorf("github 在已知列表里出现了 %d 次: %q", n, list)
	}
}

// Name 会变成 ~/.toy/pkg/<host>/… 的目录名，大小写两种写法必须落到同一个目录，
// 否则同一个包会被装成两份。
func TestLookupNameIsCanonicalLowercase(t *testing.T) {
	overrides := map[string]Host{"MyGit": {Raw: "https://git.example.com/{owner}/{repo}/{ref}/{path}"}}
	h, err := Lookup("MyGit", overrides)
	if err != nil {
		t.Fatalf("Lookup 报错: %v", err)
	}
	if h.Name != "mygit" {
		t.Errorf("Name = %q, 想要 %q", h.Name, "mygit")
	}
}

// asset 是四个入参里唯一来自包作者的（toy.toml 的 [artifact] 值）。
// 它在四家托管方上都是一个扁平文件名，所以斜杠和 .. 一律转义掉：否则作者能把
// 下载指到同一域名下别人的 release，而安装那一行仍然如实写着「来源 github.com/他自己」
// —— 那就是铁律 8 说的静默撒谎。
func TestReleaseURLEscapesAsset(t *testing.T) {
	h, err := Lookup("github", nil)
	if err != nil {
		t.Fatal(err)
	}
	const base = "https://github.com/victim/repo/releases/download/v1.0.0/"
	tests := []struct{ asset, want string }{
		// 正常资产名一个字节都不能变
		{"toy_1.0.0_linux_amd64.tar.gz", base + "toy_1.0.0_linux_amd64.tar.gz"},
		{"../../../attacker/repo/releases/download/v9/evil", base + "..%2F..%2F..%2Fattacker%2Frepo%2Freleases%2Fdownload%2Fv9%2Fevil"},
		{"my tool.tar.gz", base + "my%20tool.tar.gz"},
		{"x?a=b", base + "x%3Fa=b"},
		{"x#frag", base + "x%23frag"},
	}
	for _, tt := range tests {
		got := h.ReleaseURL("victim", "repo", "v1.0.0", tt.asset)
		if got != tt.want {
			t.Errorf("ReleaseURL(asset=%q) = %q\n想要 %q", tt.asset, got, tt.want)
		}
		if !strings.HasPrefix(got, base) {
			t.Errorf("asset %q 逃出了 release 路径: %q", tt.asset, got)
		}
	}
}

// 铁律 1：任何时候都不执行别人的代码。`git ls-remote <url>` 里 url 以 '-' 开头时
// git 把它当成选项，--upload-pack=<命令> 就是一条现成的任意命令执行。
// 断言的是「git 压根没被拉起来」—— 所以这个假 git 一跑就会留下一个文件。
func TestTagsRefusesOptionLikeURL(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	// 只用 shell 内建：PATH 被换成了只有假 git 的目录，用 touch 会「因为找不到
	// touch 而失败」——那样这条测试就会因为错误的理由通过（第一版真踩了）。
	// 这个假 git 一旦被拉起来就既留下文件、又让 Tags 成功返回，两边都会响。
	writeFakeGit(t, dir, "#!/bin/sh\n: > %s\necho \"deadbee\trefs/tags/v1.0.0\"\n", marker)
	t.Setenv("PATH", dir)

	for _, bad := range []string{"--upload-pack=touch /tmp/pwned", "-u", "  --exec=x  "} {
		if _, err := Tags(context.Background(), bad); err == nil {
			t.Fatalf("%q 应该被拒绝", bad)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%q: git 真的被跑起来了", bad)
		}
	}
}

// 命令行拼错了没有别的地方会说：假 git 把自己的参数回显成 tag 名。
// 顺便钉住 `--` 分隔符 —— 少了它，上面那条拒绝就成了唯一一道防线。
func TestTagsCommandLine(t *testing.T) {
	dir := t.TempDir()
	writeFakeGit(t, dir, "#!/bin/sh\nfor a in \"$@\"; do echo \"deadbee\trefs/tags/$a\"; done\n")
	t.Setenv("PATH", dir)

	got, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags 报错: %v", err)
	}
	want := []string{"ls-remote", "--tags", "--refs", "--", "https://example.com/o/r"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git 的参数是 %#v，想要 %#v", got, want)
	}
}

// git 会读【当前目录所属仓库】的 .git/config，那里的 url.insteadOf 和
// credential.helper 都能让 git 转头去执行别的东西。在别人写得进去的目录里跑 toy
// 不该等于把铁律 1 交出去，所以 git 必须在一个中立目录里跑。
func TestTagsDoesNotRunInCurrentDirectory(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, "#!/bin/sh\necho \"deadbee\trefs/tags/cwd\"\necho \"deadbee\trefs/tags/$(pwd)\"\n")
	t.Setenv("PATH", bin)

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	tags, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags 报错: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("拿到 %#v", tags)
	}
	if strings.HasPrefix(tags[1], work) {
		t.Errorf("git 跑在当前目录 %q 里了（它会读那里的 .git/config）", work)
	}
}

// WaitDelay 那一行很像可以顺手删掉的东西，所以这里钉住它保护的场景：
// git 拉起的帮手进程活得比 git 久、还攥着 stderr 管道，杀掉 git 本身并不能让
// Output() 返回。没有 WaitDelay，这个调用会挂到那个孙子进程自己退出为止。
func TestTagsSurvivesOrphanHoldingPipes(t *testing.T) {
	dir := t.TempDir()
	// `sleep 30 &` 继承了 stdout/stderr 管道；ctx 到期只杀得掉前台这个 sh。
	writeFakeGit(t, dir, "#!/bin/sh\nsleep 30 &\nsleep 20\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Tags(ctx, "https://example.com/o/r")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("想要超时报错，拿到 %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Tags 被一个攥着管道的孙子进程挂住了 —— WaitDelay 没起作用")
	}
}

// `remote: …` 那些行是远端服务器写的，会一路进到报错里印在用户终端上。
// 一个能往别人终端上任意写字（改颜色、用 \r 盖掉前面那句话）的错误信息不叫错误信息。
func TestTagsSanitizesGitStderr(t *testing.T) {
	dir := t.TempDir()
	writeFakeGit(t, dir,
		"#!/bin/sh\nprintf 'remote: \\033[31mfatal\\033[0m\\rHACKED %s\\n' >&2\nexit 128\n",
		strings.Repeat("A", 2000))
	t.Setenv("PATH", dir)

	_, err := Tags(context.Background(), "https://example.com/o/r")
	if err == nil {
		t.Fatal("想要报错")
	}
	msg := err.Error()
	for _, bad := range []string{"\x1b", "\r", "\a", "\b"} {
		if strings.Contains(msg, bad) {
			t.Errorf("报错里还有控制字符 %q: %q", bad, msg)
		}
	}
	if len(msg) > 2*maxStderrLine {
		t.Errorf("报错没封顶，长度 %d: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "fatal") {
		t.Errorf("把有用的字也一起剥掉了: %q", msg)
	}
}

func writeFakeGit(t *testing.T, dir, script string, args ...any) {
	t.Helper()
	if len(args) > 0 {
		script = fmt.Sprintf(script, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Ctrl-C 打在 ls-remote 上时，错误必须还带得住 context.Canceled ——
// runUpdate 的 `case errors.Is(err, context.Canceled): return err` 全靠它，
// 吃掉了就会在用户按下 Ctrl-C 之后接着把剩下每个包都跑一遍。
// 顺带钉住超时那条：它仍然要说"超时"、并且能被 errors.Is 认成 DeadlineExceeded。
func TestTagsPropagatesContextError(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Tags(canceled, "https://example.com/o/r"); !errors.Is(err, context.Canceled) {
		t.Errorf("取消后的错误认不出 context.Canceled，上层会把 Ctrl-C 当普通失败继续跑：%v", err)
	}

	timeout, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	_, err := Tags(timeout, "https://example.com/o/r")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("超时的错误认不出 context.DeadlineExceeded：%v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("超时了却没说是超时：%v", err)
	}
}
