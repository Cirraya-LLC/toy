package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
)

const handToml = `name = "ast-grep"
desc = "structural search"
license = "MIT"
[bin]
ast-grep = "ast-grep"
[artifact]
"linux/amd64" = "app-x86_64-unknown-linux-gnu.zip"
`

func addFixture(t *testing.T, repos map[string]fakeRepo) (addConfig, string) {
	t.Helper()
	srv := fakeAPI(t, repos)
	out := filepath.Join(t.TempDir(), "idx.json")
	return addConfig{
		id: "ast-grep/ast-grep", toyfile: []byte(handToml), out: out, prov: newGitHub(srv.URL, ""),
		host: host.Host{Raw: srv.URL + "/raw/{owner}/{repo}/{ref}/{path}"},
	}, out
}

func TestAddWritesTheFileAsItIs(t *testing.T) {
	cfg, out := addFixture(t, map[string]fakeRepo{"ast-grep/ast-grep": tinyserve})
	if err := add(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	e := loadOut(t, out).Packages
	if len(e) != 1 || e[0].Toyfile != handToml || e[0].Source != "github:ast-grep/ast-grep" {
		t.Fatalf("got %+v", e)
	}
}

// Everything gen refuses, add refuses too.
func TestAddHoldsAHandWrittenEntryToGensRules(t *testing.T) {
	busl := tinyserve
	busl.license = "BUSL-1.1"
	withUpstream := tinyserve
	withUpstream.toyToml = handToml
	cases := map[string]struct {
		repo  fakeRepo
		toml  string
		lists lists
		want  string
	}{
		"licence":     {busl, handToml, lists{}, "not on the open-source allowlist"},
		"sha256":      {tinyserve, handToml + "[sha256]\n\"linux/amd64\" = \"" + strings.Repeat("a", 64) + "\"\n", lists{}, "[sha256]"},
		"platform":    {tinyserve, strings.Replace(handToml, "ast-grep = ", "ast-grep-linux = ", 1), lists{}, "platform word"},
		"upstream":    {withUpstream, handToml, lists{}, "pointer entry"},
		"delisted":    {tinyserve, handToml, lists{delisted: map[string]bool{"ast-grep/ast-grep": true}}, "delisted"},
		"declined":    {tinyserve, handToml, lists{declined: map[string]bool{"ast-grep/ast-grep": true}}, "declined.txt"},
		"bad licence": {tinyserve, strings.Replace(handToml, `"MIT"`, `"GPL-3.0"`, 1), lists{}, "repository's licence is MIT"},
	}
	for name, c := range cases {
		cfg, out := addFixture(t, map[string]fakeRepo{"ast-grep/ast-grep": c.repo})
		cfg.toyfile, cfg.lists = []byte(c.toml), c.lists
		err := add(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, c.want, err)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("%s: nothing should have been written", name)
		}
	}
}
