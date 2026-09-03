package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
)

// newStore opens a temporary store and clears PATH. The shadow check reads the
// real PATH, so without isolating it the results depend on what happens to be
// installed on the machine running the tests.
func newStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("PATH", "")
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// mkexec creates an executable file and returns its path.
func mkexec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenCreatesLayout(t *testing.T) {
	tests := []struct {
		name    string
		root    func(t *testing.T) string // returns the root passed to Open
		toyHome string
		wantEnv bool // expect it to land on TOY_HOME
	}{
		{name: "explicit root", root: func(t *testing.T) string { return t.TempDir() }},
		{name: "empty root falls back to TOY_HOME", root: func(t *testing.T) string { return "" }, toyHome: "x", wantEnv: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := tc.root(t)
			if tc.toyHome != "" {
				root = ""
				t.Setenv("TOY_HOME", filepath.Join(base, tc.toyHome))
			}
			s, err := Open(root)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tc.wantEnv && s.Root != filepath.Join(base, tc.toyHome) {
				t.Fatalf("Root = %q, want the one TOY_HOME points at", s.Root)
			}
			for _, d := range []string{
				s.BinDir(), s.CacheDir(), s.KeysDir(), s.KnownDir(), s.IndexDir(),
				filepath.Join(s.Root, dirPkg), filepath.Join(s.Root, dirInstalled),
			} {
				fi, err := os.Stat(d)
				if err != nil {
					t.Errorf("directory was not created %s: %v", d, err)
					continue
				}
				if !fi.IsDir() {
					t.Errorf("%s is not a directory", d)
				}
			}
		})
	}
}

// Private keys live in keys/. Nobody else gets to read them.
func TestOpenKeysDirIsPrivate(t *testing.T) {
	s := newStore(t)
	fi, err := os.Stat(s.KeysDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("keys/ mode = %o, want 700 (private keys live in there)", perm)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("the second Open should succeed too: %v", err)
	}
}

func TestOpenRelativeRootBecomesAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := Open("toyhome")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.Root) {
		t.Fatalf("Root = %q, should be absolute", s.Root)
	}
}

func TestPkgDirIsAlwaysInsideRoot(t *testing.T) {
	s := newStore(t)
	pkgRoot := filepath.Join(s.Root, dirPkg)
	tests := []struct {
		name                      string
		host, owner, pkg, version string
	}{
		{"ordinary", "github", "Derrity", "tinyserve", "1.2.3"},
		{"path traversal", "..", "..", "..", ".."},
		{"slashes", "gh/x", "a/../..", "n", "v/../.."},
		{"empty segments", "", "", "", ""},
		{"absolute paths", "/etc", "/etc", "passwd", "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.PkgDir(tc.host, tc.owner, tc.pkg, tc.version)
			rel, err := filepath.Rel(pkgRoot, got)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("PkgDir = %q escaped %q", got, pkgRoot)
			}
			// The four-segment structure must not get flattened.
			if n := len(strings.Split(rel, string(os.PathSeparator))); n != 4 {
				t.Fatalf("PkgDir relative path %q has %d segments, want 4", rel, n)
			}
		})
	}
}

func TestConfigRoundTrip(t *testing.T) {
	s := newStore(t)

	// A brand-new store reads back as the built-in defaults. How many indexes that
	// is depends on DefaultIndexURL; see DefaultConfig.
	got, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if fresh := DefaultConfig(); len(got.Indexes) != len(fresh.Indexes) {
		t.Fatalf("a fresh store should read back the built-in defaults %+v, got %+v", fresh.Indexes, got.Indexes)
	}

	want := Config{
		Indexes: []IndexRef{
			{Name: "default", URL: "https://example.com/index.json"},
			{Name: "local", URL: "file:///tmp/idx.json"},
		},
		Mirrors: []string{"https://m1.example.com", "https://m2.example.com/with#hash-and\"quotes\""},
		Hosts: map[string]host.Host{
			"mygit": {
				Name:    "mygit",
				Raw:     "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}",
				Clone:   "https://git.example.com/{owner}/{repo}",
				Release: "https://git.example.com/{owner}/{repo}/releases/download/{tag}/{asset}",
				Web:     "https://git.example.com/{owner}/{repo}",
			},
		},
	}
	if err := s.SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err = s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Indexes) != 2 || got.Indexes[0] != want.Indexes[0] || got.Indexes[1] != want.Indexes[1] {
		t.Errorf("Indexes = %+v, want %+v", got.Indexes, want.Indexes)
	}
	if strings.Join(got.Mirrors, "|") != strings.Join(want.Mirrors, "|") {
		t.Errorf("Mirrors = %q, want %q", got.Mirrors, want.Mirrors)
	}
	if got.Hosts["mygit"] != want.Hosts["mygit"] {
		t.Errorf("Hosts[mygit] = %+v, want %+v", got.Hosts["mygit"], want.Hosts["mygit"])
	}
}

