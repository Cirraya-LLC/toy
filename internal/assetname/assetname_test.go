package assetname

import (
	"strings"
	"testing"
)

// Inferring from filenames is all this package does. Get one word wrong and
// somebody trying to install sees "no build for your platform" — and the author
// will never find out which line was to blame.
func TestClassify(t *testing.T) {
	cases := []struct {
		file, platform, goos string
	}{
		// -- goreleaser's default naming --
		{"gadget_1.4.2_linux_amd64.tar.gz", "linux/amd64", "linux"},
		// '_' tears x86_64 in half; the adjacent pair puts it back. This is
		// goreleaser's default naming.
		{"gadget_1.4.2_linux_x86_64.tar.gz", "linux/amd64", "linux"},
		{"gadget-1.4.2-linux-x64.tar.gz", "linux/amd64", "linux"},
		{"gadget_1.4.2_darwin_arm64.zip", "darwin/arm64", "darwin"},
		{"gadget_1.4.2_macos_aarch64.tar.gz", "darwin/arm64", "darwin"},
		{"gadget-osx-x86_64.tar.gz", "darwin/amd64", "darwin"},
		{"gadget-1.0.0-apple-arm64.tar.gz", "darwin/arm64", "darwin"},
		// musl / gnu become the third segment, which is exactly the first key
		// platform.Candidates looks for.
		{"gadget_1.4.2_linux_amd64_musl.tar.gz", "linux/amd64/musl", "linux"},
		{"gadget_1.4.2_linux_x86_64_gnu.tar.gz", "linux/amd64/gnu", "linux"},
		// The arm generation is specific enough on its own; hanging a libc off
		// it means nothing.
		{"gadget_1.4.2_linux_armv7.tar.gz", "linux/armv7", "linux"},
		{"gadget_1.4.2_linux_armhf.tar.gz", "linux/armv7", "linux"},
		{"gadget_1.4.2_linux_armv6.tar.gz", "linux/armv6", "linux"},
		{"gadget_1.4.2_linux_i386.tar.gz", "linux/386", "linux"},
		// Bare arm: what goreleaser produces with GOARCH=arm and no goarm set,
		// and linux/arm is the fallback candidate platform.go offers armv6/armv7
		// (spec §3.1).
		{"tool_1.2.3_linux_arm.tar.gz", "linux/arm", "linux"},
		{"tool-1.2.3-linux-arm.tar.gz", "linux/arm", "linux"},

		// -- Rust target triples (plenty of the tools we want to index are Rust) --
		// The extra segments (unknown / pc) are simply not recognized, so no
		// special case is needed.
		{"tool-1.0.0-x86_64-unknown-linux-gnu.tar.gz", "linux/amd64/gnu", "linux"},
		{"tool-1.0.0-x86_64-unknown-linux-musl.tar.gz", "linux/amd64/musl", "linux"},
		{"tool-1.0.0-aarch64-unknown-linux-gnu.tar.gz", "linux/arm64/gnu", "linux"},
		{"tool-1.0.0-aarch64-apple-darwin.tar.gz", "darwin/arm64", "darwin"},
		{"tool-1.0.0-x86_64-apple-darwin.tar.gz", "darwin/amd64", "darwin"},
		{"tool-1.0.0-armv7-unknown-linux-gnueabihf.tar.gz", "linux/armv7", "linux"},
		{"tool-1.0.0-i686-pc-windows-msvc.zip", "", "windows"},

		// -- cargo-dist: asset names often carry no version at all --
		{"tool-x86_64-unknown-linux-gnu.tar.xz", "linux/amd64/gnu", "linux"},
		{"tool-aarch64-apple-darwin.tar.xz", "darwin/arm64", "darwin"},

		// Spec §11: Windows is recognized, but it must not enter [artifact].
		{"gadget_1.4.2_windows_amd64.zip", "", "windows"},
		{"gadget_1.4.2_win_x86_64.zip", "", "windows"},
		// Anything unrecognized gets skipped. No guessing.
		{"checksums.txt", "", ""},
		{"artifacts.json", "", ""},
		{"gadget_1.4.2_linux.tar.gz", "", "linux"}, // os but no arch
		{"gadget_amd64.tar.gz", "", ""},            // arch but no os
	}
	for _, c := range cases {
		key, goos := detect(c.file)
		if key != c.platform || goos != c.goos {
			t.Errorf("detect(%q) = (%q,%q), want (%q,%q)", c.file, key, goos, c.platform, c.goos)
		}
		if got := Classify(c.file); got.Platform != c.platform {
			t.Errorf("Classify(%q).Platform = %q, want %q", c.file, got.Platform, c.platform)
		}
	}
}

