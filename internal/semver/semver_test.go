package semver

import (
	"math"
	"strings"
	"testing"
)

func TestParseOK(t *testing.T) {
	cases := []struct {
		in   string
		want V
	}{
		{"1.2.3", V{Major: 1, Minor: 2, Patch: 3, Pre: "", Raw: "1.2.3"}},
		{"v1.2.3", V{Major: 1, Minor: 2, Patch: 3, Pre: "", Raw: "v1.2.3"}},
		{"v0.0.0", V{Major: 0, Minor: 0, Patch: 0, Pre: "", Raw: "v0.0.0"}},
		{"v10.20.30", V{Major: 10, Minor: 20, Patch: 30, Pre: "", Raw: "v10.20.30"}},
		{"1.2.3-rc.1", V{Major: 1, Minor: 2, Patch: 3, Pre: "rc.1", Raw: "1.2.3-rc.1"}},
		{"v2.0.0-alpha", V{Major: 2, Minor: 0, Patch: 0, Pre: "alpha", Raw: "v2.0.0-alpha"}},
		{"v1.0.0-rc-1", V{Major: 1, Minor: 0, Patch: 0, Pre: "rc-1", Raw: "v1.0.0-rc-1"}},
		// Build metadata takes no part in comparison and is dropped on parse,
		// but Raw keeps it verbatim.
		{"v1.2.3+build.5", V{Major: 1, Minor: 2, Patch: 3, Pre: "", Raw: "v1.2.3+build.5"}},
		{"v1.2.3-rc.1+build.5", V{Major: 1, Minor: 2, Patch: 3, Pre: "rc.1", Raw: "v1.2.3-rc.1+build.5"}},
		// Leading zeros are not strictly semver, but real repos have them, so
		// accept them.
		{"v1.02.3", V{Major: 1, Minor: 2, Patch: 3, Pre: "", Raw: "v1.02.3"}},
		{"  v1.2.3  ", V{Major: 1, Minor: 2, Patch: 3, Pre: "", Raw: "v1.2.3"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"release-2024", // the kind of non-semver tag the spec calls out
		"latest",
		"v",
		"1.2",
		"1",
		"1.2.3.4",
		"v1.2.x",
		"v1..3",
		"1.-2.3",
		"v1.2.-3",
		"1.0.0-",      // empty pre-release tag
		"1.0.0-rc..1", // empty part in the middle
		"1.0.0-rc.",   // trailing dot
		"1.0.0-rc!",   // illegal character
		"v 1.2.3",
		"99999999999999999999.0.0", // overflow
		// Build metadata is dot-separated identifiers too, and "takes no part in
		// comparison" is no excuse for skipping validation — it still lands in
		// Raw / Number(), hence in directory names and download URLs.
		"v1.2.3+a/b",         // git really will let you push this tag; verified
		"1.2.3-rc.1+foo/bar", // pre-release plus build metadata with a slash
		"v1.2.3+",            // empty build metadata
		"v1.2.3+a..b",        // empty part in the middle
		"v1.2.3+a b",
		"v1.2.3+++",
		strings.Repeat("1", 200) + ".0.0",    // too long (and past int range)
		"v1.2.3-" + strings.Repeat("a", 200), // too long, though every part is legal
	}
	for _, s := range bad {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) should have failed, got %+v", s, v)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0}, // the v prefix does not affect comparison
		{"2.0.0", "1.9.9", 1},
		{"1.2.0", "1.10.0", -1},
		{"1.2.10", "1.2.9", 1},
		{"0.0.1", "0.0.0", 1},
		// A pre-release sorts before the stable release of the same number
		{"1.0.0-rc", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc", 1},
		{"1.0.0-rc", "0.9.9", 1},
		{"1.0.0-rc.1", "1.0.0-rc.1", 0},
		// All-digit parts compare by value, not as strings
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1}, // numeric parts rank below parts with letters
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},      // more parts wins
	}
	for _, c := range cases {
		a, err := Parse(c.a)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.a, err)
		}
		b, err := Parse(c.b)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.b, err)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(b, a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, reversed it should be %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestMax(t *testing.T) {
	cases := []struct {
		name     string
		tags     []string
		allowPre bool
		want     string // expected Raw
	}{
		{"picks the highest", []string{"v1.0.0", "v1.2.0", "v0.9.9"}, false, "v1.2.0"},
		{"the highest is not last", []string{"v0.1.0", "v2.0.0", "v1.5.0"}, false, "v2.0.0"},
		{"skips non-semver tags", []string{"release-2024", "latest", "v1.0.0", "nightly"}, false, "v1.0.0"},
		{"pre-releases ignored by default", []string{"v1.0.0", "v2.0.0-rc.1"}, false, "v1.0.0"},
		{"allowPre is what takes the rc", []string{"v1.0.0", "v2.0.0-rc.1"}, true, "v2.0.0-rc.1"},
		{"stable beats the rc of the same number", []string{"v1.0.0-rc.1", "v1.0.0"}, true, "v1.0.0"},
		{"mixed v prefixes", []string{"1.0.0", "v1.1.0"}, false, "v1.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Max(c.tags, c.allowPre)
			if err != nil {
				t.Fatalf("Max failed: %v", err)
			}
			if got.Raw != c.want {
				t.Fatalf("Max = %q, want %q", got.Raw, c.want)
			}
		})
	}
}

