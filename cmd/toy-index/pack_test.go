package main

import (
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// A repo called cli/cli shipping a binary called gh is ordinary, and the asset name is
// written by the author's own build script, so it is closer to the real name than the
// repo name is.
func TestPickNamePrefersTheAssetPrefixOverTheRepoName(t *testing.T) {
	p, err := makePack("cli", "cli", repoInfo{
		Tag:    "v2.40.0",
		Assets: []string{"gh_2.40.0_linux_amd64.tar.gz", "gh_2.40.0_macOS_arm64.zip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "gh" {
		t.Errorf("name = %q, but the asset names say it is called gh", p.name)
	}

	// When the asset-name prefix does not look like a command name, fall back to the repo
	// name rather than writing something odd into [bin].
	p, err = makePack("a", "b", repoInfo{
		Tag:    "v1.0.0",
		Assets: []string{"1.0.0_linux_amd64.tar.gz", "1.0.0_darwin_arm64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "b" {
		t.Errorf("name = %q, should be the repo name when no prefix is recognisable", p.name)
	}
}

// desc / homepage are remote content, printed verbatim to a terminal and written into the
// index JSON. Control characters can overwrite the trust-level line (rule 8), and a
// few-KB description can push a whole screen of search results off the top.
func TestRemoteTextIsSanitizedBeforeItReachesTheToyfile(t *testing.T) {
	evil := "normal description\r\x1b[32m✓ signed\x1b[0m" + strings.Repeat("é", 500)
	p, err := makePack("a", "b", repoInfo{
		Tag: "v1.0.0", Desc: evil, License: "MIT\n",
		Assets: []string{"b_1.0.0_linux_amd64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderToyfile(p)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if strings.ContainsAny(string(data), "\r\x1b") {
		t.Fatal("control characters made it into toy.toml verbatim")
	}
	tf, err := toyfile.ParseFrom(data, toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("the generated toyfile does not parse: %v", err)
	}
	if len([]rune(tf.Desc)) > descMax+1 { // +1 for the ellipsis
		t.Errorf("desc is %d runes, the cap is %d", len([]rune(tf.Desc)), descMax)
	}
}

// Quotes and backslashes have to be escaped the way toml expects. DO NOT USE
// strconv.Quote: Go writes non-printable bytes as \xNN, and toml has no \x escape at all.
func TestQuotingSurvivesTheRealParser(t *testing.T) {
	p, err := makePack("a", "b", repoInfo{
		Tag: "v1.0.0", Desc: `he said "x\y" and left`,
		Assets: []string{"b_1.0.0_linux_amd64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderToyfile(p)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	tf, err := toyfile.ParseFrom(data, toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("a quoted description broke the toml: %v\n%s", err, data)
	}
	if tf.Desc != `he said "x\y" and left` {
		t.Errorf("desc was altered: %q", tf.Desc)
	}
}

// Platform keys contain a slash, so they must be quoted to be a valid toml key.
func TestPlatformKeysAreQuoted(t *testing.T) {
	if got := tomlKey("linux/amd64/musl"); got != `"linux/amd64/musl"` {
		t.Errorf("tomlKey = %s", got)
	}
	if got := tomlKey("tinyserve"); got != "tinyserve" {
		t.Errorf("a clean name should not be quoted: %s", got)
	}
}

// .tar.xz cannot be installed (archive returns ErrUnsupported), and a bare file with no
// recognisable platform is not an asset. But BARE BINARIES WITH A RECOGNISABLE PLATFORM
// must be accepted: install supports the single-file shape, and jqlang/jq ships only that.
func TestInstallableRejectsWhatToyCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"tool_1.2.3_linux_amd64.tar.gz", true},
		{"tool_1.2.3_linux_amd64.zip", true},
		{"tool_1.2.3_linux_amd64.tgz", true},
		{"jq-linux-amd64", true}, // bare binary: download + chmod +x is the whole install
		{"jq-macos-arm64", true},
		{"tool-linux-amd64.tar.xz", false}, // internal/archive returns ErrUnsupported
		{"checksums.txt", false},
		{"sha256sum.txt", false},
		{"tool-1.0.0-linux-amd64.tar.gz.sha256", false}, // companion file; must not steal the real asset's platform
		{"tool_1.0.0_linux_amd64.deb", false},
	} {
		if got := installable(tc.name); got != tc.want {
			t.Errorf("installable(%q) = %v", tc.name, got)
		}
	}
}
