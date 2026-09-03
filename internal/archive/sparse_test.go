package archive

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A GNU sparse file (`tar --sparse -cf x.tar tool`) carries typeflag 'S'. Go's reader
// understands it and hands us the fully expanded content, but KEEPS that typeflag. The
// moment it falls into untar's default branch it gets continue'd away alongside device
// nodes: the file vanishes silently, Extract still reports success, and the install
// leaves behind a command that does not exist. This asserts the content really lands.
func TestGNUSparseFileIsExtractedWithItsContent(t *testing.T) {
	outside, dest := canary(t)
	payload := []byte("this is the real payload of the tool binary")

	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("tool", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparse.tar", m.Bytes())

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("a sparse file should extract normally, got err=%v", err)
	}
	canaryIntact(t, outside)
	if len(files) != 1 || files[0] != "tool" {
		t.Fatalf("file list = %v, want [tool]", files)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatalf("tool never landed on disk: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content = %q, want %q", got, payload)
	}
}

// An expanded sparse file can dwarf the archive itself, so the byte limit still applies.
func TestGNUSparseFileStillHitsTheByteLimit(t *testing.T) {
	_, dest := canary(t)
	payload := bytes.Repeat([]byte("A"), 4096)

	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("big", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparsebig.tar", m.Bytes())

	files, err := extractTo(src, dest, 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a 4 KiB sparse file under a 1 KiB limit should be ErrTooLarge, got files=%v err=%v", files, err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "big")); !os.IsNotExist(statErr) {
		t.Fatalf("a failure must not leave half a file behind: %v", statErr)
	}
}

// An unknown typeflag that declares content cannot just be continue'd away: Go's reader
// decompresses those bytes out of the gzip stream and discards them, MaxTotalBytes never
// sees them (a few KiB of archive can burn hundreds of MiB of decompression), and the
// package silently installs with something missing. It has to fail loudly.
func TestUnknownTypeflagWithContentIsRejected(t *testing.T) {
	_, dest := canary(t)
	body := []byte("payload nobody will ever see")

	var m bytes.Buffer
	m.Write(rawHdr("blob", 'Z', "", int64(len(body)), 0o644))
	rawBody(&m, body)
	src := rawTar(t, "unknown.tar", m.Bytes())

	files, err := Extract(src, dest)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an unknown type carrying content should be refused, got files=%v err=%v", files, err)
	}
}

// Control: contentless irregular members (device nodes, FIFOs, PAX global headers) are
// still skipped quietly. The rule above must not reject every archive git archive
// produces.
func TestContentlessIrregularMembersStillSkipped(t *testing.T) {
	_, dest := canary(t)
	var m bytes.Buffer
	m.Write(rawHdr("dev/null", '3', "", 0, 0o666))  // char
	m.Write(rawHdr("dev/loop0", '4', "", 0, 0o660)) // block
	m.Write(rawHdr("run/pipe", '6', "", 0, 0o600))  // fifo
	m.Write(rawHdr("volume-label", 'V', "", 0, 0o644))
	body := []byte("ok")
	m.Write(rawHdr("real.txt", '0', "", int64(len(body)), 0o644))
	rawBody(&m, body)
	src := rawTar(t, "irregular.tar", m.Bytes())

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("contentless irregular members should be skipped, not error out, got %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("file list = %v, want real.txt only", files)
	}
}