func TestMaxErrors(t *testing.T) {
	bad := []struct {
		name     string
		tags     []string
		allowPre bool
	}{
		{"empty list", nil, false},
		{"all non-semver", []string{"release-2024", "latest", "main"}, false},
		{"only pre-releases, and they are not allowed", []string{"v1.0.0-rc.1", "v1.0.0-rc.2"}, false},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if v, err := Max(c.tags, c.allowPre); err == nil {
				t.Fatalf("should have failed, got %+v", v)
			}
		})
	}
}

// A non-semver tag must be SKIPPED, never treated as 0.0.0 — otherwise it beats
// a genuinely low version.
func TestMaxNeverTreatsJunkAsZero(t *testing.T) {
	got, err := Max([]string{"release-2024", "v0.0.1"}, false)
	if err != nil {
		t.Fatalf("Max failed: %v", err)
	}
	if got.Raw != "v0.0.1" {
		t.Fatalf("Max = %q, want v0.0.1", got.Raw)
	}
}

func TestNumber(t *testing.T) {
	cases := []struct {
		in   V
		want string
	}{
		{mustParse(t, "v1.2.3"), "1.2.3"},
		{mustParse(t, "1.2.3"), "1.2.3"},
		{mustParse(t, "v1.2.3-rc.1"), "1.2.3-rc.1"},
		{mustParse(t, "v1.2.3+build.5"), "1.2.3+build.5"},
		{V{Major: 1, Minor: 2, Patch: 3}, "1.2.3"},                   // hand-built, no Raw
		{V{Major: 1, Minor: 0, Patch: 0, Pre: "rc.1"}, "1.0.0-rc.1"}, // same, with a pre-release
	}
	for _, c := range cases {
		if got := c.in.Number(); got != c.want {
			t.Errorf("%+v.Number() = %q, want %q", c.in, got, c.want)
		}
	}
}

func mustParse(t *testing.T, s string) V {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

// Whatever Parse lets through becomes, verbatim, a directory name under
// ~/.toy/pkg (spec §6) and part of a release download URL (§2.4, via Number()
// → toyfile.Expand). So "it parsed" must imply "it is safe to use as a path
// component and a URL component".
//
// Written as a fuzz target rather than a table: a table only catches the bad
// characters I thought of, and this invariant needs to catch the ones nobody
// thinks of the day someone loosens the grammar. Without -fuzz it just runs the
// seeds below.
func FuzzParseNeverYieldsUnsafeVersion(f *testing.F) {
	for _, seed := range []string{
		"v1.2.3", "1.2.3-rc.1", "v1.2.3+build.5", "v1.2.3-rc.1+build.5",
		"v1.2.3+a/b", "v1.2.3+../x", "v1.2.3+\n", "release-2024", "v1.2.3+",
		"", "v", "1.2.3.4", "v1.02.3",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := Parse(s)
		if err != nil {
			return // rejected, so there is no invariant left to hold
		}
		for what, got := range map[string]string{"Raw": v.Raw, "Number()": v.Number()} {
			if got == "" {
				t.Fatalf("Parse(%q) passed, but %s is empty — an empty string cannot be a directory name", s, what)
			}
			if len(got) > maxLen {
				t.Errorf("Parse(%q) passed, but %s is %d characters, over the %d limit", s, what, len(got), maxLen)
			}
			if strings.Contains(got, "..") {
				t.Errorf("Parse(%q) passed, but %s = %q contains \"..\" (path traversal)", s, what, got)
			}
			for _, c := range got {
				ok := c == '.' || c == '-' || c == '+' ||
					(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
				if !ok {
					t.Errorf("Parse(%q) passed, but %s = %q contains %q — this is used as a directory name and a URL component", s, what, got, c)
				}
			}
		}
	})
}

// V's fields are exported, so "they are all non-negative" is not something
// anything guarantees. A subtraction-based Compare overflows here and inverts
// the ordering.
func TestCompareDoesNotOverflowOnHandBuiltV(t *testing.T) {
	lo, hi := V{Major: math.MinInt}, V{Major: math.MaxInt}
	if got := Compare(lo, hi); got != -1 {
		t.Errorf("Compare(min, max) = %d, want -1", got)
	}
	if got := Compare(hi, lo); got != 1 {
		t.Errorf("Compare(max, min) = %d, want 1", got)
	}
}

// "No semver tags at all" and "only pre-releases" are two different situations
// for the user: the first leaves nothing to do but nag upstream to tag a
// release, while the second is solved by adding --version. Collapsing both into
// one vague error leaves the second person with nowhere to go (spec §3: "fail
// with an error that tells the author exactly what to do").
func TestMaxErrorsTellYouWhichSituationYouAreIn(t *testing.T) {
	_, preOnly := Max([]string{"v1.0.0-rc.1", "v1.0.0-rc.2"}, false)
	if preOnly == nil {
		t.Fatal("pre-releases only should be an error")
	}
	if !strings.Contains(preOnly.Error(), "--version") {
		t.Errorf("with pre-releases only, the error should point at --version, got: %v", preOnly)
	}

	_, noSemver := Max([]string{"release-2024", "latest"}, false)
	if noSemver == nil {
		t.Fatal("all non-semver should be an error")
	}
	if strings.Contains(noSemver.Error(), "--version") {
		t.Errorf("with no semver tag at all, do not suggest --version (no tag would install): %v", noSemver)
	}
}
