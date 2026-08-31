package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// 文件名反推是这条命令的全部内容。认错一个词的下场是别人装的时候
// 一句「没有适配当前平台的构建」—— 而作者永远不会知道是哪一行错了。
func TestIniDetect(t *testing.T) {
	cases := []struct{ file, key, goos string }{
		{"gadget_1.4.2_linux_amd64.tar.gz", "linux/amd64", "linux"},
		// x86_64 会被 '_' 切成两段，靠相邻二元组捞回来。goreleaser 的默认命名就是它。
		{"gadget_1.4.2_linux_x86_64.tar.gz", "linux/amd64", "linux"},
		{"gadget-1.4.2-linux-x64.tar.gz", "linux/amd64", "linux"},
		{"gadget_1.4.2_darwin_arm64.zip", "darwin/arm64", "darwin"},
		{"gadget_1.4.2_macos_aarch64.tar.gz", "darwin/arm64", "darwin"},
		{"gadget-osx-x86_64.tar.gz", "darwin/amd64", "darwin"},
		{"gadget-1.0.0-apple-arm64.tar.gz", "darwin/arm64", "darwin"},
		// musl / gnu 变成第三段，正是 platform.Candidates 头一个要找的那个 key。
		{"gadget_1.4.2_linux_amd64_musl.tar.gz", "linux/amd64/musl", "linux"},
		{"gadget_1.4.2_linux_x86_64_gnu.tar.gz", "linux/amd64/gnu", "linux"},
		// arm 代次本身就够具体了，再挂 libc 没有意义。
		{"gadget_1.4.2_linux_armv7.tar.gz", "linux/armv7", "linux"},
		{"gadget_1.4.2_linux_armhf.tar.gz", "linux/armv7", "linux"},
		{"gadget_1.4.2_linux_armv6.tar.gz", "linux/armv6", "linux"},
		{"gadget_1.4.2_linux_i386.tar.gz", "linux/386", "linux"},
		// 规格 §11：Windows 认得出来，但不能进 [artifact]。key 空、goos 非空
		// 就是「认出来了但用不了」，调用方靠这个区分它和一个完全没认出来的文件。
		{"gadget_1.4.2_windows_amd64.zip", "", "windows"},
		{"gadget_1.4.2_win_x86_64.zip", "", "windows"},
		// 认不出来的一律跳过，不许瞎猜。
		{"checksums.txt", "", ""},
		{"artifacts.json", "", ""},
		{"gadget_1.4.2_linux.tar.gz", "", "linux"}, // 只有 os 没有 arch
		{"gadget_amd64.tar.gz", "", ""},            // 只有 arch 没有 os
	}
	for _, c := range cases {
		key, goos := iniDetect(c.file)
		if key != c.key || goos != c.goos {
			t.Errorf("iniDetect(%q) = (%q,%q)，想要 (%q,%q)", c.file, key, goos, c.key, c.goos)
		}
	}
}

// 版本号不换成 {version}，作者每次发版都要回来改这个文件 ——
// 而「打个 tag 就是发新版」是这个产品的卖点。
func TestIniScanReplacesTheVersion(t *testing.T) {
	files := []string{
		"gadget_1.4.2_linux_amd64.tar.gz",
		"gadget_1.4.2_darwin_arm64.tar.gz",
		"checksums.txt",
	}
	assets, _ := iniScan(files, "")
	if len(assets) != 2 {
		t.Fatalf("认出 %d 个，想要 2：%+v", len(assets), assets)
	}
	for _, a := range assets {
		if strings.Contains(a.tmpl, "1.4.2") {
			t.Errorf("%q 里还留着写死的版本号：%q", a.file, a.tmpl)
		}
		if !strings.Contains(a.tmpl, "{version}") {
			t.Errorf("%q 没有 {version} 占位符：%q", a.file, a.tmpl)
		}
	}
}

// 资产名里带 v 前缀时，{version} 不带 v（规格 §3），所以只该替换裸版本号，
// 让 v 留在模板里 —— 换掉 "v1.4.2" 会生成一个每次都少个 v 的地址。
func TestIniScanKeepsTheVPrefixInTheTemplate(t *testing.T) {
	assets, _ := iniScan([]string{"gadget_v1.4.2_linux_amd64.tar.gz"}, "")
	if len(assets) != 1 {
		t.Fatalf("认出 %d 个，想要 1", len(assets))
	}
	if got, want := assets[0].tmpl, "gadget_v{version}_linux_amd64.tar.gz"; got != want {
		t.Fatalf("模板 = %q，想要 %q", got, want)
	}
}

// --version 说了算：文件名里认不出三段式版本（1.4 这种）时它是唯一的出路。
func TestIniScanHonoursTheGivenVersion(t *testing.T) {
	assets, _ := iniScan([]string{"gadget-2024.09-linux-amd64.tar.gz"}, "2024.09")
	if got, want := assets[0].tmpl, "gadget-{version}-linux-amd64.tar.gz"; got != want {
		t.Fatalf("模板 = %q，想要 %q", got, want)
	}
}

