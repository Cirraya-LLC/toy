package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
)

func inlineEntry(id, cmd string) index.Entry {
	return index.Entry{ID: id, Source: "github:" + id, Desc: "d | with a pipe", Stars: 10, Toyfile: "name = \"" + cmd + "\"\n" +
		"license = \"MIT\"\n[bin]\n" + cmd + " = \"" + cmd + "\"\n[artifact]\n\"linux/amd64\" = \"" + cmd + "_{version}_linux_amd64.tar.gz\"\n"}
}

func writeIdx(t *testing.T, path string, es ...index.Entry) {
	t.Helper()
	idx := &index.Index{Version: index.Version}
	for _, e := range es {
		idx.Upsert(e)
	}
	if err := writeIndex(path, idx); err != nil {
		t.Fatal(err)
	}
}

// Kept: passed somewhere and failed nowhere. A skip (no build for that platform) is
// neither, and an entry no run mentions was never checked.
func TestMergeKeepsOnlyWhatInstalled(t *testing.T) {
	dir := t.TempDir()
	from, into := filepath.Join(dir, "gen.json"), filepath.Join(dir, "index.json")
	writeIdx(t, from,
		inlineEntry("ok/both", "both"), inlineEntry("ok/linuxonly", "lo"),
		inlineEntry("bad/mac", "bm"), inlineEntry("never/checked", "nc"),
		inlineEntry("old/one", "ls"))
	writeIdx(t, into, inlineEntry("old/one", "old"), inlineEntry("has/name", "both"))
	linux, mac := filepath.Join(dir, "linux.tsv"), filepath.Join(dir, "mac.tsv")
	os.WriteFile(linux, []byte("ok/both\tpass\tboth\nok/linuxonly\tpass\tlo\nbad/mac\tpass\tbm\n"), 0o644)
	os.WriteFile(mac, []byte("ok/both\tpass\tboth\nok/linuxonly\tskip\tno darwin build\nbad/mac\tfail\trun: Exec format error\n"), 0o644)
	report := filepath.Join(dir, "pr.md")

	flagged := filepath.Join(dir, "flagged.txt")
	if err := merge(from, into, []string{linux, mac}, report, prExtras{toRead: []string{"read/me"}, flaggedPath: flagged}); err != nil {
		t.Fatal(err)
	}
	idx := loadOut(t, into)
	var ids []string
	for _, e := range idx.Packages {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, ","); got != "has/name,ok/both,ok/linuxonly,old/one" {
		t.Fatalf("index holds %s", got)
	}
	if e, _ := idx.Find("old/one"); !strings.Contains(e.Toyfile, "old = ") {
		t.Fatal("an entry already in the index must not be replaced from a batch")
	}

	md, _ := os.ReadFile(report)
	body := string(md)
	// Only a name that could take something else's holds the PR: the collision does,
	// the short name does not.
	if fl, _ := os.ReadFile(flagged); !strings.Contains(string(fl), "both\tok/both") || strings.Contains(string(fl), "lo\t") {
		t.Errorf("flagged file:\n%s", fl)
	}
	for _, want := range []string{
		"`both` from ok/both: already the command of has/name", // collision with the index
		"waits for a person",
		"- `lo` from ok/linuxonly", // a short name is shown, not held against the PR
		"bad/mac: run: Exec format error",
		"never/checked: not checked in run 1",
		`d \| with a pipe`, // upstream text cannot break the table
		"- [read/me](https://github.com/read/me)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the PR body is missing %q:\n%s", want, body)
		}
	}
}

func TestReadResultsRejectsUnknownStatus(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.tsv")
	os.WriteFile(p, []byte("a/b\tmaybe\tx\n"), 0o644)
	if _, err := readResults(p); err == nil {
		t.Fatal("an unknown status must be an error, not quietly treated as something")
	}
}

// With nothing that could collide, the PR says it merges itself, and the flagged file is
// empty -- which is what the discovery run reads to decide.
func TestMergeWithNothingBlockingSaysItMergesItself(t *testing.T) {
	dir := t.TempDir()
	from, into := filepath.Join(dir, "gen.json"), filepath.Join(dir, "index.json")
	writeIdx(t, from, inlineEntry("a/tool", "uniquetool"), inlineEntry("b/short", "hl"))
	writeIdx(t, into)
	res := filepath.Join(dir, "r.tsv")
	os.WriteFile(res, []byte("a/tool\tpass\tuniquetool\nb/short\tpass\thl\n"), 0o644)
	report, flagged := filepath.Join(dir, "pr.md"), filepath.Join(dir, "flagged.txt")
	if err := merge(from, into, []string{res}, report, prExtras{flaggedPath: flagged}); err != nil {
		t.Fatal(err)
	}
	if fl, err := os.ReadFile(flagged); err != nil || len(fl) != 0 {
		t.Fatalf("nothing should hold this PR, flagged file: %q %v", fl, err)
	}
	if md, _ := os.ReadFile(report); !strings.Contains(string(md), "merges itself") {
		t.Fatalf("the PR body should say it merges itself:\n%s", md)
	}
}
