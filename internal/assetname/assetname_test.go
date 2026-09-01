package assetname

import (
	"strings"
	"testing"
)

// 文件名反推是这个包的全部内容。认错一个词的下场是别人装的时候
// 一句「没有适配当前平台的构建」—— 而作者永远不会知道是哪一行错了。
func TestClassify(t *testing.T) {
	cases := []struct {
		file, platform, goos string
	}{
		// —— goreleaser 默认命名 ——
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
		// 裸 arm：GOARCH=arm 且没设 goarm 时 goreleaser 产出的就是它，而
		// linux/arm 正是 platform.go 给 armv6/armv7 兜底的候选（规格 §3.1）。
		{"tool_1.2.3_linux_arm.tar.gz", "linux/arm", "linux"},
		{"tool-1.2.3-linux-arm.tar.gz", "linux/arm", "linux"},

		// —— Rust 的 target triple（我们要收录的工具很多是 Rust 写的）——
		// unknown / pc 这些多余的段本来就不认，所以不用特判。
		{"tool-1.0.0-x86_64-unknown-linux-gnu.tar.gz", "linux/amd64/gnu", "linux"},
		{"tool-1.0.0-x86_64-unknown-linux-musl.tar.gz", "linux/amd64/musl", "linux"},
		{"tool-1.0.0-aarch64-unknown-linux-gnu.tar.gz", "linux/arm64/gnu", "linux"},
		{"tool-1.0.0-aarch64-apple-darwin.tar.gz", "darwin/arm64", "darwin"},
		{"tool-1.0.0-x86_64-apple-darwin.tar.gz", "darwin/amd64", "darwin"},
		{"tool-1.0.0-armv7-unknown-linux-gnueabihf.tar.gz", "linux/armv7", "linux"},
		{"tool-1.0.0-i686-pc-windows-msvc.zip", "", "windows"},

		// —— cargo-dist：资产名里常常没有版本号 ——
		{"tool-x86_64-unknown-linux-gnu.tar.xz", "linux/amd64/gnu", "linux"},
		{"tool-aarch64-apple-darwin.tar.xz", "darwin/arm64", "darwin"},

		// 规格 §11：Windows 认得出来，但不能进 [artifact]。
		{"gadget_1.4.2_windows_amd64.zip", "", "windows"},
		{"gadget_1.4.2_win_x86_64.zip", "", "windows"},
		// 认不出来的一律跳过，不许瞎猜。
		{"checksums.txt", "", ""},
		{"artifacts.json", "", ""},
		{"gadget_1.4.2_linux.tar.gz", "", "linux"}, // 只有 os 没有 arch
		{"gadget_amd64.tar.gz", "", ""},            // 只有 arch 没有 os
	}
	for _, c := range cases {
		key, goos := detect(c.file)
		if key != c.platform || goos != c.goos {
			t.Errorf("detect(%q) = (%q,%q)，想要 (%q,%q)", c.file, key, goos, c.platform, c.goos)
		}
		if got := Classify(c.file); got.Platform != c.platform {
			t.Errorf("Classify(%q).Platform = %q，想要 %q", c.file, got.Platform, c.platform)
		}
	}
}

// Skip 是给人看的，而「值不值得说」和「跳没跳过」是两件事：
// Windows 和认不出的资产要出声，checksums.txt 这种不许出声（否则把真的那条淹掉）。
func TestClassifySkipReasons(t *testing.T) {
	cases := []struct {
		file    string
		skip    string
		notable bool
	}{
		{"gadget_1.0.0_linux_amd64.tar.gz", "", false},
		{"gadget_1.0.0_windows_amd64.zip", SkipWindows, true},
		// freebsd 认得出来，只是 toy 不支持它 —— 那和"认不出是哪个平台的"
		// 不是一回事：后者那句话是在请作者补一行 [artifact]，而补了也装不上。
		{"tool_1.2.3_freebsd_riscv64.tar.gz", SkipForeignOS("freebsd"), true},
		{"tool-1.2.3-x86_64-unknown-illumos.tar.gz", SkipForeignOS("illumos"), true},
		{"tool_1.2.3_dragonfly_amd64.tar.gz", SkipUnrecognized, true}, // 没进表的还是"认不出"
		{"tool_1.2.3_solaris_amd64.tar.gz", SkipForeignOS("solaris"), true},
		{"checksums.txt", SkipNotAnAsset, false},
		{"LICENSE", SkipNotAnAsset, false},
		{"README.md", SkipNotAnAsset, false},
	}
	for _, c := range cases {
		got := Classify(c.file)
		if got.Skip != c.skip || got.Notable != c.notable {
			t.Errorf("Classify(%q) = (skip %q, notable %v)，想要 (%q, %v)",
				c.file, got.Skip, got.Notable, c.skip, c.notable)
		}
	}
}