// Skip is written for humans, and "worth mentioning" is a separate question
// from "was it skipped": Windows and unrecognized assets have to speak up,
// checksums.txt and friends must stay quiet (or they drown out the real line).
func TestClassifySkipReasons(t *testing.T) {
	cases := []struct {
		file    string
		skip    string
		notable bool
	}{
		{"gadget_1.0.0_linux_amd64.tar.gz", "", false},
		{"gadget_1.0.0_windows_amd64.zip", SkipWindows, true},
		// freebsd is recognized, toy just does not support it — which is not the
		// same as "cannot tell which platform this is": that message asks the
		// author to add an [artifact] line, and adding one still would not
		// install.
		{"tool_1.2.3_freebsd_riscv64.tar.gz", SkipForeignOS("freebsd"), true},
		{"tool-1.2.3-x86_64-unknown-illumos.tar.gz", SkipForeignOS("illumos"), true},
		{"tool_1.2.3_dragonfly_amd64.tar.gz", SkipUnrecognized, true}, // not in the table, so still "unrecognized"
		{"tool_1.2.3_solaris_amd64.tar.gz", SkipForeignOS("solaris"), true},
		{"checksums.txt", SkipNotAnAsset, false},
		{"LICENSE", SkipNotAnAsset, false},
		{"README.md", SkipNotAnAsset, false},
	}
	for _, c := range cases {
		got := Classify(c.file)
		if got.Skip != c.skip || got.Notable != c.notable {
			t.Errorf("Classify(%q) = (skip %q, notable %v), want (%q, %v)",
				c.file, got.Skip, got.Notable, c.skip, c.notable)
		}
	}
}

func TestClassifyVersion(t *testing.T) {
	cases := map[string]string{
		"gadget_1.4.2_linux_amd64.tar.gz":      "1.4.2",
		"gadget_v1.4.2_linux_amd64.tar.gz":     "1.4.2", // the v is not part of the version string
		"gadget-1.0.0-rc.1-linux-amd64.tar.gz": "1.0.0-rc.1",
		"tool-x86_64-unknown-linux-gnu.tar.xz": "", // cargo-dist often ships no version
		"gadget-2024.09-linux-amd64.tar.gz":    "", // two-component versions are not matched; matching them would replace the wrong thing
	}
	for f, want := range cases {
		if got := Classify(f).Version; got != want {
			t.Errorf("Classify(%q).Version = %q, want %q", f, got, want)
		}
	}
}

// Without turning the version into {version}, the author has to come back and
// edit this file on every release — and "push a tag, that's the release" is this
// product's selling point.
func TestInferReplacesTheVersion(t *testing.T) {
	res := Infer([]string{
		"gadget_1.4.2_linux_amd64.tar.gz",
		"gadget_1.4.2_darwin_arm64.tar.gz",
		"checksums.txt",
	}, "")
	if len(res.Assets) != 2 || len(res.Artifact) != 2 {
		t.Fatalf("recognized %d, want 2: %+v", len(res.Assets), res.Assets)
	}
	if res.Version != "1.4.2" {
		t.Fatalf("version = %q, want 1.4.2", res.Version)
	}
	for _, a := range res.Assets {
		if strings.Contains(a.Template, "1.4.2") {
			t.Errorf("%q still has the version hard-coded: %q", a.File, a.Template)
		}
		if !strings.Contains(a.Template, "{version}") {
			t.Errorf("%q has no {version} placeholder: %q", a.File, a.Template)
		}
		if res.Artifact[a.Platform] != a.Template {
			t.Errorf("Artifact[%q] = %q, does not match Assets", a.Platform, res.Artifact[a.Platform])
		}
	}
}

// When the asset name carries a v prefix, {version} does not (spec §3), so only
// the bare version may be replaced and the v has to stay in the template —
// replacing "v1.4.2" would generate a URL missing a v every single time.
func TestInferKeepsTheVPrefixInTheTemplate(t *testing.T) {
	res := Infer([]string{"gadget_v1.4.2_linux_amd64.tar.gz"}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("recognized %d, want 1", len(res.Assets))
	}
	if got, want := res.Assets[0].Template, "gadget_v{version}_linux_amd64.tar.gz"; got != want {
		t.Fatalf("template = %q, want %q", got, want)
	}
}

