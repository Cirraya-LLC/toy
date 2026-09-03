package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/store"
)

// The id in the index is "Derrity/tinyserve", the id in an install manifest is
// "github:Derrity/tinyserve". If the two ends do not meet, every search result
// renders as "not installed".
func TestInstalledIndexMatchesBothShapes(t *testing.T) {
	idx := qryInstalledIndex([]store.Manifest{{
		ID: "github:Derrity/tinyserve", Name: "tinyserve", Version: "1.2.3",
		Host: "github", Owner: "Derrity", Repo: "tinyserve",
	}})

	for _, key := range []string{
		"github:Derrity/tinyserve", // the manifest's own id
		"Derrity/tinyserve",        // the index entry's id
		"derrity/TINYSERVE",        // owner/repo is case-insensitive at the host anyway
	} {
		m, ok := qryLookupInstalled(idx, key)
		if !ok || m.Version != "1.2.3" {
			t.Fatalf("%q should find the installed 1.2.3, got ok=%v version=%q", key, ok, m.Version)
		}
	}
	if _, ok := qryLookupInstalled(idx, "someone/else"); ok {
		t.Fatal("a package that was never installed must not be found")
	}
}

// Iron law 8: an unrecognized trust level must not render as the safe one, and
// "claims signed but cannot name a signer" is the same thing — that would be
// vouching for a key nobody can identify.
func TestTrustLabelNeverOverclaims(t *testing.T) {
	if s := qryTrustLabel(store.TrustSigned, ""); strings.HasPrefix(s, "✓") {
		t.Fatalf("no check mark without a signer, got %q", s)
	}
	if s := qryTrustLabel("totally-fine", ""); !strings.Contains(s, "totally-fine") || strings.HasPrefix(s, "✓") {
		t.Fatalf("an unknown level must be reported as-is and get no check mark, got %q", s)
	}
	if s := qryTrustLabel(store.TrustSigned, "a3f9…"); !strings.Contains(s, "a3f9…") {
		t.Fatalf("signed must carry the fingerprint, got %q", s)
	}
	if s := qryTrustLabel(store.TrustUnverified, ""); strings.HasPrefix(s, "✓") {
		t.Fatalf("unsigned gets no check mark, got %q", s)
	}
}

// The desc in an index entry is written by somebody else: \r plus a bit of ANSI
// is enough to overwrite the trust line that was just printed, and \t and \n
// throw the table's columns off. Escape it before it reaches tabwriter.
func TestCellEscapesControlBytes(t *testing.T) {
	got := qryCell("\r\x1b[32m✓ signed\tx\ny")
	for _, bad := range []string{"\r", "\x1b", "\t", "\n"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q still carries the control byte %q", got, bad)
		}
	}
	if !strings.Contains(got, `\x1b`) {
		t.Fatalf("the escaping has to be visible (\\xNN) — dropping it just wipes the attacker's tracks for them: %q", got)
	}
	if s := "an ordinary description ünïcödé"; qryCell(s) != s {
		t.Fatalf("ordinary text must be left alone, got %q", qryCell(s))
	}
}

func TestTruncKeepsRunesIntact(t *testing.T) {
	// Multibyte input on purpose: truncating by byte would slice a rune in half.
	if got := qryTrunc("αβγδε", 3); got != "αβγ…" {
		t.Fatalf("truncate by rune, got %q", got)
	}
	if got := qryTrunc("short", 10); got != "short" {
		t.Fatalf("leave it alone when it fits, got %q", got)
	}
}

func TestIndexURLAcceptsBarePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "index.json")
	got, err := qryIndexURL(p)
	if err != nil || got != "file://"+filepath.ToSlash(p) {
		t.Fatalf("a bare path should be treated as a local file, got %q (err=%v)", got, err)
	}
	if got, err := qryIndexURL("https://example.com/index.json"); err != nil || got != "https://example.com/index.json" {
		t.Fatalf("an https address should be taken as-is, got %q (err=%v)", got, err)
	}
	// Any scheme outside the allowlist is refused — the index URL goes straight to fetch.
	if _, err := qryIndexURL("ftp://example.com/index.json"); err == nil {
		t.Fatal("ftp:// must not be accepted")
	}
	if _, err := qryIndexURL("   "); err == nil {
		t.Fatal("an empty address must not be accepted")
	}
}

// info's "current platform" has to use the same candidate order install uses.
// Comparing one exact key reports "no build for this platform, cannot install"
// for cases that install fine (Rosetta, musl falling back to the generic build)
// — the user reads that screen and does not even try, though it would have worked.
func TestQryMatchPlatformFollowsCandidateOrder(t *testing.T) {
	cands := []string{"linux/amd64/musl", "linux/amd64"}
	if got := qryMatchPlatform([]string{"linux/amd64", "darwin/arm64"}, cands); got != "linux/amd64" {
		t.Fatalf("with only the generic key declared it should hit linux/amd64, got %q", got)
	}
	// The more specific one has to win when it exists — the other way round hands
	// Alpine users a glibc build.
	if got := qryMatchPlatform([]string{"linux/amd64", "linux/amd64/musl"}, cands); got != "linux/amd64/musl" {
		t.Fatalf("linux/amd64/musl should win, got %q", got)
	}
	if got := qryMatchPlatform([]string{"darwin/arm64"}, cands); got != "" {
		t.Fatalf("with no candidate matching it must return the empty string, got %q", got)
	}
}