// 同一个平台发了 .tar.gz 和 .zip 两份很常见。静默丢一个会让作者以为
// 自己发的那个格式生效了，所以必须说出来。
func TestIniScanReportsDuplicatePlatforms(t *testing.T) {
	assets, skipped := iniScan([]string{
		"gadget_1.0.0_linux_amd64.tar.gz",
		"gadget_1.0.0_linux_amd64.zip",
	}, "")
	if len(assets) != 1 {
		t.Fatalf("同一个平台应该只留一行，得到 %d", len(assets))
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "linux/amd64") {
		t.Fatalf("重复的那个要说明，得到 %v", skipped)
	}
}

// 规格 §11：不做 Windows。但静默跳过等于让作者以为 Windows 用户装得上。
func TestIniScanSaysWhyWindowsIsSkipped(t *testing.T) {
	assets, skipped := iniScan([]string{"gadget_1.0.0_windows_amd64.zip"}, "")
	if len(assets) != 0 {
		t.Fatalf("Windows 不能进 [artifact]，得到 %+v", assets)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Windows") {
		t.Fatalf("要说明是因为 Windows，得到 %v", skipped)
	}
}

func TestIniCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"gadget_1.4.2_linux_amd64.tar.gz", "gadget_1.4.2_darwin_arm64.tar.gz"}, "gadget"},
		{[]string{"my-tool-1.0.0-linux-amd64.tar.gz", "my-tool-1.0.0-darwin-arm64.tar.gz"}, "my-tool"},
		{[]string{"solo_1.0.0_linux_amd64.tar.gz"}, "solo"},
		{[]string{"a_1.0.0_linux_amd64.tar.gz", "b_1.0.0_linux_arm64.tar.gz"}, ""},
	}
	for _, c := range cases {
		assets := make([]iniAsset, len(c.in))
		for i, f := range c.in {
			assets[i] = iniAsset{file: f}
		}
		if got := iniCommonPrefix(assets); got != c.want {
			t.Errorf("iniCommonPrefix(%v) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// 生成出来的东西必须能被真正的解析器吃下去 —— 否则这条命令交付的是
// 一个"看起来生成好了"的谜题。这条也顺带钉住 [sha256] 的 key 和 [artifact] 一致
// （toyfile.Parse 对不上就拒）。
func TestIniRenderProducesAParsableToyfile(t *testing.T) {
	assets := []iniAsset{
		{key: "linux/amd64", tmpl: "gadget_{version}_linux_amd64.tar.gz", sum: strings.Repeat("a", 64)},
		{key: "darwin/arm64", tmpl: "gadget_{version}_darwin_arm64.tar.gz", sum: strings.Repeat("b", 64)},
	}
	data, err := iniRender("gadget", assets)
	if err != nil {
		t.Fatalf("iniRender：%v", err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		t.Fatalf("生成出来的 toy.toml 自己解析不了：%v\n%s", err, data)
	}
	if tf.Name != "gadget" || tf.Bin["gadget"] != "gadget" {
		t.Fatalf("name/bin 不对：%+v", tf)
	}
	// 铁律 4：文件就在本地，算得出来就必须写进去 —— 少这一步整包只有 TOFU。
	if tf.SHA256["linux/amd64"] == "" || tf.SHA256["darwin/arm64"] == "" {
		t.Fatalf("[sha256] 没写全：%+v", tf.SHA256)
	}
	// 平台 key 带斜杠，裸键写不了。
	if !strings.Contains(string(data), `"linux/amd64" = `) {
		t.Fatalf("平台 key 必须加引号：\n%s", data)
	}
}

// 整条命令走一遍：造 dist → init → 文件真的落地且能解析。
func TestRunInitEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"gadget_3.0.1_linux_x86_64.tar.gz",
		"gadget_3.0.1_darwin_arm64.tar.gz",
		"gadget_3.0.1_windows_x86_64.zip",
		"checksums.txt",
	} {
		if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(t.Context(), []string{"--name", "gadget", dir}); err != nil {
		t.Fatalf("runInit：%v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		t.Fatalf("写出来的 toy.toml 解析不了：%v\n%s", err, data)
	}
	if len(tf.Artifact) != 2 {
		t.Fatalf("想要 2 个平台（Windows 不算），得到 %v", tf.Artifact)
	}
	if strings.Contains(string(data), "3.0.1") {
		t.Fatalf("版本号没被换成 {version}：\n%s", data)
	}
	// 已经存在就不许覆盖：那可能是作者手改过的文件。
	if err := runInit(t.Context(), []string{"--name", "gadget", dir}); err == nil {
		t.Fatal("第二次 init 应该拒绝覆盖")
	}
	if err := runInit(t.Context(), []string{"--force", "--name", "gadget", dir}); err != nil {
		t.Fatalf("--force 应该允许覆盖：%v", err)
	}
}

// 一个都认不出来时绝不能写一个空壳 —— 那是个"看起来成了、谁也装不了"的文件。
func TestRunInitRefusesToWriteAnEmptyShell(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runInit(t.Context(), []string{dir})
	if err == nil {
		t.Fatal("认不出任何平台时应该报错")
	}
	if !strings.Contains(err.Error(), "README") {
		t.Errorf("报错要说它看到了什么，得到：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "toy.toml")); statErr == nil {
		t.Fatal("不该写出 toy.toml")
	}
}
