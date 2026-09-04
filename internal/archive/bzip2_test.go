package archive

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bzip2 in the standard library is decompress-only, so the fixtures have to be
// produced by the bzip2 tool. Skip rather than fail where it is missing: this
// tests our reading, not the CI image.
func bzip2File(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 not installed; this fixture cannot be produced from the standard library")
	}
	raw := filepath.Join(dir, name)
	if err := os.WriteFile(raw, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bzip2", "-k", "-f", raw).CombinedOutput(); err != nil {
		t.Fatalf("bzip2: %v: %s", err, out)
	}
	return raw + ".bz2"
}

// restic ships restic_0.19.1_linux_amd64.bz2 and nothing else for linux/amd64 --
// one binary, squeezed, no container. Before this it was not an archive, so it
// fell through to "must be a bare binary then", was chmod +x'd as a bzip2 stream,
// and the shell answered `cannot execute binary file: Exec format error`.
func TestBareBz2IsDecompressedToTheTarget(t *testing.T) {
	dir := t.TempDir()
	src := bzip2File(t, dir, "restic", []byte("#!/bin/sh\necho ok\n"))
	if c := SingleFileCompression(src); c != "bz2" {
		t.Fatalf("SingleFileCompression = %q, want bz2", c)
	}
	dst := filepath.Join(dir, "out", "restic")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Decompress(src, src, dst); err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(b), "echo ok") {
		t.Fatalf("content = %q (%v)", b, err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("mode %04o has no execute bit; the whole point is that it runs", fi.Mode().Perm())
	}
}

// A .tar.gz is a container and must keep going through Extract, not be mistaken
// for a squeezed single file.
func TestTarballsAreNotTreatedAsSingleCompressedFiles(t *testing.T) {
	for _, n := range []string{"x.tar.gz", "x.tgz", "x.tar.bz2", "x.tbz2", "x.zip"} {
		if c := SingleFileCompression(n); c != "" {
			t.Errorf("SingleFileCompression(%q) = %q; that is a container", n, c)
		}
	}
}

// A single compressed file is the easiest decompression bomb there is, and the
// cap has to be on bytes written rather than on anything the file says about
// itself.
func TestDecompressionIsCapped(t *testing.T) {
	dir := t.TempDir()
	// A megabyte of zeroes compresses to almost nothing and expands right back.
	src := bzip2File(t, dir, "bomb", make([]byte, 1<<20))
	dst := filepath.Join(dir, "out")
	if err := decompressTo(src, src, dst, 4096); err == nil {
		t.Fatal("expanding past the limit must be refused")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("the partial file must be deleted")
	}
}
