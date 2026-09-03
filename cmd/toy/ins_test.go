package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/platform"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

func insTestResolved(t *testing.T, s, tag string) *resolved {
	t.Helper()
	r, err := ref.Parse(s)
	if err != nil {
		t.Fatalf("ref.Parse(%q): %v", s, err)
	}
	res := &resolved{Ref: r, Owner: r.Owner, Repo: r.Repo, Tag: tag, Trust: store.TrustHost}
	if r.Kind == ref.KindHost {
		res.HostName = r.Host
		res.Host = host.Builtin()[r.Host]
		res.Version = semver.V{Raw: tag}
	}
	return res
}

// Get the upgrade decision wrong and you either install the wrong version or
// never upgrade at all.
func TestInsIsNewer(t *testing.T) {
	cases := []struct {
		old, remote string
		want        bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.3.0", "v1.2.0", false}, // remote is older: leave it alone, or that is a downgrade
		{"v1.2.0", "v1.2.1", true},
		// If either side is not semver, all we can go on is "the tag changed".
		// Treating it as 0.0.0 would "upgrade" release-2024 into something
		// nobody asked for.
		{"release-2024", "release-2025", true},
		{"release-2024", "release-2024", false},
	}
	for _, c := range cases {
		got := insIsNewer(store.Manifest{Tag: c.old}, &resolved{Tag: c.remote})
		if got != c.want {
			t.Errorf("insIsNewer(%q -> %q) = %v, want %v", c.old, c.remote, got, c.want)
		}
	}
}

// Iron law 6: what gets pinned is the hash of one ref@version. Leave the tag
// out of the id and every normal upgrade gets reported as "the hash changed" --
// and a warning that fires every day is a warning nobody reads.
func TestInsTrustIDCarriesTheTag(t *testing.T) {
	res := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insPackageID(res), "github:Derrity/tinyserve"; got != want {
		t.Fatalf("insPackageID = %q, want %q", got, want)
	}
	if got, want := insTrustID(res), "github:Derrity/tinyserve@v1.2.0"; got != want {
		t.Fatalf("insTrustID = %q, want %q", got, want)
	}
	// A bare URL has no tag, so the id is the URL itself -- if the content
	// behind that URL changes we have to stop.
	u := insTestResolved(t, "https://example.com/x/toy.toml", "")
	if got := insTrustID(u); got != "https://example.com/x/toy.toml" {
		t.Fatalf("trust id for a bare URL is %q", got)
	}
}

