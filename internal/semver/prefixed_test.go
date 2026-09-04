package semver

import "testing"

// jq tags every release as jq-1.8.2. Before this, Max told the user the
// repository "may not have tagged a release yet" while it was staring at 19 tags
// -- blaming an upstream that had done nothing wrong, and locking out a whole
// style of tagging that monorepos and C projects use too.
func TestPrefixedTagsAreUsedWhenNothingElseParses(t *testing.T) {
	const repoName = "jq"
	tags := []string{"jq-1.7", "jq-1.7rc2", "jq-1.8.0", "jq-1.8.1", "jq-1.8.2", "jq-1.8.2rc1"}
	v, err := Max(tags, false, repoName)
	if err != nil {
		t.Fatalf("jq's tags must resolve: %v", err)
	}
	if v.Raw != "jq-1.8.2" {
		t.Fatalf("Raw = %q, want jq-1.8.2 -- the download URL is built from the tag verbatim", v.Raw)
	}
	if got := v.Number(); got != "1.8.2" {
		t.Fatalf("Number() = %q, want 1.8.2 -- this is what {version} expands to and what names the pkg directory", got)
	}
}

// The guard that matters. ripgrep's repo carries its own releases as bare 15.2.0
// next to sub-crate tags like wincolor-0.1.6. Taking prefixed tags there would let
// a sub-crate that happens to sit on a higher version become the release we
// install -- and the download URL would be built from that sub-crate's tag.
func TestBareTagsAlwaysWinOverPrefixedOnes(t *testing.T) {
	const repoName = "ripgrep"
	tags := []string{"wincolor-0.1.6", "globset-99.0.0", "15.2.0", "grep-0.2.0"}
	v, err := Max(tags, false, repoName)
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "15.2.0" {
		t.Fatalf("Raw = %q, want 15.2.0 -- a sub-crate on a higher version must not win", v.Raw)
	}
}

// Two projects tagged in one repository: picking a maximum across them installs
// whichever sub-project is furthest ahead, which is nobody's intent. Refuse and
// name them, rather than guess.
func TestTwoPrefixesRefuseRatherThanGuess(t *testing.T) {
	_, err := Max([]string{"foo-1.0.0", "bar-2.0.0"}, false, "baz")
	if err == nil {
		t.Fatal("mixing two prefixes must be refused, not resolved")
	}
	for _, want := range []string{"foo", "bar", "--version"} {
		if !contains(err.Error(), want) {
			t.Errorf("the error must name the projects and the way out, missing %q: %v", want, err)
		}
	}
}

// Raw goes verbatim into the download URL and Number() names a directory under
// ~/.toy/pkg. A prefix carrying a slash or a dot-dot is upstream choosing our
// paths for us.
func TestPrefixCannotCarryPathCharacters(t *testing.T) {
	for _, bad := range []string{"../evil-1.0.0", "a/b-1.0.0", "..-1.0.0", "e vil-1.0.0"} {
		if _, err := parsePrefixed(bad); err == nil {
			t.Errorf("%q must be refused as a prefixed tag", bad)
		}
	}
}

// The last '-' can belong to the version, so the scan has to walk left.
func TestPrefixScanWalksPastPrereleaseDashes(t *testing.T) {
	v, err := parsePrefixed("mytool-1.2.3-rc.1")
	if err != nil {
		t.Fatalf("mytool-1.2.3-rc.1 should parse: %v", err)
	}
	if v.Prefix != "mytool-" || v.Number() != "1.2.3-rc.1" {
		t.Fatalf("Prefix = %q, Number = %q; want mytool- and 1.2.3-rc.1", v.Prefix, v.Number())
	}
}

// Parse itself stays strict: --version is checked with it, and a user asking for
// "jq-1.8.2" there should be told to pass the tag, not silently reinterpreted.
func TestParseItselfStillRejectsPrefixes(t *testing.T) {
	if _, err := Parse("jq-1.8.2"); err == nil {
		t.Fatal("Parse must stay strict; only Max's fallback accepts a prefix")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// gping migrated its tagging the other way from ripgrep: releases are
// gping-v1.21.0 today and the bare v1.0.0..v1.2.1 tags are 2021 leftovers.
// "Bare always wins" installed a three-year-old version whose asset names no
// longer exist upstream -- found by actually installing every index entry, not
// by reading code.
func TestARepoThatMigratedToPrefixedTagsGetsItsCurrentRelease(t *testing.T) {
	tags := []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.2.1", "gping-v1.19.0", "gping-v1.21.0"}
	v, err := Max(tags, false, "gping")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "gping-v1.21.0" {
		t.Fatalf("Raw = %q, want gping-v1.21.0 -- the bare tags are years-old leftovers", v.Raw)
	}
	if got := v.Number(); got != "1.21.0" {
		t.Fatalf("Number() = %q, want 1.21.0", got)
	}
}

// And the discriminator itself: the same tag list resolves differently for a
// repository the prefix does not name, because then it is a sub-project.
func TestThePrefixOnlyCountsWhenItNamesTheRepo(t *testing.T) {
	tags := []string{"15.2.0", "wincolor-99.0.0"}
	if v, _ := Max(tags, false, "ripgrep"); v.Raw != "15.2.0" {
		t.Fatalf("for ripgrep, wincolor-99.0.0 is a sub-crate; got %q", v.Raw)
	}
	if v, _ := Max(tags, false, "wincolor"); v.Raw != "wincolor-99.0.0" {
		t.Fatalf("for wincolor, that same tag IS the release; got %q", v.Raw)
	}
}

// The name comparison folds case and the separators projects vary on.
func TestRepoPrefixComparisonFoldsSeparatorsAndCase(t *testing.T) {
	for _, tag := range []string{"dua-cli-v1.0.0", "dua_cli-v1.0.0", "DUA-CLI-v1.0.0"} {
		if v, err := Max([]string{tag}, false, "dua-cli"); err != nil || v.Number() != "1.0.0" {
			t.Errorf("%q should resolve for repo dua-cli: %v (%q)", tag, err, v.Number())
		}
	}
}
