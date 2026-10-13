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

	if err := merge(from, into, []string{linux, mac}, report, prExtras{toRead: []string{"read/me"}}); err != nil {
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
	for _, want := range []string{
		"`both` from ok/both: already the command of has/name", // collision with the index
		"`lo` from ok/linuxonly: two characters or less",
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
