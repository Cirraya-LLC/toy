package index

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/ref"
)

func TestLoadRejectsWholeFile(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"not JSON", "this is not json"},
		{"empty input", ""},
		{"version is 2", `{"version":2,"packages":[]}`},
		{"no version at all", `{"packages":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := Load([]byte(tt.data), "default"); err == nil {
				t.Fatal("this index should have failed as a whole, but it loaded")
			}
		})
	}
}

// These entries are rejected before the source check even runs: the ID is
// malformed, or source is empty.
func TestLoadSkipsBadEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{"ID has no slash", Entry{ID: "tinyserve", Source: "github:x/tinyserve"}},
		{"ID has two slashes", Entry{ID: "a/b/c", Source: "github:a/b"}},
		{"ID owner is empty", Entry{ID: "/tinyserve", Source: "github:x/tinyserve"}},
		{"ID repo is empty", Entry{ID: "Derrity/", Source: "github:Derrity/x"}},
		{"ID is the empty string", Entry{ID: "", Source: "github:x/y"}},
		{"ID has an illegal character", Entry{ID: "Derr ity/tiny", Source: "github:x/y"}},
		{"ID is non-ASCII", Entry{ID: "德睿/小服务", Source: "github:x/y"}},
		{"source is empty", Entry{ID: "Derrity/tinyserve", Source: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(Index{Version: 1, Packages: []Entry{tt.entry}})
			if err != nil {
				t.Fatal(err)
			}
			idx, skipped, err := Load(data, "default")
			if err != nil {
				t.Fatalf("one rotten record must not fail the whole index: %v", err)
			}
			if skipped != 1 {
				t.Errorf("skipped = %d, want 1", skipped)
			}
			if len(idx.Packages) != 0 {
				t.Errorf("bad entry was kept: %+v", idx.Packages)
			}
			if idx.Name != "default" {
				t.Errorf("Name = %q, want %q", idx.Name, "default")
			}
		})
	}
}

func TestLoadEmptyPackages(t *testing.T) {
	idx, skipped, err := Load([]byte(`{"version":1,"packages":[]}`), "empty index")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(idx.Packages) != 0 {
		t.Fatalf("skipped=%d packages=%d, want 0/0", skipped, len(idx.Packages))
	}
	if idx.Version != 1 {
		t.Errorf("Version = %d, want 1", idx.Version)
	}
}

func TestLoadKeepsGoodEntriesAndCountsSkips(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"Derrity/tinyserve","source":"github:Derrity/tinyserve","desc":"good one"},
		{"id":"noslash","source":"github:a/b","desc":"bad ID"},
		{"id":"Derrity/other","source":"","desc":"empty source"},
		{"id":"Other/ok","source":"github:Other/ok","desc":"also good"}
	]}`)
	idx, skipped, err := Load(data, "default")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if len(idx.Packages) != 2 {
		t.Fatalf("kept %d entries, want 2: %+v", len(idx.Packages), idx.Packages)
	}
	if _, ok := idx.Find("Derrity/tinyserve"); !ok {
		t.Error("the good entry Derrity/tinyserve went missing")
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"Derrity/tinyserve", true},
		{"a/b", true},
		{"a.b_c-d/e.f_g-h", true},
		{"UPPER/Case9", true},
		{"", false},
		{"tinyserve", false},
		{"a/b/c", false},
		{"/b", false},
		{"a/", false},
		{"a b/c", false},
		{"a/b c", false},
		{"a@b/c", false},
		{"德睿/工具", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := validID(tt.id); got != tt.want {
				t.Errorf("validID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// Every tier gets pinned separately; their order must not change.
func TestScoreTiers(t *testing.T) {
	e := Entry{
		ID:   "Derrity/tinyserve",
		Desc: "a very small static file host over HTTP",
		Tags: []string{"web", "server"},
	}
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"exact ID", "derrity/tinyserve", scoreExactID},
		{"exact ID ignoring case", "DERRITY/TINYSERVE", scoreExactID},
		{"ID prefix", "derrity/tiny", scoreIDPrefix},
		{"ID prefix inside the owner", "derr", scoreIDPrefix},
		{"name substring", "tinyserv", scoreName},
		{"name substring in the middle", "nyser", scoreName},
		{"description substring", "static file", scoreDesc},
		{"description substring ignoring case", "http", scoreDesc},
		{"tag hit", "web", scoreTag},
		{"tag hit ignoring case", "SERVER", scoreTag},
		{"nothing matches", "postgres", 0},
		{"empty query", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scoreOf(e, strings.ToLower(tt.query)); got != tt.want {
				t.Errorf("scoreOf(%q) = %d, want %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestScoreTakesTheHighestTier(t *testing.T) {
	// This record hits "tiny" on the ID prefix, the name, the desc and a tag.
	// Only the highest tier may count.
	e := Entry{ID: "tiny/tinyserve", Desc: "tiny", Tags: []string{"tiny"}}
	if got := scoreOf(e, "tiny"); got != scoreIDPrefix {
		t.Errorf("scoreOf = %d, want %d (the ID prefix tier)", got, scoreIDPrefix)
	}
}

func idsOf(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Entry.ID
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSearch(t *testing.T) {
	main := &Index{Version: 1, Name: "default", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "static file server", Tags: []string{"web"}, Installs: 5},
		{ID: "Derrity/tinytool", Desc: "another tool", Installs: 50},
		{ID: "Other/serve", Desc: "mentions tinyserve in the description", Installs: 100},
		{ID: "Third/thing", Desc: "completely unrelated", Tags: []string{"tinyserve"}, Installs: 1000},
	}}

	tests := []struct {
		name  string
		query string
		limit int
		want  []string
	}{
		{
			name:  "tier ordering",
			query: "tinyserve",
			// no exact match, no ID prefix; name substring > desc > tag
			want: []string{"Derrity/tinyserve", "Other/serve", "Third/thing"},
		},
		{
			name:  "ties (both ID prefixes) break on Installs descending",
			query: "derrity/tiny",
			want:  []string{"Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "name > desc > tag, and equal names break on Installs",
			query: "tiny",
			want: []string{
				"Derrity/tinytool",  // name 60, 50 installs
				"Derrity/tinyserve", // name 60, 5 installs
				"Other/serve",       // desc 40
				"Third/thing",       // tag 20; 1000 installs still cannot beat the tier
			},
		},
		{
			name:  "empty query returns everything by Installs descending",
			query: "",
			want:  []string{"Third/thing", "Other/serve", "Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "whitespace only also counts as an empty query",
			query: "   ",
			want:  []string{"Third/thing", "Other/serve", "Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "limit truncates",
			query: "tinyserve",
			limit: 2,
			want:  []string{"Derrity/tinyserve", "Other/serve"},
		},
		{
			name:  "limit 0 means no limit",
			query: "tinyserve",
			limit: 0,
			want:  []string{"Derrity/tinyserve", "Other/serve", "Third/thing"},
		},
		{
			name:  "nothing matches at all",
			query: "postgres",
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idsOf(Search([]*Index{main}, tt.query, tt.limit))
			if !eq(got, tt.want) {
				t.Errorf("Search(%q, limit=%d) = %v, want %v", tt.query, tt.limit, got, tt.want)
			}
		})
	}
}

func TestSearchTierBeatsInstalls(t *testing.T) {
	idx := &Index{Name: "default", Packages: []Entry{
		{ID: "Big/tiny-thing", Installs: 999999}, // name substring
		{ID: "tiny/thing", Installs: 1},          // ID prefix, a higher tier
	}}
	got := idsOf(Search([]*Index{idx}, "tiny", 0))
	want := []string{"tiny/thing", "Big/tiny-thing"}
	if !eq(got, want) {
		t.Errorf("ID prefix must beat a name substring even at a million-to-one install ratio, got %v", got)
	}
}

func TestSearchExactIDWins(t *testing.T) {
	idx := &Index{Name: "default", Packages: []Entry{
		// a 1000x install lead still cannot beat an exact match
		{ID: "Other/tinyserve-plus", Desc: "tinyserve", Installs: 100000},
		{ID: "Derrity/tinyserve", Installs: 1},
	}}
	got := idsOf(Search([]*Index{idx}, "Derrity/tinyserve", 0))
	if len(got) == 0 || got[0] != "Derrity/tinyserve" {
		t.Fatalf("the exact match must come first, got %v", got)
	}
}

func TestSearchDedupKeepsTheEarlierIndex(t *testing.T) {
	first := &Index{Name: "official", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "the official one", Installs: 1},
	}}
	second := &Index{Name: "mine", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "the one I forked", Installs: 9999},
		{ID: "Me/only-here", Desc: "tinyserve related"},
	}}

	got := Search([]*Index{first, second}, "tinyserve", 0)
	seen := 0
	for _, r := range got {
		if r.Entry.ID != "Derrity/tinyserve" {
			continue
		}
		seen++
		if r.Index != "official" {
			t.Errorf("a duplicate ID must keep the earlier index's copy, got one from %q", r.Index)
		}
		if r.Entry.Desc != "the official one" {
			t.Errorf("the later record was the one kept: %q", r.Entry.Desc)
		}
	}
	if seen != 1 {
		t.Errorf("the same ID showed up %d times, want 1", seen)
	}
	if len(got) != 2 {
		t.Errorf("got %d results, want 2 (after dedup)", len(got))
	}
}

func TestSearchIgnoresNilIndex(t *testing.T) {
	idx := &Index{Name: "default", Packages: []Entry{{ID: "a/b", Desc: "x"}}}
	if got := Search([]*Index{nil, idx, nil}, "", 0); len(got) != 1 {
		t.Fatalf("nil indexes must be skipped, got %d results", len(got))
	}
	if got := Search(nil, "tiny", 0); len(got) != 0 {
		t.Fatalf("no indexes must mean no results, got %v", got)
	}
}

func TestUpsertKeepsSorted(t *testing.T) {
	idx := &Index{Version: 1}
	for _, id := range []string{"Zed/z", "Alpha/a", "Mid/m", "Beta/b"} {
		idx.Upsert(Entry{ID: id, Source: "github:" + id})
	}
	want := []string{"Alpha/a", "Beta/b", "Mid/m", "Zed/z"}
	var got []string
	for _, e := range idx.Packages {
		got = append(got, e.ID)
	}
	if !eq(got, want) {
		t.Fatalf("order after Upsert = %v, want %v", got, want)
	}
}

func TestUpsertReplacesInPlace(t *testing.T) {
	idx := &Index{Version: 1}
	idx.Upsert(Entry{ID: "a/b", Desc: "old", Installs: 1})
	idx.Upsert(Entry{ID: "c/d", Desc: "another"})
	idx.Upsert(Entry{ID: "a/b", Desc: "new", Installs: 7})

	if len(idx.Packages) != 2 {
		t.Fatalf("the same ID got inserted twice: %+v", idx.Packages)
	}
	e, ok := idx.Find("a/b")
	if !ok {
		t.Fatal("a/b went missing")
	}
	if e.Desc != "new" || e.Installs != 7 {
		t.Errorf("it was not replaced: %+v", e)
	}
	if idx.Packages[0].ID != "a/b" {
		t.Errorf("the order broke after the replacement: %+v", idx.Packages)
	}
}

func TestFindAndRemove(t *testing.T) {
	idx := &Index{Version: 1, Packages: []Entry{
		{ID: "a/b", Desc: "one"},
		{ID: "c/d", Desc: "two"},
	}}
	if _, ok := idx.Find("a/b"); !ok {
		t.Error("Find did not turn up a/b")
	}
	if _, ok := idx.Find("nope/nope"); ok {
		t.Error("Find turned up an entry that does not exist")
	}
	if !idx.Remove("a/b") {
		t.Error("Remove should have returned true")
	}
	if idx.Remove("a/b") {
		t.Error("a repeated Remove should return false")
	}
	if len(idx.Packages) != 1 || idx.Packages[0].ID != "c/d" {
		t.Errorf("Remove deleted the wrong entry: %+v", idx.Packages)
	}
}

func TestMarshal(t *testing.T) {
	idx := &Index{Version: 1, Name: "local-only-name"}
	b, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "null") {
		t.Errorf("empty packages must be written as [], not null: %s", s)
	}
	if strings.Contains(s, "local-only-name") {
		t.Errorf("Name is local and must not reach the file: %s", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("the index file must end in a newline so diffs stay clean")
	}
	if !strings.Contains(s, "\n  ") {
		t.Errorf("the index file must be indented so diffs stay readable: %s", s)
	}

	idx.Upsert(Entry{ID: "z/z", Source: "github:z/z", Desc: "later"})
	idx.Upsert(Entry{ID: "a/a", Source: "github:a/a", Desc: "earlier"})
	b, err = idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(string(b), `"a/a"`), strings.Index(string(b), `"z/z"`); i < 0 || j < 0 || i > j {
		t.Errorf("the file must be sorted by ID: %s", b)
	}

	// Whatever we serialize has to read back in (this part stays at the JSON
	// layer and does not touch ref).
	var back Index
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("we cannot read back the file we wrote ourselves: %v", err)
	}
	if back.Version != 1 || len(back.Packages) != 2 {
		t.Errorf("what came back is wrong: %+v", back)
	}
}

func TestMarshalFillsMissingVersion(t *testing.T) {
	b, err := (&Index{}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 1`) {
		t.Errorf("a missing version must be filled in as 1: %s", b)
	}
}

// The skeleton comment says "source must parse with ref.Parse". Beyond an empty
// source, a genuinely malformed one has to be rejected too — this tier had no
// cases at all before.
func TestLoadSkipsUnparsableSource(t *testing.T) {
	for _, src := range []string{
		"not a ref",
		"github:a b/c",     // space inside the owner
		"github:../x",      // path traversal that would become a directory name
		"ftp://x/toy.toml", // scheme we do not accept
		"a/b/c",
		"   ",
	} {
		t.Run(src, func(t *testing.T) {
			data, err := json.Marshal(Index{Version: 1, Packages: []Entry{{ID: "a/b", Source: src}}})
			if err != nil {
				t.Fatal(err)
			}
			idx, skipped, err := Load(data, "default")
			if err != nil {
				t.Fatal(err)
			}
			if skipped != 1 || len(idx.Packages) != 0 {
				t.Errorf("source %q should have been skipped: skipped=%d kept %+v", src, skipped, idx.Packages)
			}
		})
	}
}

// source is this record's truth (spec §5) and must point at some host or some
// URL. A bare owner/repo means "look it up in the index" — resolution loops back
// to the index, which is a cycle.
func TestLoadSkipsSourceThatPointsBackAtTheIndex(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"a/b","source":"a/b"},
		{"id":"c/d","source":"c/d@v1.0.0"},
		{"id":"e/f","source":"github:e/f"}
	]}`)
	idx, skipped, err := Load(data, "default")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (two sources point back at the index)", skipped)
	}
	if len(idx.Packages) != 1 || idx.Packages[0].ID != "e/f" {
		t.Errorf("only the github: entry should survive: %+v", idx.Packages)
	}
}

