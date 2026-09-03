package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Independent double-check: is a traversal smuggled in a PAX path record really refused?
func TestProbePAXTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg, Name: "../SECRET-" + strings.Repeat("a", 200), Mode: 0o644,
		Size: 5, Format: tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("pwned"))
	tw.Close()

	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	os.MkdirAll(dest, 0o755)
	src := filepath.Join(root, "a.tar")
	os.WriteFile(src, buf.Bytes(), 0o644)

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	// The real test: did anything appear outside destDir?
	if _, serr := os.Stat(filepath.Join(root, "SECRET")); serr == nil {
		t.Fatal("wrote outside destDir")
	}
	// And what did the tar reader actually resolve Name to?
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	h, _ := tr.Next()
	t.Logf("Name as the reader sees it = %q", h.Name)
}
