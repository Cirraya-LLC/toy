package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// audFakeHost stands a "host" up on disk: a bare git repo handles tag discovery
// (host.Tags runs git ls-remote, which works fine against a local path), and raw
// and release go over file://.
type audFakeHost struct{ root string }

func audNewFakeHost(t *testing.T) *audFakeHost {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	return &audFakeHost{root: t.TempDir()}
}

func (h *audFakeHost) tmpl() host.Host {
	return host.Host{
		Raw:     "file://" + h.root + "/{owner}/{repo}/raw/{ref}/{path}",
		Clone:   h.root + "/{owner}/{repo}.git",
		Release: "file://" + h.root + "/{owner}/{repo}/rel/{tag}/{asset}",
		Web:     "https://fake.example/{owner}/{repo}",
	}
}

// publish creates a tag, a toy.toml and a release asset for owner/repo (the
// single-file script form).
func (h *audFakeHost) publish(t *testing.T, owner, repo, tag, body string) {
	t.Helper()
	bare := filepath.Join(h.root, owner, repo+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := filepath.Join(h.root, owner, repo+".work")
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		run(bare, "init", "-q", "--bare", bare)
		os.MkdirAll(work, 0o755)
		os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
		run(work, "init", "-q", work)
		run(work, "add", "f")
		run(work, "commit", "-q", "-m", "c")
		run(work, "push", "-q", bare, "HEAD:refs/heads/main")
	}
	// These tagging lines used to sit entirely inside the "repo does not exist
	// yet" branch, so publishing a second version of the same repo only wrote
	// raw and rel and pushed no tag at all — ls-remote could only ever see the
	// first version, and in the tests it just looked like "installed the wrong
	// version". -f makes it idempotent.
	run(work, "tag", "-f", tag)
	run(work, "push", "-q", "-f", bare, "refs/tags/"+tag)
	// The toy.toml on raw (stored under {ref}, i.e. the tag)
	rawDir := filepath.Join(h.root, owner, repo, "raw", tag)
	os.MkdirAll(rawDir, 0o755)
	os.WriteFile(filepath.Join(rawDir, "toy.toml"), []byte(`
name    = "audtool"
desc    = "d"
[bin]
audtool = "audtool"
[artifact]
"`+i1Plat()+`" = "audtool"
`), 0o644)
	// The release asset: not an archive, so it takes the bare-binary path
	relDir := filepath.Join(h.root, owner, repo, "rel", tag)
	os.MkdirAll(relDir, 0o755)
	os.WriteFile(filepath.Join(relDir, "audtool"), []byte(body), 0o644)
}

// Can the index swap an installed package for the content of ANOTHER repo during
// `toy update`?
//
// When m.IndexID != "", insUpdateOne re-resolves from the index entry. For an
// entry that is NOT signed, editing one line of source swaps the content behind
// the command on the user's PATH, and there is not a single gate in the way:
//   - insTrustDowngraded only catches signed -> not signed, so with neither side
//     signed it does nothing
//   - the TOFU id is insTrustID(res) = "<host>:<owner>/<repo>@<tag>", so changing
//     source makes it a brand new record -> TrustNew -> no warning, pinned straight away
func TestAuditIndexCanRepointAnInstalledPackage(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	fh.publish(t, "evil", "tool", "v2.0.0", "#!/bin/sh\necho EVIL\n")

	_, home, writeIdx := audIndexSetup(t, fh)

	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("what got installed is not alice's: %q", b)
	}

	// That one source line in the index got edited.
	writeIdx("fake:evil/tool")

	err := runUpdate(context.Background(), nil)
	got, _ := os.ReadFile(link)
	if string(got) == "#!/bin/sh\necho EVIL\n" {
		t.Fatalf("`toy update` swapped audtool on PATH for the content of evil/tool — "+
			"no confirmation, no TOFU warning, and the trust level did not change either (update err=%v)", err)
	}
	t.Logf("not swapped (err=%v, content=%q)", err, got)
}

// audIndexSetup stands up a "one local index plus one local host" environment:
// TOY_HOME points at a temp dir, the index holds only alice/tool, and the
// returned hook rewrites its source at will.
func audIndexSetup(t *testing.T, fh *audFakeHost) (*store.Store, string, func(source string)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(t.TempDir(), "index.json")
	cfg := store.DefaultConfig()
	cfg.Indexes = []store.IndexRef{{Name: "local", URL: "file://" + idxPath}}
	cfg.Hosts = map[string]host.Host{"fake": fh.tmpl()}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return st, home, func(source string) {
		b, _ := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
			{ID: "alice/tool", Source: source, Desc: "d"},
		}})
		if err := os.WriteFile(idxPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
