package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
)

const upstreamToml = `name = "tinyserve"
desc = "d"
license = "MIT"
[bin]
tinyserve = "tinyserve"
[artifact]
"linux/amd64" = "tinyserve-{version}-linux-amd64.tar.gz"
`

func genWithRaw(t *testing.T, repos map[string]fakeRepo, list []string) (string, error) {
	t.Helper()
	srv := fakeAPI(t, repos)
	out := filepath.Join(t.TempDir(), "idx.json")
	return out, gen(t.Context(), genConfig{
		repos: list, out: out, prov: newGitHub(srv.URL, ""),
		host: host.Host{
			Raw:     srv.URL + "/raw/{owner}/{repo}/{ref}/{path}",
			Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}",
		},
	})
}

// Upstream already says how to install it, so the entry only says where to look: no
// toyfile of ours, which the client would never read anyway (upstream wins, §5.1 rule 1).
func TestGenPointsAtUpstreamsOwnToyToml(t *testing.T) {
	r := tinyserve
	r.toyToml = upstreamToml
	out, err := genWithRaw(t, map[string]fakeRepo{"Cirraya-LLC/TinyServe": r}, []string{"Cirraya-LLC/TinyServe"})
	if err != nil {
		t.Fatal(err)
	}
	e := loadOut(t, out).Packages
	if len(e) != 1 || e[0].Toyfile != "" || e[0].Source != "github:Cirraya-LLC/TinyServe" {
		t.Fatalf("want one pointer entry with no toyfile, got %+v", e)
	}
}

// No toy.toml upstream (a plain 404): the usual inline entry.
func TestGenInlinesWhenUpstreamHasNoToyToml(t *testing.T) {
	out, err := genWithRaw(t, map[string]fakeRepo{"a/b": tinyserve}, []string{"a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if e := loadOut(t, out).Packages; len(e) != 1 || !strings.Contains(e[0].Toyfile, "[artifact]") {
		t.Fatalf("want one inline entry, got %+v", e)
	}
}

// A 500 is not "absent". Falling through to an inline entry would guess on a bad
// network day; the repo fails instead and is retried next run.
func TestGenDoesNotGuessWhenUpstreamCannotBeAsked(t *testing.T) {
	r := tinyserve
	r.rawStatus = http.StatusInternalServerError
	if _, err := genWithRaw(t, map[string]fakeRepo{"a/b": r}, []string{"a/b"}); err == nil {
		t.Fatal("a 500 on upstream's toy.toml must fail the repo, not produce an inline entry")
	}
}

// A toy.toml the client would refuse is reported, not papered over with ours.
func TestGenReportsABrokenUpstreamToyToml(t *testing.T) {
	r := tinyserve
	r.toyToml = "name = \"x\"\npostinstall = \"rm -rf /\"\n"
	if _, err := genWithRaw(t, map[string]fakeRepo{"a/b": r}, []string{"a/b"}); err == nil {
		t.Fatal("an upstream toy.toml that does not parse must fail the repo")
	}
}
