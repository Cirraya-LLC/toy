package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Audit: after `--force` installing B over A's command of the same name, A's
// manifest still lists that symlink. `toy uninstall A` follows its own manifest
// and deletes bin/tool — but that link now points at B. B's manifest is
// untouched, `toy list` still claims B is installed, its command has simply
// vanished from PATH, and doctor cannot spot it either (that is not a dangling
// symlink, it is nothing at all).
//
// The fix lives in Link: the moment a symlink changes owner, drop the stale
// record from the old owner's manifest. (Unlink only receives a path, it has no
// "which package is this" information — its signature is fixed by other callers.
// And when insLink rolls back, the manifest has not hit disk yet, so looking the
// owner up through manifests would turn a rollback into "cannot delete". The
// verdict can only be made at the moment the name changes hands.)
func TestUninstallDeletesAnotherPackagesLink(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linkOf := func(id, dir string, force bool) string {
		pkg := filepath.Join(s.Root, dirPkg, dir)
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(pkg, "tool")
		if err := os.WriteFile(target, []byte(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		link, err := s.Link("tool", target, force)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveManifest(Manifest{ID: id, Name: dir, PkgDir: pkg, Links: []string{link}}); err != nil {
			t.Fatal(err)
		}
		return link
	}

	linkOf("local:o/a", "a", false)
	linkB := linkOf("local:o/b", "b", true) // B took "tool" with --force

	// Uninstall A: delete the symlinks listed in the manifest currently on disk,
	// which is exactly what runUninstall does.
	a, err := s.LoadManifest("local:o/a")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range a.Links {
		if err := s.Unlink(l); err != nil {
			t.Fatal(err)
		}
	}

	dest, err := os.Readlink(linkB)
	if err != nil {
		t.Fatalf("uninstalling A deleted B's command %s — A's manifest should have "+
			"stopped listing it the moment the symlink changed owner: %v", linkB, err)
	}
	if filepath.Base(filepath.Dir(dest)) != "b" {
		t.Fatalf("%s points at %s, which is not B's package directory", linkB, dest)
	}
}
