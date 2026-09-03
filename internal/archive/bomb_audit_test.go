package archive

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skipBomb builds a .tar.gz holding a single member of a type we do not recognise,
// whose header declares size bytes of content. After untar's default branch continues
// past it, Go's tar.Reader DECOMPRESSES AND DISCARDS those size bytes from the gzip
// stream. Not one byte lands on disk, so MaxTotalBytes never sees any of it.
func skipBomb(t *testing.T, size int64, typ byte) string {
	t.Helper()
	var raw bytes.Buffer
	raw.Write(rawHdr("blob", typ, "", size, 0o644))
	zeros := make([]byte, 1<<20)
	for w := int64(0); w < size; w += int64(len(zeros)) {
		n := int64(len(zeros))
		if size-w < n {
			n = size - w
		}
		raw.Write(zeros[:n])
	}
	if pad := (512 - raw.Len()%512) % 512; pad > 0 {
		raw.Write(make([]byte, pad))
	}
	raw.Write(make([]byte, 1024))

	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(raw.Bytes())
	zw.Close()

	p := filepath.Join(t.TempDir(), "bomb.tar.gz")
	if err := os.WriteFile(p, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("type %q declares %d bytes, the archive is only %d bytes (ratio %.0f:1)",
		typ, size, gz.Len(), float64(size)/float64(gz.Len()))
	return p
}

// 512 MiB of "skipped" data: how long does extraction take, and what is left on disk?
func TestAuditSkippedMemberBypassesByteLimit(t *testing.T) {
	_, dest := canary(t)
	const size = 512 << 20
	src := skipBomb(t, size, 'Z') // 'Z' matches no known type -> untar takes default: continue

	fi, _ := os.Stat(src)
	start := time.Now()
	files, err := extractTo(src, dest, 1024) // budget of just 1 KiB
	el := time.Since(start)
	t.Logf("download size %d bytes -> extraction took %v, files=%v err=%v", fi.Size(), el, files, err)

	if err != nil {
		t.Logf("refused, good")
		return
	}
	t.Errorf("the limit was 1 KiB, yet %d MiB got decompressed out of the gzip stream in %v "+
		"and the call returned success - MaxTotalBytes only counts bytes that land on disk", size>>20, el)
}

// Control: the same byte count, but as a regular file - that path does have the limit behind it.
func TestAuditRegularMemberIsBounded(t *testing.T) {
	_, dest := canary(t)
	src := skipBomb(t, 512<<20, '0')
	start := time.Now()
	_, err := extractTo(src, dest, 1024)
	t.Logf("regular file: took %v err=%v", time.Since(start), err)
	if err == nil {
		t.Fatalf("a regular file over the limit should be refused")
	}
}

// oldGNUSparseHdr hand-rolls an old-GNU sparse header (typeflag 'S'). Go's reader
// understands it and expands the sparse segments into the full content
// (reader.go handleSparseFile), while KEEPING Typeflag == TypeGNUSparse for the caller.
func oldGNUSparseHdr(name string, payload []byte) []byte {
	b := rawHdr(name, 'S', "", int64(len(payload)), 0o755)
	copy(b[257:265], "ustar  \x00") // GNU magic
	// from offset 386: 4 sparse entries, each offset[12] + numbytes[12]
	copy(b[386:398], fmt.Sprintf("%011o\x00", 0))
	copy(b[398:410], fmt.Sprintf("%011o\x00", len(payload)))
	b[482] = 0                                               // isextended = 0
	copy(b[483:495], fmt.Sprintf("%011o\x00", len(payload))) // realsize
	// recompute the checksum
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

// A GNU sparse file (typeflag 'S') is A REGULAR FILE WITH CONTENT, merely encoded as
// sparse segments. untar's default branch continue'd it away alongside device nodes,
// FIFOs and sockets - so the file vanished silently while Extract returned success with
// no warning anywhere. Reproduce with `tar --sparse -cf x.tar tool` (GNU tar's -S).
func TestAuditGNUSparseSilentlyDropsAFile(t *testing.T) {
	_, dest := canary(t)
	payload := []byte("this is the real payload of the tool binary")
	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("tool", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparse.tar", m.Bytes())

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	if err != nil {
		t.Skipf("Go did not accept this hand-rolled header (%v); reproduce another way", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "tool")); statErr != nil {
		t.Errorf("Extract returned success with files=%v, but tool never landed on disk (%v) - "+
			"the install leaves a command that does not exist, and nothing anywhere says so", files, statErr)
	}
}