// forceVersion wins: it is the only way out when the filenames carry no
// three-component version (things like 2024.09). A leading v has to be trimmed,
// or the substitution never matches anything.
func TestInferHonoursTheGivenVersion(t *testing.T) {
	for _, given := range []string{"2024.09", "v2024.09", " v2024.09 "} {
		res := Infer([]string{"gadget-2024.09-linux-amd64.tar.gz"}, given)
		if got, want := res.Assets[0].Template, "gadget-{version}-linux-amd64.tar.gz"; got != want {
			t.Fatalf("--version %q: template = %q, want %q", given, got, want)
		}
	}
}

// Shipping both a .tar.gz and a .zip for one platform is common. Dropping one
// silently would let the author believe the format they shipped is the one in
// use, so it has to be said out loud.
func TestInferReportsDuplicatePlatforms(t *testing.T) {
	res := Infer([]string{
		"gadget_1.0.0_linux_amd64.tar.gz",
		"gadget_1.0.0_linux_amd64.zip",
	}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("one platform should keep one line, got %d", len(res.Assets))
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Skip, "linux/amd64") {
		t.Fatalf("the duplicate has to be explained, got %+v", res.Skipped)
	}
	if res.Skipped[0].File != "gadget_1.0.0_linux_amd64.zip" {
		t.Fatalf("the dropped one should be the later in sort order, got %q", res.Skipped[0].File)
	}
}

// Spec §11: no Windows. But skipping it silently lets the author believe
// Windows users can install.
func TestInferSaysWhyWindowsIsSkipped(t *testing.T) {
	res := Infer([]string{"gadget_1.0.0_windows_amd64.zip"}, "")
	if len(res.Assets) != 0 {
		t.Fatalf("Windows must not enter [artifact], got %+v", res.Assets)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Skip, "Windows") {
		t.Fatalf("the reason has to say Windows, got %+v", res.Skipped)
	}
}

// An unrecognized ASSET has to speak up. The symptom of dropping one silently
// is nasty: the counts look right, and the missing platform waits for a user to
// report it. But checksums.txt / LICENSE were never assets, and warning about
// them drowns out the real line.
func TestInferReportsUnrecognizedAssetsButNotStrayFiles(t *testing.T) {
	res := Infer([]string{
		"tool_1.2.3_linux_amd64.tar.gz",
		"tool_1.2.3_freebsd_riscv64.tar.gz", // looks like an asset, unrecognized -> speak up
		"checksums.txt",                     // not an asset -> stay quiet
		"LICENSE",
		"README.md",
	}, "")
	if len(res.Assets) != 1 {
		t.Fatalf("recognized %d assets, want 1: %+v", len(res.Assets), res.Assets)
	}
	var mentions int
	for _, s := range res.Skipped {
		if s.File == "tool_1.2.3_freebsd_riscv64.tar.gz" {
			mentions++
		}
		for _, quiet := range []string{"checksums.txt", "LICENSE", "README.md"} {
			if s.File == quiet {
				t.Errorf("should not speak up about non-asset %s: %+v", quiet, s)
			}
		}
	}
	if mentions != 1 {
		t.Errorf("an unrecognized asset should be reported once, got %d times: %+v", mentions, res.Skipped)
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
		// cargo-dist style: no version, so the common prefix is trimmed back to
		// the separator.
		{[]string{"ripgrep-x86_64-unknown-linux-gnu.tar.xz", "ripgrep-aarch64-apple-darwin.tar.xz"}, "ripgrep"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := Infer(c.in, "").Name; got != c.want {
			t.Errorf("Infer(%v).Name = %q, want %q", c.in, got, c.want)
		}
	}
}

// Recognized assets are sorted by platform key: the same filenames have to
// produce the same toy.toml every time, or every init run is a meaningless
// diff.
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
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A command name that still carries a platform word came from an asset name the
// inference did not fully understand. These are the shapes seen in real releases.
func TestPlatformWord(t *testing.T) {
	cases := map[string]string{
		"yt-dlp_linux_aarch64":   "linux",
		"gemini-darwin":          "darwin",
		"officecli-linux-alpine": "linux",
		"linux-x64":              "linux",
		"tool_x86_64":            "x86_64",
		"tool-musl":              "musl",
		"rg":                     "",
		"yt-dlp":                 "",
		"tinyserve":              "",
		"gh-dash":                "",
		"winget":                 "", // a word containing "win" is not the word "win"
	}
	for name, want := range cases {
		if got := PlatformWord(name); got != want {
			t.Errorf("PlatformWord(%q) = %q, want %q", name, got, want)
		}
	}
}
