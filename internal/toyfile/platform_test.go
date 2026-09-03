package toyfile

import (
	"errors"
	"strings"
	"testing"
)

const threeSeg = `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64/musl" = "x_musl.tar.gz"
"linux/amd64/gnu"  = "x_gnu.tar.gz"
"linux/amd64"      = "x.tar.gz"
"linux/armv6"      = "x_armv6.tar.gz"
"linux/armv7"      = "x_armv7.tar.gz"
"darwin/amd64"     = "x_darwin.tar.gz"
`

// Three-segment keys (GOOS/GOARCH/libc) and generation archs like armv6 / armv7 must be
// accepted, or an author has no way at all to say "this build is the musl one".
func TestAcceptsLibcAndARMGenerationKeys(t *testing.T) {
	if _, err := Parse([]byte(threeSeg)); err != nil {
		t.Fatalf("valid three-segment / armv6 / armv7 keys were rejected: %v", err)
	}
}

// But the third segment can only be a libc. Anything else does not fail, it just never
// matches -- the same silent dud as getting the case wrong, so it gets rejected on the
// spot too. Do not weaken this.
func TestRejectsUnknownThirdSegment(t *testing.T) {
	for _, plat := range []string{
		"linux/amd64/uclibc", "linux/amd64/v3", "linux/amd64/",
		"linux/amd64/musl/x", "linux//musl", "/amd64/musl",
	} {
		body := "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"" + plat + "\" = \"x.tar.gz\"\n"
		if tf, err := Parse([]byte(body)); err == nil {
			t.Errorf("platform key %q must be rejected, but it parsed to %+v", plat, tf)
		}
	}
	// The error must name the offending segment, or the author does not know what to fix.
	_, err := Parse([]byte("name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64/uclibc\" = \"x.tar.gz\"\n"))
	if err == nil || !strings.Contains(err.Error(), "uclibc") {
		t.Errorf("the error should name the offending segment: %v", err)
	}
}

func TestAssetForTakesTheFirstHit(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		candidates []string
		wantAsset  string
		wantMatch  string
	}{
		{"Alpine takes the musl build", []string{"linux/amd64/musl", "linux/amd64"}, "x_musl.tar.gz", "linux/amd64/musl"},
		{"glibc takes the gnu build", []string{"linux/amd64/gnu", "linux/amd64"}, "x_gnu.tar.gz", "linux/amd64/gnu"},
		{"no libc-specific build falls back to the bare key", []string{"linux/386/gnu", "linux/386", "linux/amd64"}, "x.tar.gz", "linux/amd64"},
		{"Pi 3 takes armv7", []string{"linux/armv7", "linux/arm"}, "x_armv7.tar.gz", "linux/armv7"},
		{"Pi Zero takes armv6", []string{"linux/armv6", "linux/arm"}, "x_armv6.tar.gz", "linux/armv6"},
		{"Apple Silicon falls back through Rosetta", []string{"darwin/arm64", "darwin/amd64"}, "x_darwin.tar.gz", "darwin/amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asset, _, matched, err := tf.AssetFor(c.candidates, "1.0.0", "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if asset != c.wantAsset {
				t.Errorf("asset = %q, want %q", asset, c.wantAsset)
			}
			// Callers rely on matched to tell the user a fallback happened (iron law 8).
			if matched != c.wantMatch {
				t.Errorf("matched key = %q, want %q", matched, c.wantMatch)
			}
		})
	}
}

// Order is priority: reverse the candidates and a different key must win. An
// implementation that hardcodes "musl first" fails this.
func TestAssetForRespectsCandidateOrder(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, matched, err := tf.AssetFor([]string{"linux/amd64", "linux/amd64/musl"}, "1.0.0", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if matched != "linux/amd64" {
		t.Errorf("should match linux/amd64, the earlier candidate; got %q", matched)
	}
}

// The sha256 has to follow the key that actually matched. Get it wrong and the musl hash
// is checked against the gnu build; under iron law 4 that refuses the install outright,
// which looks exactly like a poisoned package.
func TestAssetForReturnsTheMatchedPlatformsSHA(t *testing.T) {
	musl := strings.Repeat("a", 64)
	gnu := strings.Repeat("b", 64)
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64/musl" = "x_musl.tar.gz"
"linux/amd64"      = "x.tar.gz"
[sha256]
"linux/amd64/musl" = "` + musl + `"
"linux/amd64"      = "` + gnu + `"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, sum, _, _ := tf.AssetFor([]string{"linux/amd64/musl", "linux/amd64"}, "1", "v1"); sum != musl {
		t.Errorf("sha256 for the musl build = %q", sum)
	}
	if _, sum, _, _ := tf.AssetFor([]string{"linux/amd64"}, "1", "v1"); sum != gnu {
		t.Errorf("sha256 for the bare key = %q", sum)
	}
	// Easiest thing to get wrong on a fallback: the hash comes from the key that matched,
	// not from the first candidate.
	if _, sum, _, _ := tf.AssetFor([]string{"linux/386", "linux/amd64"}, "1", "v1"); sum != gnu {
		t.Errorf("falling back to linux/amd64 must use its own sha256, got %q", sum)
	}
}

// When nothing matches, the user needs to know what we tried and the author needs to
// know which key to add.
func TestAssetForListsTriedAndDeclared(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = tf.AssetFor([]string{"openbsd/riscv64"}, "1.0.0", "v1.0.0")
	if !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("want ErrNoPlatform: %v", err)
	}
	for _, want := range []string{"openbsd/riscv64", "linux/amd64/musl", "darwin/amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text is missing %q: %s", want, err)
		}
	}
	// An empty candidate list must not panic.
	if _, _, _, err := tf.AssetFor(nil, "1.0.0", "v1.0.0"); !errors.Is(err, ErrNoPlatform) {
		t.Errorf("an empty candidate list should give ErrNoPlatform: %v", err)
	}
}

// The script form has no per-platform assets: AssetFor must say so too, not hand back an
// empty string.
func TestAssetForRejectsScriptForm(t *testing.T) {
	tf, err := Parse([]byte("name = \"x\"\n[script]\nx = \"x.sh\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tf.AssetFor([]string{"linux/amd64"}, "1", "v1"); err == nil {
		t.Error("AssetFor on the script form must error")
	}
}

// The expanded placeholder still has to pass checkRelPath -- the fallback layer does not
// get to route around iron law 7. ({tag} comes from upstream, and a tag named ../../evil
// points the download URL at another repo.)
func TestAssetForStillChecksExpandedPath(t *testing.T) {
	tf, err := Parse([]byte("name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64/musl\" = \"x_{tag}.tar.gz\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = tf.AssetFor([]string{"linux/amd64/musl"}, "1.0.0", "../../../../evil/releases/download/v9/pwn")
	if !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("an asset name containing .. after expansion must be rejected: %v", err)
	}
}

// Three-segment keys have to sort too, in a stable order (map iteration order is
// randomized).
func TestPlatformsSortsThreeSegmentKeys(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"darwin/amd64", "linux/amd64", "linux/amd64/gnu", "linux/amd64/musl",
		"linux/armv6", "linux/armv7",
	}
	for i := 0; i < 20; i++ {
		got := tf.Platforms()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("Platforms() = %v, want %v", got, want)
		}
	}
}
