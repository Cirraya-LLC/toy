package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
)

type hit struct {
	name    string
	stars   int
	licence any // nil = no licence file; "NOASSERTION" = one GitHub cannot read
}

// discoverServer answers the search per topic and everything else from fakeHandler.
func discoverServer(t *testing.T, byTopic map[string][]hit, repos map[string]fakeRepo) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "archived:false") || !strings.Contains(q, "stars:>=") {
			t.Errorf("the search should filter archived repos and a star floor: %q", q)
		}
		topic := strings.TrimPrefix(strings.Fields(q)[0], "topic:")
		var items []map[string]any
		for _, h := range byTopic[topic] {
			var lic any
			if h.licence != nil {
				lic = map[string]any{"spdx_id": h.licence}
			}
			items = append(items, map[string]any{
				"full_name": h.name, "stargazers_count": h.stars, "description": "d",
				"pushed_at": time.Now().Format(time.RFC3339), "license": lic,
			})
		}
		w.Header().Set("X-RateLimit-Remaining", "29")
		json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "items": items})
	})
	mux.Handle("/", fakeHandler(repos))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverKeepsOnlyWhatGenWouldList(t *testing.T) {
	noRel := tinyserve
	noRel.noRelease = true
	srcOnly := tinyserve
	srcOnly.assets = []string{"source.tar.gz"}
	repos := map[string]fakeRepo{
		"good/one": tinyserve, "good/two": tinyserve, "good/three": tinyserve,
		"in/index": tinyserve, "gone/away": tinyserve, "no/thanks": tinyserve,
		"no/release": noRel, "source/only": srcOnly,
	}
	byTopic := map[string][]hit{
		"cli": {
			{"good/one", 900, "MIT"}, {"in/index", 5000, "MIT"}, {"gone/away", 4000, "MIT"},
			{"no/licence", 3000, nil}, {"read/me", 2500, "NOASSERTION"}, {"source/available", 2000, "BUSL-1.1"},
		},
		"tui": {
			{"good/one", 900, "MIT"}, // the same repo under a second topic counts once
			{"good/two", 1500, "Apache-2.0"}, {"no/release", 1200, "MIT"},
			{"source/only", 1100, "MIT"}, {"no/thanks", 1000, "MIT"}, {"good/three", 800, "MIT"},
		},
	}
	srv := discoverServer(t, byTopic, repos)
	dir := t.TempDir()
	out, review := filepath.Join(dir, "cands.txt"), filepath.Join(dir, "review.txt")
	idx := &index.Index{Version: index.Version}
	idx.Upsert(index.Entry{ID: "in/index", Source: "github:in/index", Desc: "d"})

	err := discover(t.Context(), discoverConfig{
		topics: []string{"cli", "tui"}, minStars: 500, since: time.Now().AddDate(-2, 0, 0), max: 2,
		idx: idx, gh: newGitHub(srv.URL, "t"), out: out, review: review,
		host:  host.Host{Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}"},
		lists: lists{delisted: map[string]bool{"gone/away": true}, declined: map[string]bool{"no/thanks": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readRepoList(out)
	// Most stars first, and the cap of two stops before good/three.
	if strings.Join(got, ",") != "good/two,good/one" {
		t.Fatalf("accepted %v, want [good/two good/one]", got)
	}
	rev, _ := readRepoList(review)
	if strings.Join(rev, ",") != "read/me" {
		t.Fatalf("for a person to read: %v, want [read/me]", rev)
	}
}

func TestDiscoverNeedsAToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	err := runDiscover(t.Context(), []string{"--out", filepath.Join(t.TempDir(), "x")})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("want a clear 'needs a token' error, got %v", err)
	}
}