func TestClassifyVersion(t *testing.T) {
	cases := map[string]string{
		"gadget_1.4.2_linux_amd64.tar.gz":      "1.4.2",
		"gadget_v1.4.2_linux_amd64.tar.gz":     "1.4.2", // v 不算版本串的一部分
		"gadget-1.0.0-rc.1-linux-amd64.tar.gz": "1.0.0-rc.1",
		"tool-x86_64-unknown-linux-gnu.tar.xz": "", // cargo-dist 常常不带版本
		"gadget-2024.09-linux-amd64.tar.gz":    "", // 两段式不认，认了会换错东西
	}
	for f, want := range cases {
		if got := Classify(f).Version; got != want {
			t.Errorf("Classify(%q).Version = %q，想要 %q", f, got, want)
		}
	}
}

// 版本号不换成 {version}，作者每次发版都要回来改这个文件 ——
// 而「打个 tag 就是发新版」是这个产品的卖点。
func TestInferReplacesTheVersion(t *testing.T) {
	res := Infer([]string{
		"gadget_1.4.2_linux_amd64.tar.gz",
		"gadget_1.4.2_darwin_arm64.tar.gz",
		"checksums.txt",
	}, "")
	if len(res.Assets) != 2 || len(res.Artifact) != 2 {
		t.Fatalf("认出 %d 个，想要 2：%+v", len(res.Assets), res.Assets)
	}
	if res.Version != "1.4.2" {
		t.Fatalf("版本 = %q，想要 1.4.2", res.Version)
	}
	for _, a := range res.Assets {
		if strings.Contains(a.Template, "1.4.2") {
			t.Errorf("%q 里还留着写死的版本号：%q", a.File, a.Template)
		}
		if !strings.Contains(a.Template, "{version}") {
			t.Errorf("%q 没有 {version} 占位符：%q", a.File, a.Template)
		}
		if res.Artifact[a.Platform] != a.Template {
			t.Errorf("Artifact[%q] = %q，和 Assets 对不上", a.Platform, res.Artifact[a.Platform])
		}
	}
}

// 资产名里带 v 前缀时，{version} 不带 v（规格 §3），所以只该替换裸版本号，
// 让 v 留在模板里 —— 换掉 "v1.4.2" 会生成一个每次都少个 v 的地址。
func TestInferKeepsTheVPrefixInTheTemplate(t *testing.T) {
	res := Infer([]string{"gadget_v1.4.2_linux_amd64.tar.gz"}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("认出 %d 个，想要 1", len(res.Assets))
	}
	if got, want := res.Assets[0].Template, "gadget_v{version}_linux_amd64.tar.gz"; got != want {
		t.Fatalf("模板 = %q，想要 %q", got, want)
	}
}

// forceVersion 说了算：文件名里认不出三段式版本（2024.09 这种）时它是唯一的出路。
// 前面的 v 要被削掉，不然替换永远匹配不上。
func TestInferHonoursTheGivenVersion(t *testing.T) {
	for _, given := range []string{"2024.09", "v2024.09", " v2024.09 "} {
		res := Infer([]string{"gadget-2024.09-linux-amd64.tar.gz"}, given)
		if got, want := res.Assets[0].Template, "gadget-{version}-linux-amd64.tar.gz"; got != want {
			t.Fatalf("--version %q：模板 = %q，想要 %q", given, got, want)
		}
	}
}

