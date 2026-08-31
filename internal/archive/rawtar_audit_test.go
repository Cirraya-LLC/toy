package archive

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// rawHdr 手搓一个 512 字节的 ustar 头。Go 的 tar.Writer 会替我们"消毒"
// （比如 PAXRecords 里的 path/linkpath 会被 Header 字段覆盖掉、名字里有 NUL
// 直接拒绝写），而真正的攻击者是自己拼字节的 —— 所以要绕开 Writer。
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
	// checksum：先填空格算和
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

// rawTar 把若干手搓成员拼成一个 tar 文件。
func rawTar(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range parts {
		buf.Write(p)
	}
	buf.Write(make([]byte, 1024)) // 两个空块收尾
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// paxMember 造一个 PAX 扩展头（typeflag 'x'）+ 紧跟着的真实成员。
// 扩展头里的 path/linkpath 会盖掉后面那个成员头里的短名字。
func paxMember(records map[string]string, next []byte) []byte {
	var rec bytes.Buffer
	for k, v := range records {
		// 格式："%d %s=%s\n"，长度包含它自己
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

// gnuLongName 造一个 GNU 'L' 长名头 + 紧跟着的真实成员。
func gnuLong(typ byte, longname string, next []byte) []byte {
	var out bytes.Buffer
	body := append([]byte(longname), 0)
	out.Write(rawHdr("././@LongLink", typ, "", int64(len(body)), 0o644))
	rawBody(&out, body)
	out.Write(next)
	return out.Bytes()
}
