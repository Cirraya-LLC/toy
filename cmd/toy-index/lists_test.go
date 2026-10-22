package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
)

func TestLinkedRepos(t *testing.T) {
	md := `# Awesome CLI
- [ripgrep](https://github.com/BurntSushi/ripgrep) - search. Also https://github.com/BurntSushi/ripgrep/releases.
- [fd](https://github.com/sharkdp/fd.git), [bat](http://github.com/sharkdp/bat).
- Sponsor: https://github.com/sponsors/someone and a topic https://github.com/topics/cli
- This list itself: https://github.com/me/awesome-list
`
	got := strings.Join(linkedRepos(md, "me/awesome-list"), ",")
	if got != "BurntSushi/ripgrep,sharkdp/fd,sharkdp/bat" {
		t.Fatalf("got %s", got)
	}
}

type metaRepo struct {
	stars           int
	created, pushed time.Time
	archived, fork  bool
	licence         any // nil = no licence file
	gone            bool
}

var aliasRe = regexp.MustCompile(`r(\d+): repository\(owner: "([^"]+)", name: "([^"]+)"\)`)

// listServer: a README for each list, GraphQL metadata, and the REST API gen needs.
func listServer(t *testing.T, readmes map[string]string, meta map[string]metaRepo, repos map[string]fakeRepo) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q struct{ Query string }
		json.Unmarshal(body, &q)
		data := map[string]any{}
		for _, m := range aliasRe.FindAllStringSubmatch(q.Query, -1) {
			id := m[2] + "/" + m[3]
			mr, ok := meta[id]
			if !ok || mr.gone {
				data["r"+m[1]] = nil
				continue
			}
			var lic any
			if mr.licence != nil {
				lic = map[string]any{"spdxId": mr.licence}
			}
			data["r"+m[1]] = map[string]any{
				"nameWithOwner": id, "description": "d", "stargazerCount": mr.stars,
				"isArchived": mr.archived, "isFork": mr.fork, "isMirror": false,
				"createdAt": mr.created.Format(time.RFC3339), "pushedAt": mr.pushed.Format(time.RFC3339),
				"licenseInfo": lic,
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": []any{map[string]any{"message": "some gone"}}})
	})
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	fake := fakeHandler(repos)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if list, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/readme"); ok {
			md, found := readmes[list]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(md))})
			return
		}
		fake.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A list is the second source: its links go through the same floors as a topic search
// (stars, age, recent push, licence) and the same gen check.
func TestDiscoverReadsCuratedLists(t *testing.T) {
	now := time.Now()
	old, recent := now.AddDate(-3, 0, 0), now.AddDate(0, -1, 0)
	md := "- https://github.com/good/tool\n- https://github.com/few/stars\n- https://github.com/too/new\n" +
		"- https://github.com/long/dead\n- https://github.com/no/licence\n- https://github.com/gone/away\n" +
		"- https://github.com/is/archived\n"
	meta := map[string]metaRepo{
		"good/tool":   {stars: 5000, created: old, pushed: recent, licence: "MIT"},
		"few/stars":   {stars: 10, created: old, pushed: recent, licence: "MIT"},
		"too/new":     {stars: 5000, created: recent, pushed: recent, licence: "MIT"},
		"long/dead":   {stars: 5000, created: old, pushed: now.AddDate(-5, 0, 0), licence: "MIT"},
		"no/licence":  {stars: 5000, created: old, pushed: recent},
		"gone/away":   {gone: true},
		"is/archived": {stars: 5000, created: old, pushed: recent, licence: "MIT", archived: true},
	}
	srv := listServer(t, map[string]string{"me/awesome": md}, meta, map[string]fakeRepo{
		// Every one of them ships an installable release, so only the floors can reject them.
		"good/tool": tinyserve, "few/stars": tinyserve, "too/new": tinyserve, "long/dead": tinyserve,
		"no/licence": tinyserve, "is/archived": tinyserve,
	})
	out := filepath.Join(t.TempDir(), "c.txt")
	err := discover(t.Context(), discoverConfig{
		topics: []string{"cli"}, curated: []string{"me/awesome"}, minStars: 1000,
		since: now.AddDate(-2, 0, 0), born: now.AddDate(0, -6, 0), max: 10,
		idx: &index.Index{Version: index.Version}, gh: newGitHub(srv.URL, "t"), out: out,
		host: host.Host{Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readRepoList(out); strings.Join(got, ",") != "good/tool" {
		t.Fatalf("accepted %v, want only good/tool", got)
	}
}

// 120 links take three GraphQL requests of at most 50, not 120 REST calls.
func TestRepoMetaBatches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if n := len(aliasRe.FindAllString(string(body), -1)); n > 50 {
			t.Errorf("one request asked for %d repositories", n)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	ids := make([]string, 120)
	for i := range ids {
		ids[i] = "o/r" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
	}
	if _, err := newGitHub(srv.URL, "t").repoMeta(t.Context(), ids); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("%d requests, want 3", calls)
	}
}

// A renamed repository is still the same tool: the index says Peltoche/lsd, the list
// links lsd-rs/lsd, and GitHub answers for the old name with the new one.
func TestDiscoverKnowsARenamedRepoIsListed(t *testing.T) {
	now := time.Now()
	meta := map[string]metaRepo{
		"lsd-rs/lsd": {stars: 16000, created: now.AddDate(-5, 0, 0), pushed: now.AddDate(0, -1, 0), licence: "Apache-2.0"},
	}
	srv := listServer(t, map[string]string{"me/awesome": "- https://github.com/lsd-rs/lsd\n"}, meta,
		map[string]fakeRepo{"lsd-rs/lsd": tinyserve})
	// The fake GraphQL answers "Peltoche/lsd" with the repository's current name.
	renamed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "Peltoche") { // the query travels inside JSON, quotes escaped
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"r0": map[string]any{
				"nameWithOwner": "lsd-rs/lsd", "stargazerCount": 16000, "createdAt": now.AddDate(-5, 0, 0).Format(time.RFC3339),
				"pushedAt": now.Format(time.RFC3339), "licenseInfo": map[string]any{"spdxId": "Apache-2.0"},
			}}})
			return
		}
		req, _ := http.NewRequest(r.Method, srv.URL+r.URL.String(), strings.NewReader(string(body)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(renamed.Close)

	idx := &index.Index{Version: index.Version}
	idx.Upsert(index.Entry{ID: "Peltoche/lsd", Source: "github:Peltoche/lsd", Desc: "d"})
	out := filepath.Join(t.TempDir(), "c.txt")
	err := discover(t.Context(), discoverConfig{
		curated: []string{"me/awesome"}, minStars: 1000, since: now.AddDate(-2, 0, 0), born: now.AddDate(0, -6, 0),
		max: 10, idx: idx, gh: newGitHub(renamed.URL, "t"), out: out,
		host: host.Host{Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readRepoList(out); len(got) != 0 {
		t.Fatalf("a renamed repository already in the index was proposed again: %v", got)
	}
}
