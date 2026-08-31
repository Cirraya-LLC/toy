package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 独立复核：PAX path 记录里的穿越到底会不会被拒。
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
	// 真正的判据：destDir 外面有没有多出文件
	if _, serr := os.Stat(filepath.Join(root, "SECRET")); serr == nil {
		t.Fatal("写到 destDir 外面去了")
	}
	// 再看 tar reader 到底把 Name 解成了什么
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	h, _ := tr.Next()
	t.Logf("reader 看到的 Name=%q", h.Name)
}
