package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cirraya-LLC/toy/internal/index"
)

// upstream is what the fake GitHub says about one repository.
type upstream int

const (
	hasReleases upstream = iota
	noReleases
	notFound
	otherError // null data with an error that is not NOT_FOUND: not evidence of anything
)

// deadServer answers dead's GraphQL query from a table, and counts the repositories asked.
func deadServer(t *testing.T, repos map[string]upstream, asked *[]string) *ghClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("dead asked for %s; it only ever needs GraphQL", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var q struct{ Query string }
		json.Unmarshal(body, &q)
		data := map[string]any{}
		var errs []any
		for _, m := range aliasRe.FindAllStringSubmatch(q.Query, -1) {
			alias, id := "r"+m[1], m[2]+"/"+m[3]
			*asked = append(*asked, id)
			switch repos[id] {
			case hasReleases:
				data[alias] = map[string]any{"releases": map[string]any{"totalCount": 4}}
			case noReleases:
				data[alias] = map[string]any{"releases": map[string]any{"totalCount": 0}}
			case notFound:
				data[alias] = nil
				errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{alias}, "message": "Could not resolve"})
			case otherError:
				data[alias] = nil
				errs = append(errs, map[string]any{"type": "FORBIDDEN", "path": []any{alias}, "message": "nope"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
	}))
	t.Cleanup(srv.Close)
	return newGitHub(srv.URL, "")
}

type deadFixture struct {
	idx, state, report, results string
}

func newDeadFixture(t *testing.T, results string, es ...string) deadFixture {
	t.Helper()
	dir := t.TempDir()
	f := deadFixture{
		idx: filepath.Join(dir, "idx.json"), state: filepath.Join(dir, "state", "dead.tsv"),
		report: filepath.Join(dir, "pr.md"), results: filepath.Join(dir, "results.tsv"),
	}
	var entries []index.Entry
	for _, id := range es {
		_, name, _ := strings.Cut(id, "/")
		entries = append(entries, inlineEntry(id, name))
	}
	writeIdx(t, f.idx, entries...)
	if err := os.WriteFile(f.results, []byte(results), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f deadFixture) run(t *testing.T, gh *ghClient, day string, extra ...func(*deadConfig)) {
	t.Helper()
	today, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	cfg := deadConfig{index: f.idx, results: []string{f.results}, state: f.state, days: 3, apply: true, report: f.report, today: today}
	for _, e := range extra {
		e(&cfg)
	}
	if err := dead(t.Context(), cfg, gh); err != nil {
		t.Fatal(err)
	}
}

func (f deadFixture) ids(t *testing.T) []string {
	var out []string
	for _, e := range loadOut(t, f.idx).Packages {
		out = append(out, e.ID)
	}
	return out
}

// The case this exists for: a repository with no releases left, failing every day, is
// dropped on the third daily run -- not before, and with a report saying why.
func TestDeadDropsOnTheThirdDayInARow(t *testing.T) {
	f := newDeadFixture(t, "gone/away\tfail\tentry: no semver tag\nalive/well\tpass\t\n", "gone/away", "alive/well")
	var asked []string
	gh := deadServer(t, map[string]upstream{"gone/away": noReleases}, &asked)

	f.run(t, gh, "2026-11-01")
	f.run(t, gh, "2026-11-01") // a second run on the same day is not a second day
	f.run(t, gh, "2026-11-02")
	if got := f.ids(t); len(got) != 2 {
		t.Fatalf("dropped before the third day: %v", got)
	}
	if _, err := os.Stat(f.report); err == nil {
		t.Fatal("a report was written before anything was ready")
	}
	f.run(t, gh, "2026-11-03")
	if got := f.ids(t); len(got) != 1 || got[0] != "alive/well" {
		t.Fatalf("after three days: %v, want only alive/well", got)
	}
	rep, err := os.ReadFile(f.report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gone/away", "has no releases left", "2026-11-01"} {
		if !strings.Contains(string(rep), want) {
			t.Errorf("the report does not say %q:\n%s", want, rep)
		}
	}
	for _, id := range asked {
		if id == "alive/well" {
			t.Error("an entry that passed today was asked about; index-check is the first witness")
		}
	}
}

// A deleted repository counts only on a NOT_FOUND for that very alias. A null with any
// other error (a permission problem, a GitHub bad day) is not evidence.
func TestDeadNeedsNotFoundNotJustNull(t *testing.T) {
	results := "deleted/repo\tfail\tx\nflaky/api\tfail\tx\n"
	f := newDeadFixture(t, results, "deleted/repo", "flaky/api")
	var asked []string
	gh := deadServer(t, map[string]upstream{"deleted/repo": notFound, "flaky/api": otherError}, &asked)
	for _, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03"} {
		f.run(t, gh, d)
	}
	if got := f.ids(t); len(got) != 1 || got[0] != "flaky/api" {
		t.Fatalf("got %v, want only flaky/api left", got)
	}
	if rep, _ := os.ReadFile(f.report); !strings.Contains(string(rep), "does not exist any more") {
		t.Errorf("the report should say the repository is gone:\n%s", rep)
	}
}