// {tag} comes from upstream, and it is the expanded form that actually gets
// spliced into the download URL (iron laws 7 and 8).
func TestInsSafeAsset(t *testing.T) {
	if err := insSafeAsset("gitwip", "scripts/gitwip.sh"); err != nil {
		t.Fatalf("an ordinary relative path must not be rejected: %v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../../x.sh", "a.sh?raw=1", "a.sh#frag"} {
		if err := insSafeAsset("gitwip", bad); err == nil {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

// The bare-URL path has no host behind it, so assets can only be resolved
// relative to the toy.toml's own URL.
func TestInsAssetURL(t *testing.T) {
	u := insTestResolved(t, "https://example.com/tools/toy.toml", "")
	got, err := insAssetURL(u, "tinyserve.tar.gz", false)
	if err != nil || got != "https://example.com/tools/tinyserve.tar.gz" {
		t.Fatalf("asset URL for a bare URL = %q (err=%v)", got, err)
	}

	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	rel, err := insAssetURL(h, "tinyserve_1.2.0_linux_amd64.tar.gz", false)
	if err != nil || !strings.Contains(rel, "/releases/download/v1.2.0/") {
		t.Fatalf("release URL = %q (err=%v)", rel, err)
	}
	// A [script] value is a path inside the repo, so it goes through the raw
	// template. Send it through the release template and the slashes get
	// escaped to %2F, and what comes back is an asset that does not exist.
	raw, err := insAssetURL(h, "scripts/gitwip.sh", true)
	if err != nil || !strings.Contains(raw, "scripts/gitwip.sh") {
		t.Fatalf("raw URL = %q (err=%v)", raw, err)
	}
}

// Iron law 8: the trust line has to say exactly who is being trusted.
func TestInsTrustDetail(t *testing.T) {
	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insTrustDetail(h), "github.com/Derrity"; got != want {
		t.Fatalf("host-tier detail = %q, want %q", got, want)
	}
	// The signed tier can only say "which key, registered in which index".
	// The signed message is id + source + toy.toml, and not one byte of it
	// binds the public key to the repo owner -- so writing "Derrity (key ...)"
	// is claiming a name on behalf of an unrelated key.
	h.Trust, h.Signer, h.IndexName = store.TrustSigned, "a3f91b2c…", "default"
	got := insTrustDetail(h)
	if !strings.Contains(got, "a3f91b2c…") || !strings.Contains(got, "default") {
		t.Fatalf("the signed tier must name the key and who registered it, got %q", got)
	}
	if strings.HasPrefix(got, "Derrity") {
		t.Fatalf(`must not print the repo owner behind the checkmark; that reads as "Derrity signed this": %q`, got)
	}
}

// TestInsTrustDowngradeNeedsForce nails down "a signature must not quietly
// disappear": verifying signatures cannot catch this on its own. Delete the
// sig from the index entry and there is nothing left to verify, the checkmark
// is simply gone -- and nobody notices a missing checkmark (spec 4 on TOFU,
// iron law 8).
func TestInsTrustDowngradeNeedsForce(t *testing.T) {
	signed := &store.Manifest{Name: "tinyserve", Trust: store.TrustSigned, Signer: "a3f9…"}
	for _, c := range []struct {
		name string
		old  *store.Manifest
		res  *resolved
		want bool
	}{
		{"signature vanished: stop", signed, &resolved{Trust: store.TrustHost}, true},
		{"dropping to unverified: stop harder", signed, &resolved{Trust: store.TrustUnverified}, true},
		{"still signed: go ahead", signed, &resolved{Trust: store.TrustSigned}, false},
		{"first install has nothing to compare against", nil, &resolved{Trust: store.TrustHost}, false},
		{"never was signed, so this is not a downgrade",
			&store.Manifest{Trust: store.TrustHost}, &resolved{Trust: store.TrustHost}, false},
	} {
		if got := insTrustDowngraded(c.old, c.res); got != c.want {
			t.Errorf("%s: insTrustDowngraded = %v, want %v", c.name, got, c.want)
		}
	}
}

// Iron law 8: if the match was not the first candidate, we installed something
// that was not built for this machine. Both kinds of fallback fail in ways that
// look nothing like a fallback -- missing Rosetta reads as "cannot be opened",
// a glibc binary dropped into Alpine reads as "file not found" -- which is why
// the note has to come out at install time.
func TestInsFallbackNote(t *testing.T) {
	cases := []struct {
		name     string
		cands    []string
		matched  string
		declared []string
		want     []string // all must appear; empty means there must be no note at all
	}{
		{"first candidate matched: stay quiet", []string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64/gnu",
			[]string{"linux/amd64", "linux/amd64/gnu"}, nil},
		// This is the most important case in this function, and the one it used
		// to be the only one not covering: the vast majority of projects ship
		// two-segment keys only, and on linux the first candidate is always the
		// /gnu one we synthesize ourselves. Make the rule "warn whenever the
		// first candidate missed" and every ordinary install grows a bogus
		// warning -- and a warning that fires every time drowns out the two
		// real ones below.
		{"two-segment-key package on a glibc box: that IS the right build, stay quiet",
			[]string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64", "linux/arm64"}, nil},
		{"author split by libc but shipped no glibc build: say so",
			[]string{"linux/amd64/gnu", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64", "linux/amd64/musl"},
			[]string{"libc", "linux/amd64"}},
		{"Apple Silicon falling back to Intel: mention Rosetta",
			[]string{"darwin/arm64", "darwin/amd64"}, "darwin/amd64",
			[]string{"darwin/amd64"},
			[]string{"Rosetta", "Intel"}},
		// musl the other way around: a bare linux/amd64 is almost never a musl
		// build, so we say something whether or not the author split by libc --
		// the price of silence is a "file is missing" for a file that is
		// plainly right there.
		{"musl box falling back to the generic build: warn it will look like a missing file (even with no split)",
			[]string{"linux/amd64/musl", "linux/amd64"}, "linux/amd64",
			[]string{"linux/amd64"},
			[]string{"musl", "file is missing"}},
		// glibc box bottoming out on the musl tier (last candidate in the
		// chain). This is nearly always good news -- projects that ship musl
		// only for x86-64 (ripgrep, zoxide) are installable because of it --
		// but say it anyway: this is not the build made for this machine, and
		// if that musl build happens to be dynamically linked it will not run
		// here.
		{"glibc box found only a musl build: say it works because it is static",
			[]string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}, "linux/amd64/musl",
			[]string{"linux/amd64/musl"},
			[]string{"musl", "statically linked", "linux/amd64"}},
		{"same on arm64", []string{"linux/arm64/gnu", "linux/arm64", "linux/arm64/musl"}, "linux/arm64/musl",
			[]string{"linux/arm64/musl"}, []string{"musl", "statically linked"}},
		// Matching the middle (bare) key in a three-candidate chain must not
		// change the rule: the author did not split by libc, so the generic
		// build IS the right one. A longer candidate chain is no excuse to
		// start emitting bogus warnings.
		{"with musl appended to the chain, matching the bare key still stays quiet",
			[]string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}, "linux/amd64",
			[]string{"linux/amd64", "linux/arm64"}, nil},
		{"other fallbacks get a note too",
			[]string{"linux/armv7", "linux/arm"}, "linux/arm",
			[]string{"linux/arm"},
			[]string{"linux/armv7", "linux/arm"}},
		{"no candidates, nothing to say", nil, "linux/amd64", []string{"linux/amd64"}, nil},
	}
	for _, c := range cases {
		got := insFallbackNote(c.cands, c.matched, c.declared)
		if len(c.want) == 0 {
			if got != "" {
				t.Errorf("%s: there should be no note, got %q", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: the note must contain %q, got %q", c.name, w, got)
			}
		}
	}
}

// Picking an asset has to walk the candidate order, not compare one exact
// GOOS/GOARCH. Compare only one and installing any "generic linux build only"
// package on Alpine gets judged "no build for this platform" -- when that
// package very likely does install on Alpine.
func TestInsItemsFallsBackThroughCandidates(t *testing.T) {
	cands := platform.Candidates()
	if len(cands) < 2 {
		t.Skipf("this machine has only one candidate (%v), no fallback to exercise", cands)
	}
	// Pick a candidate that is NOT GOOS/GOARCH: only then does the "compare one
	// exact key" implementation fail while the candidate order succeeds -- test
	// with GOOS/GOARCH itself and both implementations pass.
	only := ""
	for _, c := range cands {
		if c != runtime.GOOS+"/"+runtime.GOARCH {
			only = c
			break
		}
	}
	if only == "" {
		t.Skipf("every candidate on this machine equals GOOS/GOARCH (%v), no fallback to exercise", cands)
	}
	tf, err := toyfile.Parse(fmt.Appendf(nil,
		"name = \"probe\"\n[bin]\nprobe = \"probe\"\n[artifact]\n%q = \"probe_{version}_x.tar.gz\"\n", only))
	if err != nil {
		t.Fatal(err)
	}
	res := insTestResolved(t, "github:o/r", "v1.0.0")
	res.Toyfile = tf
	items, err := insItems(res)
	if err != nil {
		t.Fatalf("a package declaring only %q should install via the candidate order, got: %v", only, err)
	}
	if len(items) != 1 || items[0].asset != "probe_1.0.0_x.tar.gz" {
		t.Fatalf("wrong asset picked: %+v", items)
	}
}
