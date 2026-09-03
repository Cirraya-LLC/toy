// Package index reads, writes and searches the index. Spec §5: the index stores
// pointers, not substance.
package index

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// Version is the index format version this client understands — the "version": 1
// in the JSON from spec §5.
const Version = 1

type Entry struct {
	ID     string   `json:"id"`     // "Derrity/tinyserve"
	Source string   `json:"source"` // "github:Derrity/tinyserve" — this is always the truth
	Desc   string   `json:"desc"`
	Tags   []string `json:"tags,omitempty"`
	Key    string   `json:"key,omitempty"` // author's public key, hex; only once the name is claimed
	// Sig is the author's signature (base64) over id + source + the raw toy.toml,
	// produced by `toy publish`. A KEY WITHOUT A SIG IS NOT A SIGNATURE: anyone can
	// fork the index and anyone can open a PR, so copying someone's public key in
	// costs nothing and proves nothing. The client only prints "✓ signed" after it
	// has verified this signature (rule 8).
	Sig      string `json:"sig,omitempty"`
	Stars    int    `json:"stars,omitempty"`
	Installs int    `json:"installs,omitempty"`

	// Toyfile is the raw toy.toml supplied by the index for upstreams that do NOT
	// have one of their own. It is the only way out of the index's cold start:
	// none of the thousands of CLI tools that exist today ship a toy.toml, so a
	// pointer-only index produces nothing but entries that cannot be installed.
	//
	// Four boundaries. Drop any one of them and this design falls apart:
	//   1. Upstream always wins. This copy is used only when fetching toy.toml over
	//      the raw URL says it definitively does not exist, and it steps aside
	//      unconditionally the moment upstream ships its own. Otherwise one
	//      malicious entry could override what the author declared — that is
	//      hijacking, not packaging.
	//   2. It can NEVER reach TrustSigned. A signature covers what the person who
	//      submitted this record wrote; that person is not the author, and putting
	//      a checkmark on it is exactly the overclaim we keep avoiding (§14.3).
	//      When an entry carries both a toyfile and a key/sig, the signature only
	//      proves this record was not tampered with — the tier stays
	//      store.TrustIndexed.
	//   3. It is validated exactly as strictly as upstream's (validEntry below
	//      calls toyfile.ParseFrom). "It came from our own index" is not a reason
	//      to take a looser path — the index is precisely the place anyone can fork
	//      and anyone can send a PR to.
	//   4. An asset name is still just an asset name: the download URL is still
	//      built from the host template plus the tag. So the blast radius is
	//      pinned between "some asset in the author's own release" and "a command
	//      name on PATH", and store's reserved names plus the shadowing check
	//      already cover the latter. THIS IS WHY THE DESIGN HOLDS: the index can
	//      change WHICH artifact you install, never WHERE it comes from.
	Toyfile string `json:"toyfile,omitempty"`
}

type Index struct {
	Version  int     `json:"version"`
	Name     string  `json:"-"` // local name from the config; never written to the file
	Packages []Entry `json:"packages"`
}

// Load parses index JSON and validates it: version must be 1, id must be in
// owner/repo form, and source must parse with ref.Parse. Bad entries are skipped
// rather than failing the whole index — one rotten record must not take the whole
// search down. Returns the number of entries skipped.
//
// "Bad" also covers two more cases: a source that parses back into index form
// (see validEntry), and the second and later entries sharing one ID (see the
// comment in the loop).
func Load(data []byte, name string) (*Index, int, error) {
	var raw Index
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, fmt.Errorf("index %s: invalid JSON: %w", name, err)
	}
	if raw.Version != Version {
		return nil, 0, fmt.Errorf("index %s: format version %d, but this client only understands version %d — upgrade toy or use another index",
			name, raw.Version, Version)
	}

	out := &Index{Version: raw.Version, Name: name, Packages: make([]Entry, 0, len(raw.Packages))}
	seen := make(map[string]bool, len(raw.Packages))
	skipped := 0
	for _, e := range raw.Packages {
		if !validEntry(e) {
			skipped++
			continue
		}
		// An index with the same ID twice is ambiguous: Find only ever sees the
		// first one, and Remove deletes the first and still returns true while the
		// ID is still in the index ("removed" would be a lie). Everything from the
		// second occurrence on is treated as a bad record and counted.
		// Compared by foldID: Derrity/tinyserve and derrity/tinyserve are the same
		// repo, and keeping both preserves that exact ambiguity.
		if seen[foldID(e.ID)] {
			skipped++
			continue
		}
		seen[foldID(e.ID)] = true
		out.Packages = append(out.Packages, e)
	}
	return out, skipped, nil
}

// validEntry holds every criterion for "is this record usable".
func validEntry(e Entry) bool {
	if !validID(e.ID) || e.Source == "" {
		return false
	}
	r, err := ref.Parse(e.Source)
	if err != nil {
		return false
	}
	// source is this record's truth (spec §5), so it has to point somewhere we can
	// actually fetch from. A bare owner/repo would send resolution back to the
	// index for another lookup — that is a cycle, not a source.
	if r.Kind == ref.KindIndex {
		return false
	}
	// The index itself is REMOTE CONTENT, and this line decides what the client
	// opens next. Allowing file:// lets any index (anyone can fork it, anyone can
	// send a PR) direct the client to read the local ~/.aws/credentials; allowing
	// plaintext http lets through addresses like 169.254.169.254 that only mean
	// something on the victim's own machine, and anyone on the path can rewrite
	// the response. The hosted path (github: and friends) is unaffected — those
	// URLs come from our own host table, not from whatever the index says.
	if r.Kind == ref.KindURL && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
		return false
	}
	// An inline toyfile is fully parsed right here, and a bad one is a bad record
	// on the spot. Letting it blow up at install time means the user is already
	// waiting on a download, and the error reads like a problem with the upstream
	// repo — when the thing that is actually wrong is the index. This uses the
	// very same function as the upstream path, with nothing relaxed
	// (Entry.Toyfile, boundary 3).
	if e.Toyfile != "" {
		if _, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex); err != nil {
			return false
		}
	}
	return true
}