// An entry that fails while upstream still has releases is the renamed-asset case, or a
// broken release (dua-cli v3.0.1): a person's job, however many days it lasts.
func TestDeadLeavesAFailingEntryWithReleasesAlone(t *testing.T) {
	f := newDeadFixture(t, "renamed/asset\tfail\tentry: HTTP 404\n", "renamed/asset")
	var asked []string
	gh := deadServer(t, map[string]upstream{"renamed/asset": hasReleases}, &asked)
	for _, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03", "2026-11-04"} {
		f.run(t, gh, d)
	}
	if len(f.ids(t)) != 1 {
		t.Fatal("an entry whose upstream still has releases was dropped")
	}
	if b, _ := os.ReadFile(f.state); len(b) != 0 {
		t.Fatalf("it should not even be counted:\n%s", b)
	}
}

// Passing on one runner and failing on the other is alive, whatever GitHub says.
func TestDeadNeedsAFailAndNoPass(t *testing.T) {
	f := newDeadFixture(t, "", "half/way")
	other := filepath.Join(filepath.Dir(f.results), "other.tsv")
	os.WriteFile(f.results, []byte("half/way\tfail\tx\n"), 0o644)
	os.WriteFile(other, []byte("half/way\tpass\t\n"), 0o644)
	var asked []string
	gh := deadServer(t, map[string]upstream{"half/way": noReleases}, &asked)
	for _, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03"} {
		f.run(t, gh, d, func(c *deadConfig) { c.results = []string{f.results, other} })
	}
	if len(f.ids(t)) != 1 || len(asked) != 0 {
		t.Fatalf("an entry that passed somewhere was asked about (%v) or dropped", asked)
	}
}

// Days in a row: one day with releases back resets the count.
func TestDeadCountResetsWhenUpstreamComesBack(t *testing.T) {
	f := newDeadFixture(t, "on/off\tfail\tx\n", "on/off")
	repos := map[string]upstream{"on/off": noReleases}
	var asked []string
	gh := deadServer(t, repos, &asked)
	f.run(t, gh, "2026-11-01")
	f.run(t, gh, "2026-11-02")
	repos["on/off"] = hasReleases
	f.run(t, gh, "2026-11-03")
	repos["on/off"] = noReleases
	f.run(t, gh, "2026-11-04")
	f.run(t, gh, "2026-11-05")
	if len(f.ids(t)) != 1 {
		t.Fatal("dropped after two days, a day back, and two more: the count must restart")
	}
	b, _ := os.ReadFile(f.state)
	if !strings.Contains(string(b), "on/off\t2026-11-04\t2026-11-05\t2\t") {
		t.Fatalf("the state should have restarted on 11-04:\n%s", b)
	}
	f.run(t, gh, "2026-11-06")
	if len(f.ids(t)) != 0 {
		t.Fatal("three days in a row after the reset should drop it")
	}
}

// A signed entry is a person's call, and a source that is not GitHub cannot be asked.
func TestDeadNeverTouchesSignedOrNonGitHubEntries(t *testing.T) {
	dir := t.TempDir()
	f := deadFixture{idx: filepath.Join(dir, "idx.json"), state: filepath.Join(dir, "dead.tsv"),
		report: filepath.Join(dir, "pr.md"), results: filepath.Join(dir, "r.tsv")}
	signed := inlineEntry("signed/tool", "st")
	signed.Key, signed.Sig = strings.Repeat("ab", 32), "c2ln"
	gitlab := inlineEntry("lab/tool", "lt")
	gitlab.Source = "gitlab:lab/tool"
	writeIdx(t, f.idx, signed, gitlab)
	os.WriteFile(f.results, []byte("signed/tool\tfail\tx\nlab/tool\tfail\tx\n"), 0o644)
	var asked []string
	gh := deadServer(t, map[string]upstream{"signed/tool": notFound, "lab/tool": notFound}, &asked)
	for _, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03"} {
		f.run(t, gh, d)
	}
	if len(f.ids(t)) != 2 || len(asked) != 0 {
		t.Fatalf("asked %v; left %v", asked, f.ids(t))
	}
}

// Without --apply nothing leaves the index; the state still counts.
func TestDeadWithoutApplyOnlyCounts(t *testing.T) {
	f := newDeadFixture(t, "gone/away\tfail\tx\n", "gone/away")
	var asked []string
	gh := deadServer(t, map[string]upstream{"gone/away": notFound}, &asked)
	for _, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03"} {
		f.run(t, gh, d, func(c *deadConfig) { c.apply = false })
	}
	if len(f.ids(t)) != 1 {
		t.Fatal("dropped without --apply")
	}
	if _, err := os.Stat(f.report); err != nil {
		t.Fatalf("the report should still say what is ready: %v", err)
	}
}

// GitHub answers an expensive query with a 502 now and then; one is retried, not fatal.
func TestDeadRetriesABadGateway(t *testing.T) {
	old := deadRetryWait
	deadRetryWait = 0
	t.Cleanup(func() { deadRetryWait = old })
	f := newDeadFixture(t, "gone/away\tfail\tx\n", "gone/away")
	var asked []string
	inner := deadServer(t, map[string]upstream{"gone/away": noReleases}, &asked)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req, _ := http.NewRequest(r.Method, inner.base+r.URL.Path, r.Body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	f.run(t, newGitHub(srv.URL, ""), "2026-11-01")
	if calls != 2 {
		t.Fatalf("%d requests, want a 502 and one retry", calls)
	}
	if b, _ := os.ReadFile(f.state); !strings.Contains(string(b), "gone/away") {
		t.Fatalf("the retried answer was not counted:\n%s", b)
	}
}
