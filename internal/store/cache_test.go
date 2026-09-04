package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The cache is deliberately kept when a package is uninstalled: it is
// content-addressed and it is what `toy serve` mirrors. That is a reason to keep
// some of it, and no reason to keep everything forever -- so pruning has to know
// which files an installed package still refers to.
func TestPruneKeepsWhatIsStillInstalled(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const used = "aa11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddee"
	const dead = "1111111111111111111111111111111111111111111111111111111111111111"
	for name, size := range map[string]int{used: 10, dead: 40} {
		if err := os.WriteFile(filepath.Join(st.CacheDir(), name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file that is not a sha256 is somebody else's; prune must never touch it.
	stray := filepath.Join(st.CacheDir(), "notes.txt")
	if err := os.WriteFile(stray, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveManifest(Manifest{ID: "github:a/b", Name: "b", Version: "1", SHA256: used}); err != nil {
		t.Fatal(err)
	}

	es, err := st.CacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("expected the two sha256 files and not %s, got %d entries", filepath.Base(stray), len(es))
	}

	n, freed, err := st.PruneCache(false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != 40 {
		t.Fatalf("prune removed %d files freeing %d bytes, want 1 and 40", n, freed)
	}
	if _, err := os.Stat(filepath.Join(st.CacheDir(), used)); err != nil {
		t.Fatalf("the file an installed package still refers to was deleted: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("prune deleted a file it did not put there: %v", err)
	}

	// --all is the only way to drop the ones still in use.
	if n, _, err := st.PruneCache(true); err != nil || n != 1 {
		t.Fatalf("PruneCache(true) removed %d, want 1 (err %v)", n, err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("--all still must not touch a file we did not write: %v", err)
	}
}