// validID checks for "owner/repo" form. The rules are borrowed from ref rather
// than copied into a second allowlist here: these two segments end up as
// ~/.toy/pkg/<host>/<owner>/<name>/ (spec §6), and ref already rejects "." ".."
// and over-long segments there. A copy drifts — the previous version of this
// function really did let through "../x" and "a/..".
func validID(id string) bool {
	r, err := ref.Parse(id)
	// r.ID() == id also nails down two things: no @version, and no leading or
	// trailing whitespace (ref.Parse trims it, and an ID with whitespace would
	// make Find and dedup disagree).
	return err == nil && r.Kind == ref.KindIndex && r.ID() == id
}

func (i *Index) Marshal() ([]byte, error) {
	out := *i
	if out.Version == 0 {
		out.Version = Version
	}
	if out.Packages == nil {
		out.Packages = []Entry{} // an empty index writes as [], not null
	}
	// Indented, with a trailing newline: this file goes into git, so the diff has
	// to be readable.
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("index %s: cannot serialize: %w", i.Name, err)
	}
	return append(b, '\n'), nil
}

// foldID is the comparison form of an ID. GitHub and GitLab owners and repos are
// case-insensitive to begin with, so `toy install derrity/tinyserve` has to find
// Derrity/tinyserve in the index — otherwise the user gets "no such package"
// while the package is sitting right there.
//
// The folding happens only on the comparison side: ref must keep the original
// case, because raw URLs and release download URLs are built verbatim and
// lowercasing them builds a 404.
func foldID(id string) string { return strings.ToLower(id) }

// Find looks an entry up by ID, case-insensitively (see foldID).
func (i *Index) Find(id string) (Entry, bool) {
	want := foldID(id)
	for _, e := range i.Packages {
		if foldID(e.ID) == want {
			return e, true
		}
	}
	return Entry{}, false
}

// Upsert inserts or replaces by ID, keeping Packages sorted by ID so the index
// file's diff stays readable.
func (i *Index) Upsert(e Entry) {
	for n := range i.Packages {
		if foldID(i.Packages[n].ID) == foldID(e.ID) {
			i.Packages[n] = e
			return // replaced in place: position unchanged, so the sort holds
		}
	}
	i.Packages = append(i.Packages, e)
	sort.Slice(i.Packages, func(a, b int) bool { return i.Packages[a].ID < i.Packages[b].ID })
}

func (i *Index) Remove(id string) bool {
	want := foldID(id)
	for n, e := range i.Packages {
		if foldID(e.ID) == want {
			i.Packages = append(i.Packages[:n], i.Packages[n+1:]...)
			return true
		}
	}
	return false
}

type Result struct {
	Entry Entry
	Index string // which index it came from
	Score int
}

// Ranking tiers. The numbers mean nothing on their own; only their order does.
const (
	scoreExactID  = 100 // exact ID match
	scoreIDPrefix = 80  // ID prefix
	scoreName     = 60  // substring of the name (the part after the slash)
	scoreDesc     = 40  // substring of the description
	scoreTag      = 20  // tag hit
)

// Search runs across several indexes. Ranking: exact ID > ID prefix > name
// substring > description substring > tag hit, ties broken by Installs
// descending. Case-insensitive. An empty query returns everything (by Installs
// descending). When one ID appears in several indexes only the first index's copy
// is kept — the one listed earlier in the config wins.
func Search(idxs []*Index, query string, limit int) []Result {
	q := strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]bool)

	var out []Result
	for _, idx := range idxs {
		if idx == nil {
			continue
		}
		for _, e := range idx.Packages {
			if seen[foldID(e.ID)] {
				continue // an earlier index already claimed this ID
			}
			// Claim the ID BEFORE scoring: appearing at all counts, whether or not
			// this query matched. Claiming after would let a later index write a
			// desc/tag that grazes a query the earlier index's entry misses, and
			// swap that ID's source out for its own.
			seen[foldID(e.ID)] = true
			score := scoreOf(e, q)
			if q != "" && score == 0 {
				continue
			}
			out = append(out, Result{Entry: e, Index: idx.Name, Score: score})
		}
	}

	// Stable sort: on a full tie, keep "index order, then order within the index",
	// which is what makes results reproducible.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Entry.Installs > out[b].Entry.Installs
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scoreOf returns the highest tier that hits. q is already lowercased and
// trimmed; an empty q always scores 0.
func scoreOf(e Entry, q string) int {
	if q == "" {
		return 0
	}
	id := strings.ToLower(e.ID)
	switch {
	case id == q:
		return scoreExactID
	case strings.HasPrefix(id, q):
		return scoreIDPrefix
	}
	name := id
	if _, repo, ok := strings.Cut(id, "/"); ok {
		name = repo
	}
	if strings.Contains(name, q) {
		return scoreName
	}
	if strings.Contains(strings.ToLower(e.Desc), q) {
		return scoreDesc
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return scoreTag
		}
	}
	return 0
}
