package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only a licence that allows using and redistributing the binaries gets a repo listed.
// Each way of failing has its own sentinel, because each asks something different of
// the maintainer: nothing (no licence), reading a file (unrecognized), or nothing
// again (source-available).
func TestDecideLicence(t *testing.T) {
	reviewed := map[string]string{"jqlang/jq": "MIT", "acme/busl": "BUSL-1.1"}
	cases := []struct {
		id, spdx string
		st       licenceState
		want     string
		err      error
	}{
		{"a/b", "MIT", licenceKnown, "MIT", nil},
		{"a/b", "GPL-3.0", licenceKnown, "GPL-3.0", nil},
		{"a/b", "BUSL-1.1", licenceKnown, "", errNotOpen},
		{"a/b", "SSPL-1.0", licenceKnown, "", errNotOpen},
		{"a/b", "", licenceNone, "", errNoLicence},
		{"a/b", "", licenceUnrecognized, "", errUnrecognized},
		// A person read jq's COPYING and wrote down MIT.
		{"jqlang/jq", "", licenceUnrecognized, "MIT", nil},
		{"JQLang/JQ", "", licenceUnrecognized, "MIT", nil},
		// Writing a licence down names it; it does not let it past the allowlist.
		{"acme/busl", "", licenceUnrecognized, "", errNotOpen},
	}
	for _, c := range cases {
		got, err := decideLicence(c.id, c.spdx, c.st, reviewed)
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("decideLicence(%q, %q, %v) = %q, %v; want %q, %v", c.id, c.spdx, c.st, got, err, c.want, c.err)
		}
	}
}

func TestGenRefusesWhatTheLicenceDoesNotAllow(t *testing.T) {
	ok := tinyserve
	none := tinyserve
	none.license = ""
	busl := tinyserve
	busl.license = "BUSL-1.1"
	other := tinyserve
	other.license = "NOASSERTION"
	repos := map[string]fakeRepo{"a/ok": ok, "a/none": none, "a/busl": busl, "a/other": other}

	out := filepath.Join(t.TempDir(), "idx.json")
	if err := runFakeGen(t, repos, []string{"a/ok", "a/none", "a/busl", "a/other"}, out, false); err != nil {
		t.Fatal(err)
	}
	idx := loadOut(t, out)
	if len(idx.Packages) != 1 || idx.Packages[0].ID != "a/ok" {
		t.Fatalf("only the MIT repo may be listed, got %+v", idx.Packages)
	}
	if !strings.Contains(idx.Packages[0].Toyfile, `license = "MIT"`) {
		t.Errorf("the licence should be written into the toyfile:\n%s", idx.Packages[0].Toyfile)
	}
}

// Once a person has read an unrecognized licence and written it down, the repo goes in,
// with that licence in its toyfile.
func TestGenUsesALicenceReadByHand(t *testing.T) {
	other := tinyserve
	other.license = "NOASSERTION"
	srv := fakeAPI(t, map[string]fakeRepo{"jqlang/jq": other})
	out := filepath.Join(t.TempDir(), "idx.json")
	err := gen(t.Context(), genConfig{
		repos: []string{"jqlang/jq"}, out: out, prov: newGitHub(srv.URL, ""),
		lists: lists{reviewed: map[string]string{"jqlang/jq": "MIT"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e := loadOut(t, out).Packages; len(e) != 1 || !strings.Contains(e[0].Toyfile, `license = "MIT"`) {
		t.Fatalf("want one entry carrying the reviewed licence, got %+v", e)
	}
}

// A maintainer who asked to go stays gone, even when the line comes from a list that
// was generated -- and the check happens before any API call.
func TestGenNeverRelistsADelistedRepo(t *testing.T) {
	srv := fakeAPI(t, map[string]fakeRepo{}) // any call would 404 and show up as an error
	out := filepath.Join(t.TempDir(), "idx.json")
	err := gen(t.Context(), genConfig{
		repos: []string{"Gone/Away"}, out: out, prov: newGitHub(srv.URL, ""),
		lists: lists{delisted: map[string]bool{"gone/away": true}},
	})
	if err != nil {
		t.Fatalf("a delisted repo is a skip, not a failure: %v", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("nothing should have been written")
	}
}

func TestLoadListsRejectsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	lic := filepath.Join(dir, "licenses.txt")
	del := filepath.Join(dir, "delisted.txt")
	os.WriteFile(lic, []byte("# header\njqlang/jq MIT   # COPYING is the MIT text\n\n"), 0o644)
	os.WriteFile(del, []byte("someone/gone  # asked 2026-10-13\n"), 0o644)
	l, err := loadLists(lic, del, "")
	if err != nil || l.reviewed["jqlang/jq"] != "MIT" || !l.delisted["someone/gone"] {
		t.Fatalf("got %+v, %v", l, err)
	}
	// A typo in delisted.txt must not quietly re-list someone.
	os.WriteFile(del, []byte("not-a-repo\n"), 0o644)
	if _, err := loadLists(lic, del, ""); err == nil {
		t.Fatal("a malformed delisted.txt line must be an error")
	}
	if _, err := loadLists(filepath.Join(dir, "absent"), filepath.Join(dir, "absent2"), filepath.Join(dir, "absent3")); err != nil {
		t.Fatalf("missing files are empty lists: %v", err)
	}
}
