package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// 文件名反推本身在 internal/assetname 里，它的单测也在那儿（Classify / Infer 两组）。
// 这个文件只测 cmd 这一层：反推结果怎么变成一份能解析的 toy.toml、怎么落地、
// 以及那两条【必须走完整条命令】才有意义的回归。
//
// 别把 assetname 的表驱动用例抄回这里 —— 抄回来就是第二份会漂移的实现的第二份
// 会漂移的测试，而这个包被抽出来正是为了消灭第一份。

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

// goreleaser 在 GOARCH=arm 且没设 goarm 时，默认 name_template 产出的就是
// `tool_1.2.3_linux_arm.tar.gz`。裸 "arm" 一度不在映射表里，于是这份构建被
// 【静默丢掉】：init 打印「认出了 N 个平台」，看起来一切正常，而树莓派用户装的
// 时候拿到的是「没有适配当前平台的构建」。
//
// linux/arm 不是个可有可无的 key —— platform.go 就是拿它给 armv6/armv7 兜底的
// （规格 §3.1）。走完整条命令而不是只调推断函数：漏掉的那一环可以是 cmd 这一层
// 把结果转成 [artifact] 时丢的，而那种丢法在推断的单测里看不见。
func TestRunInitKeepsBareArm(t *testing.T) {
	tf := iniRunOnDist(t, []string{
		"tool_1.2.3_linux_arm.tar.gz",
		"tool_1.2.3_linux_armv6.tar.gz",
		"tool_1.2.3_linux_armv7.tar.gz",
		"tool_1.2.3_linux_amd64.tar.gz",
	})
	for _, want := range []string{"linux/arm", "linux/armv6", "linux/armv7", "linux/amd64"} {
		if tf.Artifact[want] == "" {
			t.Errorf("[artifact] 里少了 %s：%v", want, tf.Artifact)
		}
	}
}

// 认不出来的【资产】必须出声。静默丢掉的症状和上面那条一模一样：数字看着对，
// 少的那个平台要等用户来报。但 checksums.txt / LICENSE 这些本来就不是资产，
// 为它们报警会把真的那条淹掉。
func TestRunInitReportsUnrecognizedAssetsButNotStrayFiles(t *testing.T) {
	files := []string{
		"tool_1.2.3_linux_amd64.tar.gz",
		"tool_1.2.3_freebsd_riscv64.tar.gz", // 像资产，认不出 → 要出声
		"checksums.txt",                     // 不是资产 → 不许出声
		"LICENSE",
		"README.md",
	}
	var lines []string
	for _, a := range assetnameSkipped(files) {
		lines = append(lines, a.File+"："+a.Skip)
	}
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "freebsd_riscv64") != 1 {
		t.Errorf("认不出的资产应该被报一次：%q", joined)
	}
	for _, quiet := range []string{"checksums.txt", "LICENSE", "README.md"} {
		if strings.Contains(joined, quiet) {
			t.Errorf("不该为非资产 %s 出声：%q", quiet, joined)
		}
	}
	// 出声归出声，能认的那个平台还是要照常进 [artifact]。
	if tf := iniRunOnDist(t, files); len(tf.Artifact) != 1 {
		t.Errorf("想要 1 个平台，得到 %v", tf.Artifact)
	}
}

// iniRunOnDist 造一个 dist、跑一遍 runInit、把写出来的 toy.toml 解析回来。
func iniRunOnDist(t *testing.T, files []string) *toyfile.Toyfile {
	t.Helper()
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(t.Context(), []string{"--name", "tool", dir}); err != nil {
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
	return tf
}

// assetnameSkipped 拿到 cmd 这一层真正会打印的那份跳过清单。
func assetnameSkipped(files []string) []assetname.Asset {
	return assetname.Infer(files, "").Skipped
}