// Two records under one ID make Find and Remove lie: Remove returns true and the
// ID is still there.
func TestLoadSkipsDuplicateIDs(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"a/b","source":"github:a/b","desc":"the real one"},
		{"id":"a/b","source":"github:evil/x","desc":"the fake one"},
		{"id":"c/d","source":"github:c/d"}
	]}`)
	idx, skipped, err := Load(data, "default")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the second a/b)", skipped)
	}
	if e, _ := idx.Find("a/b"); e.Desc != "the real one" {
		t.Errorf("the first record should be the one kept: %+v", e)
	}
	if !idx.Remove("a/b") {
		t.Fatal("Remove should have returned true")
	}
	if _, ok := idx.Find("a/b"); ok {
		t.Error("Remove claimed it deleted a/b and a/b is still there — duplicate IDs in the index")
	}
}

// validID's rules must match ref's: these two segments become directory names
// under ~/.toy (spec §6).
func TestValidIDMatchesRefsSegmentRules(t *testing.T) {
	long := strings.Repeat("a", 120)
	for _, id := range []string{"../x", "a/..", "./x", "a/.", long + "/x", "x/" + long, " a/b", "a/b ", "a/b@v1"} {
		if validID(id) {
			t.Errorf("validID(%q) = true, but ref would reject it", id)
		}
		if _, err := ref.Parse(id); err == nil && id != " a/b" && id != "a/b " && id != "a/b@v1" {
			t.Errorf("the premise changed: ref.Parse(%q) no longer errors", id)
		}
	}
}

// A later index must not be able to swap an earlier index's package for its own
// source: "the earlier one in the config wins" means an ID that appears at all
// claims the slot, not an ID that happens to match this query.
func TestSearchLaterIndexCannotShadowAnEarlierID(t *testing.T) {
	official := &Index{Name: "official", Packages: []Entry{
		{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "static file server"},
	}}
	evil := &Index{Name: "attacker", Packages: []Entry{
		// the desc is written to graze "web" on purpose, while the official one does not
		{ID: "Derrity/tinyserve", Source: "github:evil/backdoor", Desc: "web server", Installs: 9999},
	}}
	for _, r := range Search([]*Index{official, evil}, "web", 0) {
		if r.Entry.ID == "Derrity/tinyserve" && r.Index != "official" {
			t.Fatalf("a later index shadowed the same ID: it came from %q, source=%s", r.Index, r.Entry.Source)
		}
	}
}

// An index we write ourselves must Load back in (not merely unmarshal).
func TestMarshalRoundTripsThroughLoad(t *testing.T) {
	idx := &Index{Name: "local name"}
	idx.Upsert(Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve",
		Desc: "static file server", Tags: []string{"web"}, Key: "a3f9", Installs: 7})
	idx.Upsert(Entry{ID: "Other/ok", Source: "https://example.com/toy.toml", Desc: "a bare URL works too"})

	b, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, skipped, err := Load(b, "loaded back")
	if err != nil {
		t.Fatalf("we cannot Load the index we wrote ourselves: %v", err)
	}
	if skipped != 0 {
		t.Errorf("%d of our own entries were judged bad by our own loader", skipped)
	}
	if len(back.Packages) != 2 {
		t.Fatalf("loaded %d entries, want 2: %+v", len(back.Packages), back.Packages)
	}
	e, ok := back.Find("Derrity/tinyserve")
	if !ok || e.Installs != 7 || e.Key != "a3f9" || len(e.Tags) != 1 {
		t.Errorf("fields were lost in the round trip: %+v", e)
	}
	if back.Name != "loaded back" {
		t.Errorf("Name must come from the argument, not the file: %q", back.Name)
	}
}

// GitHub owners and repos are case-insensitive to begin with, so
// `toy install derrity/tinyserve` has to find Derrity/tinyserve in the index.
// This used to be an exact string match, and the user got "no such package"
// while the package was sitting right there in the index.
func TestFindIsCaseInsensitive(t *testing.T) {
	idx := &Index{Version: 1, Packages: []Entry{
		{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve"},
	}}
	for _, q := range []string{"Derrity/tinyserve", "derrity/tinyserve", "DERRITY/TINYSERVE", "derrity/TinyServe"} {
		e, ok := idx.Find(q)
		if !ok {
			t.Fatalf("Find(%q) found nothing", q)
		}
		// What comes back must be the record exactly as the index holds it: the
		// download URL is built from its casing.
		if e.Source != "github:Derrity/tinyserve" {
			t.Fatalf("Find(%q) returned %+v — source casing must be preserved verbatim", q, e)
		}
	}
	if !idx.Remove("DERRITY/tinyserve") {
		t.Fatal("Remove must be case-insensitive too, or it cannot delete what Find just returned")
	}
}

// Upsert has to compare the same way: otherwise publishing an ID with different
// casing inserts a second record, which Load will then drop as a duplicate next
// time ("I submitted it and nothing happened").
func TestUpsertIsCaseInsensitive(t *testing.T) {
	idx := &Index{Version: 1, Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "old"},
	}}
	idx.Upsert(Entry{ID: "derrity/tinyserve", Desc: "new"})
	if len(idx.Packages) != 1 {
		t.Fatalf("a second record was inserted: %+v", idx.Packages)
	}
	if idx.Packages[0].Desc != "new" || idx.Packages[0].ID != "derrity/tinyserve" {
		t.Fatalf("the whole record should have been replaced: %+v", idx.Packages[0])
	}
}

// Same for dedup: a later index must not be able to swap an ID's source for its
// own by flipping the case of one letter.
func TestSearchDedupIsCaseInsensitive(t *testing.T) {
	first := &Index{Version: 1, Name: "official", Packages: []Entry{
		{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve"},
	}}
	second := &Index{Version: 1, Name: "rogue", Packages: []Entry{
		{ID: "derrity/tinyserve", Source: "github:evil/x"},
	}}
	got := Search([]*Index{first, second}, "tinyserve", 0)
	if len(got) != 1 {
		t.Fatalf("only one result should remain, got %d: %+v", len(got), got)
	}
	if got[0].Index != "official" {
		t.Fatalf("the earlier index should win, got one from %q", got[0].Index)
	}
}

// Two entries in one index differing only by case are duplicates too: keeping
// both means Find only ever sees the first, and Remove deletes the first while
// the second is still findable ("removed" would be a lie).
func TestLoadSkipsCaseVariantDuplicateIDs(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"Derrity/tinyserve","source":"github:Derrity/tinyserve","desc":"the real one"},
		{"id":"derrity/tinyserve","source":"github:evil/x","desc":"the fake one"}
	]}`)
	idx, skipped, err := Load(data, "default")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if e, _ := idx.Find("derrity/tinyserve"); e.Desc != "the real one" {
		t.Fatalf("the first record should be the one kept: %+v", e)
	}
}
