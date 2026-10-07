package archive

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// This is the archive the system tar writes on a recent Mac: every member that
// carries an extended attribute gets a `._name` AppleDouble twin, including the
// wrapper directory itself. `tar tzf` on a Mac hides them when listing, and CI's
// macOS runner never produced them, so this was first seen on a real Mac where
// `toy install` of such a package failed with "link target ... is unusable".
//
// The members are written by hand so the test does not depend on the machine
// running it having that attribute.
func TestAppleDoubleMembersDoNotBlockTheWrapperStrip(t *testing.T) {
	src := buildTar(t, "pkg.tar.gz", true, []tarEntry{
		reg("._tool-1.0", "apple double for the directory"),
		{name: "tool-1.0/", typ: tar.TypeDir, mode: 0o755},
		reg("tool-1.0/._tool", "apple double for the binary"),
		{name: "tool-1.0/tool", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho ok\n"},
	})
	dest := t.TempDir()
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Fatalf("the wrapper was not stripped -- tool is not at the top: %v", err)
	}
	assertNoMacMetadata(t, dest)
}

// Finder's "Compress" puts the same metadata under a __MACOSX/ tree in a zip.
func TestMacOSXTreeInAZipIsIgnored(t *testing.T) {
	src := buildZip(t, "pkg.zip", []zipEntry{
		{name: "__MACOSX/", mode: os.ModeDir | 0o755},
		{name: "__MACOSX/tool-1.0/._tool", mode: 0o644, body: "metadata"},
		{name: "tool-1.0/", mode: os.ModeDir | 0o755},
		{name: "tool-1.0/tool", mode: 0o755, body: "#!/bin/sh\necho ok\n"},
	})
	dest := t.TempDir()
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Fatalf("the wrapper was not stripped -- tool is not at the top: %v", err)
	}
	assertNoMacMetadata(t, dest)
}

// Skipping metadata happens after the path checks, so a traversal attempt named
// like metadata still rejects the whole archive rather than being quietly dropped.
func TestTraversalDisguisedAsMetadataIsStillRefused(t *testing.T) {
	src := buildTar(t, "evil.tar.gz", true, []tarEntry{
		reg("../._evil", "x"),
		reg("tool", "ok"),
	})
	_, err := Extract(src, t.TempDir())
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("want ErrTraversal, got %v", err)
	}
}

func assertNoMacMetadata(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && macMetadata(d.Name()) {
			t.Errorf("macOS metadata was written into the package: %s", p)
		}
		return nil
	})
}