// After the user clears every index with `toy index rm`, the defaults must not
// refill the list.
func TestLoadConfigKeepsAnEmptyIndexList(t *testing.T) {
	s := newStore(t)
	if err := s.SaveConfig(Config{Indexes: nil, Mirrors: []string{}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Indexes) != 0 {
		t.Fatalf("Indexes = %+v, should be empty once the user removed them all", got.Indexes)
	}
}

func TestLoadConfigRejectsGarbage(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.configPath(), []byte("[hosts.github\nraw = "), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err == nil {
		t.Fatal("a corrupt config.toml must error out, not silently become an empty config")
	}
}

func sampleManifest(id, name string) Manifest {
	return Manifest{
		ID: id, Name: name, Version: "1.2.3", Tag: "v1.2.3",
		Host: "github", Owner: "Derrity", Repo: name,
		Trust: TrustHost, Signer: "", SHA256: "abc123",
		Source: "https://github.com/Derrity/" + name + "/releases/download/v1.2.3/x.tar.gz",
		PkgDir: "/root/.toy/pkg/github/Derrity/" + name + "/1.2.3",
		Links:  []string{"/root/.toy/bin/" + name},
		// Non-ASCII text and quotes have to round-trip byte for byte.
		InstalledAt: "2026-08-31T12:00:00Z",
	}
}

func TestManifestRoundTrip(t *testing.T) {
	s := newStore(t)
	want := sampleManifest("github:Derrity/tinyserve", "tinyserve")
	want.Signer = `a3f9"naïve unicode"\backslash`
	want.Links = append(want.Links, "/root/.toy/bin/ts")

	if err := s.SaveManifest(want); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	got, err := s.LoadManifest(want.ID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.Version != want.Version ||
		got.Tag != want.Tag || got.Host != want.Host || got.Owner != want.Owner ||
		got.Repo != want.Repo || got.Trust != want.Trust || got.Signer != want.Signer ||
		got.SHA256 != want.SHA256 || got.Source != want.Source || got.PkgDir != want.PkgDir ||
		got.InstalledAt != want.InstalledAt {
		t.Fatalf("the manifest did not round-trip:\n got = %+v\nwant = %+v", got, want)
	}
	if strings.Join(got.Links, "|") != strings.Join(want.Links, "|") {
		t.Fatalf("Links = %q, want %q", got.Links, want.Links)
	}
}

func TestLoadManifestNotInstalled(t *testing.T) {
	s := newStore(t)
	_, err := s.LoadManifest("github:nobody/nothing")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

func TestManifestPathStaysInInstalledDir(t *testing.T) {
	s := newStore(t)
	installed := filepath.Join(s.Root, dirInstalled)
	ids := []string{
		"github:Derrity/tinyserve",
		"../../../etc/passwd",
		"..",
		"https://evil.example.com/a/b/toy.toml",
		strings.Repeat("é", 200),
	}
	seen := map[string]string{}
	for _, id := range ids {
		p, err := s.manifestPath(id)
		if err != nil {
			t.Fatalf("manifestPath(%q): %v", id, err)
		}
		if filepath.Dir(p) != installed {
			t.Errorf("manifestPath(%q) = %q, escaped %q", id, p, installed)
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("%q and %q collide on the same file %q", id, prev, p)
		}
		seen[p] = id
	}
}

// The escaping is many-to-one; the hash suffix is what keeps them apart.
func TestManifestPathDistinguishesIDsThatEscapeAlike(t *testing.T) {
	s := newStore(t)
	a, _ := s.manifestPath("github:a/b")
	b, _ := s.manifestPath("github/a:b")
	if a == b {
		t.Fatalf("two different ids landed in the same manifest file %q — the hash suffix did nothing", a)
	}
	if !strings.HasPrefix(filepath.Base(a), "github_a_b-") {
		t.Fatalf("filename %q does not look escaped", filepath.Base(a))
	}
}

func TestManifestPathRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	if _, err := s.manifestPath("   "); err == nil {
		t.Fatal("an empty id must error out")
	}
	if err := s.SaveManifest(Manifest{}); err == nil {
		t.Fatal("SaveManifest must reject an empty id")
	}
}

func TestListAndRemoveManifests(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"github:z/zed", "github:a/alpha", "gitlab:m/mid"} {
		name := strings.SplitN(id, "/", 2)[1]
		if err := s.SaveManifest(sampleManifest(id, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Anything that is not a .toml must not be mistaken for a manifest.
	if err := os.WriteFile(filepath.Join(s.Root, dirInstalled, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListManifests()
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	want := []string{"github:a/alpha", "github:z/zed", "gitlab:m/mid"} // sorted by id
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ListManifests = %v, want %v (sorted by id)", ids, want)
	}

	if err := s.RemoveManifest("github:z/zed"); err != nil {
		t.Fatalf("RemoveManifest: %v", err)
	}
	if err := s.RemoveManifest("github:z/zed"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("repeated Remove err = %v, want ErrNotInstalled", err)
	}
	if got, _ := s.ListManifests(); len(got) != 2 {
		t.Fatalf("%d manifests left after the delete, want 2", len(got))
	}
}

func TestListManifestsEmptyStore(t *testing.T) {
	s := newStore(t)
	got, err := s.ListManifests()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: got = %v, err = %v", got, err)
	}
}

// A corrupt manifest has to be loud, never skipped: skipping it means that
// package can never be uninstalled.
func TestListManifestsRefusesToSkipACorruptOne(t *testing.T) {
	s := newStore(t)
	if err := s.SaveManifest(sampleManifest("github:a/b", "b")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(s.Root, dirInstalled, "broken.toml")
	if err := os.WriteFile(bad, []byte(`name = "no closing quote`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManifests(); err == nil {
		t.Fatal("a corrupt manifest must make ListManifests fail")
	}
}

func TestFindByName(t *testing.T) {
	s := newStore(t)
	ts := sampleManifest("github:Derrity/tinyserve", "tinyserve")
	ts.Links = []string{"/x/bin/tinyserve", "/x/bin/ts"}
	other := sampleManifest("gitlab:someone/tinyserve", "tinyserve") // same name, different host
	solo := sampleManifest("github:me/solo", "solo")
	for _, m := range []Manifest{ts, other, solo} {
		if err := s.SaveManifest(m); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name     string
		query    string
		wantID   string
		wantErr  error
		wantList []string // candidates the ambiguity error must name
	}{
		{name: "unique hit by package name", query: "solo", wantID: "github:me/solo"},
		{name: "unique hit by repo name", query: "solo", wantID: "github:me/solo"},
		{name: "full id hits directly", query: "github:Derrity/tinyserve", wantID: "github:Derrity/tinyserve"},
		{name: "the other full id", query: "gitlab:someone/tinyserve", wantID: "gitlab:someone/tinyserve"},
		{name: "hit by symlink name", query: "ts", wantID: "github:Derrity/tinyserve"},
		{name: "not installed", query: "nope", wantErr: ErrNotInstalled},
		{name: "empty name", query: "  ", wantErr: nil},
		{
			name:     "ambiguity must list the candidates",
			query:    "tinyserve",
			wantList: []string{"github:Derrity/tinyserve", "gitlab:someone/tinyserve"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FindByName(tc.query)
			switch {
			case tc.wantList != nil:
				if err == nil {
					t.Fatalf("want an ambiguity error, got %+v", got)
				}
				for _, id := range tc.wantList {
					if !strings.Contains(err.Error(), id) {
						t.Errorf("the error does not name candidate %q: %v", id, err)
					}
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantID != "":
				if err != nil {
					t.Fatalf("FindByName(%q): %v", tc.query, err)
				}
				if got.ID != tc.wantID {
					t.Fatalf("ID = %q, want %q", got.ID, tc.wantID)
				}
			default:
				if err == nil {
					t.Fatal("want an error")
				}
			}
		})
	}
}

func TestLinkHappyPath(t *testing.T) {
	s := newStore(t)
	target := mkexec(t, t.TempDir(), "tinyserve")

	got, err := s.Link("tinyserve", target, false)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got != filepath.Join(s.BinDir(), "tinyserve") {
		t.Fatalf("link path = %q", got)
	}
	dest, err := os.Readlink(got)
	if err != nil {
		t.Fatalf("not a symlink: %v", err)
	}
	if dest != target {
		t.Fatalf("points at %q, want %q", dest, target)
	}
}

// Iron law 5: a program of the same name already on PATH blocks the install,
// unless --force.
func TestLinkRefusesToShadowPATH(t *testing.T) {
	s := newStore(t)
	sysDir := t.TempDir()
	sudo := mkexec(t, sysDir, "sudo")
	t.Setenv("PATH", sysDir)
	target := mkexec(t, t.TempDir(), "payload")

	_, err := s.Link("sudo", target, false)
	if !errors.Is(err, ErrShadow) {
		t.Fatalf("err = %v, want ErrShadow", err)
	}
	// The error must name the full path of the program being shadowed; without it
	// the user cannot judge whether --force is the right call.
	if !strings.Contains(err.Error(), sudo) {
		t.Fatalf("the error does not say which program gets shadowed (want %q): %v", sudo, err)
	}
	if _, statErr := os.Lstat(filepath.Join(s.BinDir(), "sudo")); statErr == nil {
		t.Fatal("a rejected Link must not leave any symlink behind")
	}

	// Only --force gets through.
	got, err := s.Link("sudo", target, true)
	if err != nil {
		t.Fatalf("force should be allowed through: %v", err)
	}
	if dest, _ := os.Readlink(got); dest != target {
		t.Fatalf("after force it points at %q, want %q", dest, target)
	}
}

// When BinDir itself is on PATH, LookPath finds our own symlink. That is not
// shadowing.
func TestLinkOurOwnBinDirIsNotShadow(t *testing.T) {
	s := newStore(t)
	t.Setenv("PATH", s.BinDir())
	target := mkexec(t, t.TempDir(), "tinyserve")

	if _, err := s.Link("tinyserve", target, false); err != nil {
		t.Fatalf("first Link: %v", err)
	}
	// bin/tinyserve is now both on PATH and inside BinDir: that should report
	// "already exists", not "shadowing".
	_, err := s.Link("tinyserve", target, false)
	if !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v, want ErrLinkExists (our own symlink is not shadowing)", err)
	}
}

// A PATH entry written as a symlink to BinDir, or as a relative path, must not
// count as shadowing either.
func TestLinkPATHEntryViaSymlinkIsNotShadow(t *testing.T) {
	s := newStore(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(s.BinDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", alias)
	target := mkexec(t, t.TempDir(), "tinyserve")

	if _, err := s.Link("tinyserve", target, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Link("tinyserve", target, false); !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v, want ErrLinkExists — through the symlink it is still our own BinDir", err)
	}
}

func TestLinkExistsAndForceReplaces(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	old := mkexec(t, dir, "v1")
	fresh := mkexec(t, dir, "v2")

	if _, err := s.Link("tool", old, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.Link("tool", fresh, false)
	if !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v, want ErrLinkExists", err)
	}
	if dest, _ := os.Readlink(filepath.Join(s.BinDir(), "tool")); dest != old {
		t.Fatal("after a rejection the old symlink must stay exactly as it was")
	}

	link, err := s.Link("tool", fresh, true)
	if err != nil {
		t.Fatalf("force replace: %v", err)
	}
	if dest, _ := os.Readlink(link); dest != fresh {
		t.Fatalf("points at %q, want the new %q", dest, fresh)
	}
	// Atomic replace: no temp file may be left behind.
	entries, _ := os.ReadDir(s.BinDir())
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("bin/ holds %v, want only tool (the temp name was not cleaned up)", names)
	}
}

// Force-replacing a regular file (not a symlink) has to work too: rename just
// takes over.
func TestLinkForceReplacesARegularFile(t *testing.T) {
	s := newStore(t)
	stale := filepath.Join(s.BinDir(), "tool")
	if err := os.WriteFile(stale, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := mkexec(t, t.TempDir(), "v1")
	if _, err := s.Link("tool", target, true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if dest, _ := os.Readlink(stale); dest != target {
		t.Fatalf("points at %q, want %q", dest, target)
	}
}

func TestLinkRejectsBadInput(t *testing.T) {
	s := newStore(t)
	good := mkexec(t, t.TempDir(), "ok")

	tests := []struct {
		name, link, target string
	}{
		{"empty name", "", good},
		{"slash", "../evil", good},
		{"slash again", "a/b", good},
		{"dot", ".", good},
		{"dot dot", "..", good},
		{"target does not exist", "tool", filepath.Join(t.TempDir(), "no-such-file")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Link(tc.link, tc.target, false); err == nil {
				t.Fatalf("Link(%q, %q) must error out", tc.link, tc.target)
			}
			// force must not waive a structural error either.
			if _, err := s.Link(tc.link, tc.target, true); err == nil {
				t.Fatalf("Link(%q, %q, force) must error out too", tc.link, tc.target)
			}
		})
	}
}

// A relative target resolves against bin/, so it has to be stored absolute.
func TestLinkStoresAnAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := newStore(t)
	if err := os.WriteFile("tool.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link, err := s.Link("tool", "tool.sh", false)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := os.Readlink(link)
	if !filepath.IsAbs(dest) {
		t.Fatalf("target = %q, should be absolute", dest)
	}
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("the symlink is dangling: %v", err)
	}
}

func TestUnlink(t *testing.T) {
	s := newStore(t)
	target := mkexec(t, t.TempDir(), "tool")
	link, err := s.Link("tool", target, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unlink(link); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("the symlink was not deleted")
	}
	// Re-runnable: an uninstall that died halfway must not fail on the retry.
	if err := s.Unlink(link); err != nil {
		t.Fatalf("a repeated Unlink should be a no-op: %v", err)
	}
	// The target itself must not be deleted.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("Unlink must not touch the file the symlink points at: %v", err)
	}
}

// A corrupt manifest must not become authorization to "delete /usr/bin/sudo".
func TestUnlinkRefusesOutsideBinDir(t *testing.T) {
	s := newStore(t)
	outside := mkexec(t, t.TempDir(), "sudo")
	for _, p := range []string{outside, filepath.Join(s.BinDir(), "sub", "x"), s.Root} {
		if err := s.Unlink(p); err == nil {
			t.Errorf("Unlink(%q) must be refused", p)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a refused path must not get deleted: %v", err)
	}
}

func TestInPath(t *testing.T) {
	s := newStore(t)
	other := t.TempDir()
	aliasDir := t.TempDir()
	alias := filepath.Join(aliasDir, "link-to-bin")
	if err := os.Symlink(s.BinDir(), alias); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"not present", other, false},
		{"empty PATH", "", false},
		{"exact match", s.BinDir(), true},
		{"somewhere in the middle", other + ":" + s.BinDir() + ":/usr/bin", true},
		{"trailing slash", s.BinDir() + string(os.PathSeparator), true},
		{".. in the path", filepath.Join(s.BinDir(), "..", dirBin), true},
		{"through a symlink", alias, true},
		{"empty entry means the current directory", ":" + other, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			if got := s.InPath(); got != tc.want {
				t.Fatalf("InPath() = %v, want %v (PATH=%q)", got, tc.want, tc.path)
			}
		})
	}
}

// A relative entry in PATH resolves against the current directory.
func TestInPathHandlesRelativeEntries(t *testing.T) {
	base := t.TempDir()
	t.Setenv("PATH", "")
	s, err := Open(filepath.Join(base, "toy"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	t.Setenv("PATH", filepath.Join("toy", dirBin))
	if !s.InPath() {
		t.Fatal("the relative PATH entry was not recognized as BinDir")
	}
	t.Setenv("PATH", ".")
	if s.InPath() {
		t.Fatal("the current directory is not BinDir, so it must not count as on PATH")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	// One constant, DefaultIndexURL, decides how many indexes the default config
	// carries: empty (as it is now) means none preconfigured, a filled-in address
	// means exactly that one. What this assertion pins down is that NO INDEX WITH
	// AN EMPTY NAME OR ADDRESS may appear — an entry with an empty url would make
	// every search print a warning.
	if DefaultIndexURL == "" {
		if len(c.Indexes) != 0 {
			t.Fatalf("DefaultIndexURL is empty, so no index may be preconfigured, got %+v", c.Indexes)
		}
	} else {
		if len(c.Indexes) != 1 {
			t.Fatalf("want one built-in index, got %d", len(c.Indexes))
		}
		if c.Indexes[0].Name == "" || c.Indexes[0].URL == "" {
			t.Fatalf("the built-in index %+v is missing something", c.Indexes[0])
		}
	}
	if len(c.Mirrors) != 0 {
		t.Fatalf("the mirror list should default to empty, got %v", c.Mirrors)
	}
	if c.Hosts == nil {
		t.Fatal("Hosts should be an empty map that is writable straight away")
	}
	c.Hosts["x"] = host.Host{} // must not panic
}

// The hole in iron law 5: when exec.LookPath finds something inside a relative
// PATH entry, it returns it together with exec.ErrDot. Check only err == nil and
// any relative entry in PATH — "." or, far more common, the empty entry that
// `export PATH=:$PATH` writes — silently switches off the shadow check for the
// whole of PATH, and `[bin] sudo = "payload"` installs.
func TestLinkShadowSurvivesARelativePATHEntry(t *testing.T) {
	base := t.TempDir()
	sysDir := filepath.Join(base, "sys")
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sudo := mkexec(t, sysDir, "sudo")
	t.Chdir(base)

	pathVals := map[string]string{
		"relative entry":                    "sys",
		"empty entry means current dir":     string(os.PathListSeparator) + sysDir,
		"relative entry before an absolute": "sys" + string(os.PathListSeparator) + sysDir,
	}
	for name, pathVal := range pathVals {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", "")
			s, err := Open(filepath.Join(t.TempDir(), "toy"))
			if err != nil {
				t.Fatal(err)
			}
			if name == "empty entry means current dir" {
				// Put a sudo in the current directory too, so the empty entry
				// really does hit something.
				mkexec(t, base, "sudo")
			}
			t.Setenv("PATH", pathVal)
			target := mkexec(t, t.TempDir(), "payload")

			if _, err := s.Link("sudo", target, false); !errors.Is(err, ErrShadow) {
				t.Fatalf("PATH=%q: err = %v, want ErrShadow (%s really is on PATH)", pathVal, err, sudo)
			}
			if _, statErr := os.Lstat(filepath.Join(s.BinDir(), "sudo")); statErr == nil {
				t.Fatal("a rejected Link must not leave a symlink behind")
			}
		})
	}
}

// Two different packages must never land in the same install directory:
// installing the second overwrites the first's files, and uninstalling the
// second deletes the first's executable along with it.
func TestPkgDirNeverCollides(t *testing.T) {
	s := newStore(t)
	long := strings.Repeat("x", 200)
	segs := [][4]string{
		{"github", "a/b", "n", "1"},
		{"github", "a:b", "n", "1"},
		{"github", "a_b", "n", "1"},
		{"github", long + "1", "n", "1"},
		{"github", long + "2", "n", "1"},
		{"github", "..", "n", "1"},
		{"github", ".", "n", "1"},
		{"github", "", "n", "1"},
		{"github", "Derrity", "tinyserve", "1.2.3"},
		{"github", "Derrity", "tinyserve", "1.2.3+build"},
		// These groups differ only in case, which on the default APFS on macOS is
		// the same directory name. They have to be kept apart by the hash suffix,
		// not by relying on the filesystem being case-sensitive.
		{"github", "derrity", "tinyserve", "1.2.3"},
		{"github", "DERRITY", "tinyserve", "1.2.3"},
		{"github", "derrity", "TinySERVE", "1.2.3"},
		{"github", "derrity", "tinyserve", "1.2.3-RC1"},
		{"github", "derrity", "tinyserve", "1.2.3-rc1"},
	}
	seen := map[string][4]string{}
	for _, sg := range segs {
		p := s.PkgDir(sg[0], sg[1], sg[2], sg[3])
		if prev, dup := seen[p]; dup {
			t.Errorf("%v and %v collide on the same directory %q", sg, prev, p)
		}
		seen[p] = sg
	}
	// A lowercase segment must not be made unreadable by a hash suffix.
	want := filepath.Join(s.Root, dirPkg, "github", "derrity", "tinyserve", "1.2.3")
	if got := s.PkgDir("github", "derrity", "tinyserve", "1.2.3"); got != want {
		t.Errorf("ordinary path = %q, want %q", got, want)
	}
	// A segment with uppercase still has to be recognizable after folding, just
	// with a hash appended.
	if got := s.PkgDir("github", "Derrity", "tinyserve", "1.2.3"); !strings.Contains(got, "/derrity-") {
		t.Errorf("an uppercase owner should fold to derrity-<hash>, got %q", got)
	}
}

// Manifest filenames, same story: on APFS two ids differing only in case write
// to the same file, the package installed second overwrites the first's manifest
// wholesale, and the first can then neither be uninstalled nor updated.
func TestManifestPathNeverCollidesOnCaseInsensitiveFS(t *testing.T) {
	s := newStore(t)
	ids := []string{
		"github:Derrity/tinyserve",
		"github:derrity/tinyserve",
		"github:derrity/TinyServe",
		"https://example.com/toy.toml",
		"https://EXAMPLE.com/toy.toml",
	}
	seen := map[string]string{}
	for _, id := range ids {
		p, err := s.manifestPath(id)
		if err != nil {
			t.Fatalf("manifestPath(%q): %v", id, err)
		}
		fold := strings.ToLower(p) // this is how a case-insensitive filesystem compares
		if prev, dup := seen[fold]; dup {
			t.Errorf("%q and %q collide on the same manifest file %q", id, prev, p)
		}
		seen[fold] = id
	}
}

// A package manager that writes your password into ~/.toy/installed/*.toml on one
// install does not get to call itself a security boundary: manifests sit on disk
// long-term and get printed, so credentials must not ride along.
func TestManifestSourceDropsTheCredentials(t *testing.T) {
	s := newStore(t)
	m := sampleManifest("https://example.com/toy.toml", "toy")
	m.Source = "https://alice:hunter2@example.com/toy_1.0_linux_amd64.tar.gz"
	if err := s.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	path, err := s.manifestPath(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("the password reached the disk:\n%s", raw)
	}
	got, err := s.LoadManifest(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://alice:xxxxx@example.com/toy_1.0_linux_amd64.tar.gz"
	if got.Source != want {
		t.Fatalf("source = %q, want %q", got.Source, want)
	}
}

// redactURL must only touch URLs that carry a password, everything else comes
// back untouched. It also runs on "the address the download actually succeeded
// from", so mangling that is lying to the user.
func TestRedactURLLeavesEverythingElseAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"https://example.com/toy.toml",
		"https://alice@example.com/toy.toml", // username only, no password
		"file:///tmp/toy.toml",
		"github:Derrity/tinyserve",
		"not a URL at all @ ://",
	} {
		if got := redactURL(s); got != s {
			t.Errorf("redactURL(%q) = %q, should come back untouched", s, got)
		}
	}
	const in = "https://u:p@example.com/x"
	if got := redactURL(in); got != "https://u:xxxxx@example.com/x" {
		t.Errorf("redactURL(%q) = %q", in, got)
	}
}

// keys/ holds private keys. MkdirAll is a no-op on a directory that already
// exists, so a keys/ created loose once stays loose forever. Open has to tighten
// it.
func TestOpenTightensALoosePreexistingKeysDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dirKeys), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, dirKeys), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.KeysDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("keys/ mode = %o, Open did not tighten the pre-existing directory", perm)
	}
}

// Linking to a directory produces a command that exists but cannot run, which
// is broken the same way a dangling link is.
func TestLinkRejectsADirectoryTarget(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	for _, force := range []bool{false, true} {
		if _, err := s.Link("tool", dir, force); err == nil {
			t.Fatalf("force=%v: linking a command at directory %s must be refused", force, dir)
		}
	}
}

// Command names with leading/trailing whitespace or control characters cannot be
// typed in a shell at all, and "sudo " looks like sudo while slipping past the
// shadow check (LookPath("sudo ") finds nothing).
func TestLinkRejectsUntypableNames(t *testing.T) {
	s := newStore(t)
	sysDir := t.TempDir()
	mkexec(t, sysDir, "sudo")
	t.Setenv("PATH", sysDir)
	target := mkexec(t, t.TempDir(), "payload")

	for _, name := range []string{"sudo ", " sudo", "   ", "a\nb", "a\tb", "a\x00b", "a\x7fb"} {
		t.Run(strconv.Quote(name), func(t *testing.T) {
			if _, err := s.Link(name, target, false); err == nil {
				t.Fatalf("Link(%q) must be refused", name)
			}
			if _, err := s.Link(name, target, true); err == nil {
				t.Fatalf("Link(%q, force) must be refused too — this is a structural error, not a security verdict", name)
			}
		})
	}
	entries, _ := os.ReadDir(s.BinDir())
	if len(entries) != 0 {
		t.Fatalf("%d things showed up in bin/", len(entries))
	}
}

// Values in manifests and config come all the way from toy.toml and the index, so
// stuffing a whole TOML section into one of them must not rewrite another field —
// least of all trust (iron law 8: the trust level must be honest).
func TestDiskFormatResistsTOMLInjection(t *testing.T) {
	s := newStore(t)

	m := sampleManifest("github:a/b", "b")
	m.Trust = TrustUnverified
	m.Signer = "x\"\ntrust = \"" + TrustSigned + "\"\nsigner = \"forged author\"\n\"y"
	m.Source = "u\"\n[[index]]\nname = \"boom\"\n\"v"
	if err := s.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadManifest(m.ID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Trust != TrustUnverified {
		t.Fatalf("the injection worked: trust = %q, want %q", got.Trust, TrustUnverified)
	}
	if got.Signer != m.Signer || got.Source != m.Source {
		t.Fatalf("the values did not round-trip:\n signer = %q\n source = %q", got.Signer, got.Source)
	}

	evil := "e]\n[hosts.pwned]\nraw = \"https://attacker.example.com/{owner}\"\n["
	c := DefaultConfig()
	c.Hosts[evil] = host.Host{Name: evil, Raw: "r", Clone: "c", Release: "l", Web: "w"}
	if err := s.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	back, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, bad := back.Hosts["pwned"]; bad {
		t.Fatal("the injection worked: a pwned host appeared")
	}
	if len(back.Hosts) != 1 || back.Hosts[evil].Raw != "r" {
		t.Fatalf("hosts = %+v", back.Hosts)
	}
}

// Files written by the old hand-rolled encoder (no [hosts] parent header, no
// indentation) must still read back fine: switching to BurntSushi needs no
// migration at all.
func TestReadsTheOldHandWrittenFormat(t *testing.T) {
	s := newStore(t)
	old := `# toy config: indexes, mirrors, and git host overrides. Spec §6.

mirrors = ["https://m.example.com"]

[[index]]
name = "default"
url = "file:///tmp/i.json"

[hosts.gitea]
raw = "https://g/{owner}/{repo}/raw/tag/{ref}/{path}"
clone = "https://g/{owner}/{repo}"
release = "https://g/{owner}/{repo}/releases/download/{tag}/{asset}"
web = "https://g/{owner}/{repo}"
`
	if err := os.WriteFile(filepath.Join(s.Root, configName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Indexes) != 1 || c.Indexes[0].Name != "default" || c.Indexes[0].URL != "file:///tmp/i.json" {
		t.Fatalf("index = %+v", c.Indexes)
	}
	if len(c.Mirrors) != 1 || c.Mirrors[0] != "https://m.example.com" {
		t.Fatalf("mirrors = %+v", c.Mirrors)
	}
	h, ok := c.Hosts["gitea"]
	if !ok || h.Name != "gitea" || h.Clone != "https://g/{owner}/{repo}" {
		t.Fatalf("hosts = %+v", c.Hosts)
	}

	oldManifest := `# toy install manifest, generated.

id = "github:a/b"
name = "b"
version = "1.0.0"
links = ["/x/bin/b"]
trust = "host"
`
	p := filepath.Join(s.Root, dirInstalled, "x.toml")
	if err := os.WriteFile(p, []byte(oldManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "github:a/b" || m.Trust != "host" || len(m.Links) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
}
