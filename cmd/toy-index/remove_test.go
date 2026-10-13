package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeclineRemovesAndRemembers(t *testing.T) {
	dir := t.TempDir()
	idx, list := filepath.Join(dir, "idx.json"), filepath.Join(dir, "declined.txt")
	writeIdx(t, idx, inlineEntry("a/gui", "gui"), inlineEntry("a/keep", "keep"))
	os.WriteFile(list, []byte("# header\n"), 0o644)

	if err := runRemove(t.Context(), "decline", []string{"--index", idx, "--list", list, "--reason", "a GUI app", "a/gui"}); err != nil {
		t.Fatal(err)
	}
	if p := loadOut(t, idx).Packages; len(p) != 1 || p[0].ID != "a/keep" {
		t.Fatalf("index after decline: %+v", p)
	}
	ls, err := loadLists("", "", list)
	if err != nil || !ls.declined["a/gui"] {
		t.Fatalf("a/gui should now be declined: %+v %v", ls.declined, err)
	}
	// Twice is harmless: no second line.
	runRemove(t.Context(), "decline", []string{"--index", idx, "--list", list, "--reason", "again", "a/gui"})
	if b, _ := os.ReadFile(list); strings.Count(string(b), "a/gui") != 1 {
		t.Fatalf("declined twice wrote two lines:\n%s", b)
	}
}

func TestDropRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "idx.json")
	writeIdx(t, idx, inlineEntry("too/new", "tn"))
	list := filepath.Join(dir, "drop.txt")
	if err := runRemove(t.Context(), "drop", []string{"--index", idx, "--list", list, "too/new"}); err != nil {
		t.Fatal(err)
	}
	if len(loadOut(t, idx).Packages) != 0 {
		t.Fatal("drop should remove the entry")
	}
	if _, err := os.Stat(list); err == nil {
		t.Fatal("drop must not record anything: it may come back")
	}
}

func TestDeclineAndDelistNeedAReason(t *testing.T) {
	for _, mode := range []string{"decline", "delist"} {
		if err := runRemove(t.Context(), mode, []string{"--index", filepath.Join(t.TempDir(), "x.json"), "a/b"}); err == nil {
			t.Errorf("%s without --reason should be refused", mode)
		}
	}
}