// 同一个平台发了 .tar.gz 和 .zip 两份很常见。静默丢一个会让作者以为
// 自己发的那个格式生效了，所以必须说出来。
func TestInferReportsDuplicatePlatforms(t *testing.T) {
	res := Infer([]string{
		"gadget_1.0.0_linux_amd64.tar.gz",
		"gadget_1.0.0_linux_amd64.zip",
	}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("同一个平台应该只留一行，得到 %d", len(res.Assets))
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Skip, "linux/amd64") {
		t.Fatalf("重复的那个要说明，得到 %+v", res.Skipped)
	}
	if res.Skipped[0].File != "gadget_1.0.0_linux_amd64.zip" {
		t.Fatalf("被丢掉的应该是排后面那个，得到 %q", res.Skipped[0].File)
	}
}

// 规格 §11：不做 Windows。但静默跳过等于让作者以为 Windows 用户装得上。
func TestInferSaysWhyWindowsIsSkipped(t *testing.T) {
	res := Infer([]string{"gadget_1.0.0_windows_amd64.zip"}, "")
	if len(res.Assets) != 0 {
		t.Fatalf("Windows 不能进 [artifact]，得到 %+v", res.Assets)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Skip, "Windows") {
		t.Fatalf("要说明是因为 Windows，得到 %+v", res.Skipped)
	}
}

// 认不出来的【资产】必须出声。静默丢掉的症状很恶心：数字看着对，
// 少的那个平台要等用户来报。但 checksums.txt / LICENSE 这些本来就不是资产，
// 为它们报警会把真的那条淹掉。
func TestInferReportsUnrecognizedAssetsButNotStrayFiles(t *testing.T) {
	res := Infer([]string{
		"tool_1.2.3_linux_amd64.tar.gz",
		"tool_1.2.3_freebsd_riscv64.tar.gz", // 像资产，认不出 → 要出声
		"checksums.txt",                     // 不是资产 → 不许出声
		"LICENSE",
		"README.md",
	}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("认出的资产数 = %d，想要 1：%+v", len(res.Assets), res.Assets)
	}
	var mentions int
	for _, s := range res.Skipped {
		if s.File == "tool_1.2.3_freebsd_riscv64.tar.gz" {
			mentions++
		}
		for _, quiet := range []string{"checksums.txt", "LICENSE", "README.md"} {
			if s.File == quiet {
				t.Errorf("不该为非资产 %s 出声：%+v", quiet, s)
			}
		}
	}
	if mentions != 1 {
		t.Errorf("认不出的资产应该被报一次，实际 %d 次：%+v", mentions, res.Skipped)
	}
}

func TestInferName(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"gadget_1.4.2_linux_amd64.tar.gz", "gadget_1.4.2_darwin_arm64.tar.gz"}, "gadget"},
		{[]string{"my-tool-1.0.0-linux-amd64.tar.gz", "my-tool-1.0.0-darwin-arm64.tar.gz"}, "my-tool"},
		{[]string{"solo_1.0.0_linux_amd64.tar.gz"}, "solo"},
		{[]string{"a_1.0.0_linux_amd64.tar.gz", "b_1.0.0_linux_arm64.tar.gz"}, ""},
		// cargo-dist 风格：没有版本号，公共前缀削到分隔符为止。
		{[]string{"ripgrep-x86_64-unknown-linux-gnu.tar.xz", "ripgrep-aarch64-apple-darwin.tar.xz"}, "ripgrep"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := Infer(c.in, "").Name; got != c.want {
			t.Errorf("Infer(%v).Name = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// 认出来的资产按平台 key 排序：同一批文件名每次都要跑出同样的 toy.toml，
// 否则 init 每跑一次就是一个无意义的 diff。
func TestInferSortsByPlatform(t *testing.T) {
	res := Infer([]string{
		"g_1.0.0_linux_amd64.tar.gz",
		"g_1.0.0_darwin_arm64.tar.gz",
		"g_1.0.0_linux_arm64.tar.gz",
	}, "")
	var got []string
	for _, a := range res.Assets {
		got = append(got, a.Platform)
	}
	want := []string{"darwin/arm64", "linux/amd64", "linux/arm64"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("顺序 = %v，想要 %v", got, want)
	}
}
