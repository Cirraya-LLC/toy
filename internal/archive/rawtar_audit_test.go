package archive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// rawHdr hand-rolls a 512-byte ustar header. Go's tar.Writer sanitises for us - a
// PAXRecords path/linkpath gets overwritten by the Header fields, a name containing NUL
// is refused outright - while a real attacker assembles the bytes himself. So we go
// around the Writer.
func rawHdr(name string, typ byte, link string, size int64, mode int64) []byte {
	b := make([]byte, 512)
	put := func(off int, s string, n int) {
		copy(b[off:off+n], s)
	}
	put(0, name, 100)
	put(100, fmt.Sprintf("%07o\x00", mode&0o7777), 8)
	put(108, "0000000\x00", 8) // uid
	put(116, "0000000\x00", 8) // gid
	put(124, fmt.Sprintf("%011o\x00", size), 12)
	put(136, fmt.Sprintf("%011o\x00", 0), 12) // mtime
	b[156] = typ
	put(157, link, 100)
	put(257, "ustar\x0000", 8)
	// checksum: fill the field with spaces first, then sum
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	put(148, fmt.Sprintf("%06o\x00 ", sum), 8)
	return b
}

func rawBody(buf *bytes.Buffer, body []byte) {
	buf.Write(body)
	if pad := (512 - len(body)%512) % 512; pad > 0 {
		buf.Write(make([]byte, pad))
	}
}

// rawTar concatenates hand-rolled members into one tar file.
func rawTar(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range parts {
		buf.Write(p)
	}
	buf.Write(make([]byte, 1024)) // two empty blocks to terminate
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// paxMember builds a PAX extended header (typeflag 'x') followed by the real member.
// The path/linkpath in the extended header overrides the short name in that member's
// own header.
func paxMember(records map[string]string, next []byte) []byte {
	var rec bytes.Buffer
	for k, v := range records {
		// format: "%d %s=%s\n", where the length counts itself
		line := fmt.Sprintf(" %s=%s\n", k, v)
		n := len(line) + 1
		for {
			if len(fmt.Sprint(n))+len(line) == n {
				break
			}
			n = len(fmt.Sprint(n)) + len(line)
		}
		rec.WriteString(fmt.Sprintf("%d%s", n, line))
	}
	var out bytes.Buffer
	out.Write(rawHdr("PaxHeaders/x", 'x', "", int64(rec.Len()), 0o644))
	rawBody(&out, rec.Bytes())
	out.Write(next)
	return out.Bytes()
}

// gnuLong builds a GNU 'L' long-name header followed by the real member.
func gnuLong(typ byte, longname string, next []byte) []byte {
	var out bytes.Buffer
	body := append([]byte(longname), 0)
	out.Write(rawHdr("././@LongLink", typ, "", int64(len(body)), 0o644))
	rawBody(&out, body)
	out.Write(next)
	return out.Bytes()
}
