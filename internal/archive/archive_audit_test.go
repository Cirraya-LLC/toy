package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func umaskSet(m int) int { return syscall.Umask(m) }

// ---- 审计用的造包工具：比 archive_test.go 那套多几个自由度 ----

type advEntry struct {
	name   string
	typ    byte
	mode   int64
	link   string
	body   string
	format tar.Format // 0 = 让 Go 自己挑
	pax    map[string]string
}

func buildAdvTar(t *testing.T, name string, gz bool, entries []advEntry) string {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:       e.name,
			Typeflag:   e.typ,
			Mode:       e.mode,
			Linkname:   e.link,
			Size:       int64(len(e.body)),
			Format:     e.format,
			PAXRecords: e.pax,
		}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写 tar 头 %q 失败：%v", e.name, err)
		}
		if e.typ == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("写内容失败：%v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar 失败：%v", err)
	}
	out := raw.Bytes()
	if gz {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		zw.Write(out)
		zw.Close()
		out = zb.Bytes()
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	return p
}

// canary 在 destDir 的【外面】放一个哨兵文件，用来证明包没写/没删到界外。
func canary(t *testing.T) (outside string, dest string) {
	t.Helper()
	root := t.TempDir()
	outside = filepath.Join(root, "SECRET")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest = filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	return outside, dest
}

func canaryIntact(t *testing.T, outside string) {
	t.Helper()
	b, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("界外哨兵被删了/动了：%v", err)
	}
	if string(b) != "original" {
		t.Fatalf("界外哨兵被改写了：%q", b)
	}
}

// ---------- A. PAX / GNU longname 扩展头里藏路径（手搓字节，绕开 Go 的 Writer） ----------

func TestAuditRawPAXPathSmugglesTraversal(t *testing.T) {
	outside, dest := canary(t)
	body := []byte("pwned")
	inner := rawHdr("harmless.txt", '0', "", int64(len(body)), 0o644)
	var m bytes.Buffer
	m.Write(inner)
	rawBody(&m, body)
	src := rawTar(t, "pax.tar", paxMember(map[string]string{"path": "../SECRET"}, m.Bytes()))

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("PAX path=../SECRET 应该被拒，实际 err=%v files=%v", err, files)
	}
}

func TestAuditRawPAXLinkpathSmugglesTraversal(t *testing.T) {
	outside, dest := canary(t)
	inner := rawHdr("l", '2', "harmless", 0, 0o777)
	src := rawTar(t, "paxl.tar", paxMember(map[string]string{"linkpath": "../SECRET"}, inner))

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if err == nil {
		if tgt, e := os.Readlink(filepath.Join(dest, "l")); e == nil && strings.Contains(tgt, "..") {
			t.Fatalf("PAX linkpath 把一个指向界外的链接放进了包：%s -> %s", "l", tgt)
		}
	}
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("PAX linkpath=../SECRET 应该被拒，实际 err=%v", err)
	}
}

func TestAuditRawGNULongNameSmugglesTraversal(t *testing.T) {
	outside, dest := canary(t)
	long := "../SECRET"
	body := []byte("pwned")
	inner := rawHdr("harmless.txt", '0', "", int64(len(body)), 0o644)
	var m bytes.Buffer
	m.Write(inner)
	rawBody(&m, body)
	src := rawTar(t, "gnu.tar", gnuLong('L', long, m.Bytes()))

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("GNU longname=../SECRET 应该被拒，实际 err=%v", err)
	}
}

func TestAuditRawGNULongLinkSmugglesTraversal(t *testing.T) {
	outside, dest := canary(t)
	inner := rawHdr("l", '2', "harmless", 0, 0o777)
	src := rawTar(t, "gnuk.tar", gnuLong('K', "../SECRET", inner))

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if err == nil {
		if tgt, e := os.Readlink(filepath.Join(dest, "l")); e == nil {
			t.Fatalf("GNU longlink 放进了一个指向 %q 的链接", tgt)
		}
	}
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("GNU longlink=../SECRET 应该被拒，实际 err=%v", err)
	}
}

// 名字里带 NUL：Go 的 tar.Writer 拒绝生成，只能手搓
func TestAuditRawNULInName(t *testing.T) {
	for _, n := range []string{"a\x00/../../SECRET", "\x00", "ok\x00.txt"} {
		t.Run(strings.ReplaceAll(n, "\x00", "NUL"), func(t *testing.T) {
			outside, dest := canary(t)
			body := []byte("pwned")
			var m bytes.Buffer
			m.Write(rawHdr(n, '0', "", int64(len(body)), 0o644))
			rawBody(&m, body)
			src := rawTar(t, "nul.tar", m.Bytes())
			files, err := Extract(src, dest)
			t.Logf("name=%q -> files=%v err=%v", n, files, err)
			canaryIntact(t, outside)
			root := filepath.Dir(dest)
			ents, _ := os.ReadDir(root)
			for _, e := range ents {
				if e.Name() != "SECRET" && e.Name() != "dest" {
					t.Fatalf("界外多出了 %q", e.Name())
				}
			}
		})
	}
}

// 绝对路径的各种写法（手搓，Go 的 Writer 也会挑剔）
func TestAuditRawAbsoluteNames(t *testing.T) {
	for _, n := range []string{"/SECRET", "//SECRET", "/./SECRET", "///../SECRET"} {
		t.Run(n, func(t *testing.T) {
			outside, dest := canary(t)
			body := []byte("pwned")
			var m bytes.Buffer
			m.Write(rawHdr(n, '0', "", int64(len(body)), 0o644))
			rawBody(&m, body)
			src := rawTar(t, "abs.tar", m.Bytes())
			_, err := Extract(src, dest)
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("绝对路径 %q 应该被拒，实际 %v", n, err)
			}
			canaryIntact(t, outside)
		})
	}
}

// ---------- B. 成员名里带 NUL / 空白 / 纯点 ----------

func TestAuditDotAndEmptyNames(t *testing.T) {
	_, dest := canary(t)
	for _, e := range []advEntry{
		{name: "", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: ".", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "   ", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "..", typ: tar.TypeDir, mode: 0o755},
	} {
		src := buildAdvTar(t, "dot.tar", false, []advEntry{e})
		files, err := Extract(src, dest)
		t.Logf("name=%q -> files=%v err=%v", e.name, files, err)
		os.RemoveAll(dest)
		os.Mkdir(dest, 0o755)
	}
}

// ---------- C. 同名成员出现两次 ----------

func TestAuditDuplicateFileThenSymlinkThenHardlink(t *testing.T) {
	outside, dest := canary(t)
	// 经典绕过：第一次以普通文件登记进 e.regular，第二次把同一个名字换成符号
	// 链接（e.regular 那条记录没被清），第三次拿硬链接去链它。
	src := buildAdvTar(t, "dup.tar", false, []advEntry{
		{name: "a", typ: tar.TypeReg, mode: 0o644, body: "real"},
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		{name: "a", typ: tar.TypeSymlink, mode: 0o777, link: "sub"},
		{name: "b", typ: tar.TypeLink, mode: 0o644, link: "a"},
	})
	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if err == nil {
		fi, lerr := os.Lstat(filepath.Join(dest, "b"))
		if lerr == nil {
			t.Logf("b 的模式=%v（符号链接？%v）", fi.Mode(), fi.Mode()&os.ModeSymlink != 0)
		}
	}
}

func TestAuditDuplicateSymlinkThenWriteThrough(t *testing.T) {
	outside, dest := canary(t)
	// 两步走：先建一个指向 dest 内部目录的链接，再通过它写。
	src := buildAdvTar(t, "two.tar", false, []advEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "sub"},
		{name: "link/evil", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
	})
	_, err := Extract(src, dest)
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("穿过符号链接往里写应该被拒，实际 err=%v", err)
	}
	canaryIntact(t, outside)
}

func TestAuditFileThenDirSameName(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "fd.tar", false, []advEntry{
		{name: "x", typ: tar.TypeReg, mode: 0o644, body: "file"},
		{name: "x", typ: tar.TypeDir, mode: 0o755},
		{name: "x/y", typ: tar.TypeReg, mode: 0o644, body: "under"},
	})
	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	if err == nil {
		t.Fatalf("同名先文件后目录竟然成功了")
	}
	mustBeEmpty(t, dest)
}

// ---------- D. zip：目录项和文件项同名 / 反斜杠 / 符号链接 ----------

func TestAuditZipDirEntryAndFileEntrySameName(t *testing.T) {
	_, dest := canary(t)
	src := buildZip(t, "dup.zip", []zipEntry{
		{name: "x/", mode: os.ModeDir | 0o755},
		{name: "x", mode: 0o644, body: "clash"},
	})
	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
}

func TestAuditZipSymlinkThenWriteThrough(t *testing.T) {
	outside, dest := canary(t)
	src := buildZip(t, "sl.zip", []zipEntry{
		{name: "sub/", mode: os.ModeDir | 0o755},
		{name: "link", mode: os.ModeSymlink | 0o777, body: "sub"},
		{name: "link/evil", mode: 0o644, body: "pwned"},
	})
	_, err := Extract(src, dest)
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("zip 穿链写入应该被拒，实际 err=%v", err)
	}
	canaryIntact(t, outside)
}

func TestAuditZipBackslashEscape(t *testing.T) {
	outside, dest := canary(t)
	for _, n := range []string{`..\SECRET`, `..\..\SECRET`, `sub\..\..\SECRET`} {
		src := buildZip(t, "bs.zip", []zipEntry{{name: n, mode: 0o644, body: "pwned"}})
		_, err := Extract(src, dest)
		if !errors.Is(err, ErrTraversal) {
			t.Fatalf("zip 名字 %q 应该被拒，实际 err=%v", n, err)
		}
		canaryIntact(t, outside)
	}
}

func TestAuditZipSymlinkAbsoluteAndOutside(t *testing.T) {
	outside, dest := canary(t)
	for _, target := range []string{"/etc", "../", "../SECRET", "../../"} {
		src := buildZip(t, "slt.zip", []zipEntry{{name: "l", mode: os.ModeSymlink | 0o777, body: target}})
		_, err := Extract(src, dest)
		if !errors.Is(err, ErrTraversal) {
			t.Fatalf("zip 符号链接指向 %q 应该被拒，实际 err=%v", target, err)
		}
		canaryIntact(t, outside)
	}
}

// ---------- E. 压缩炸弹：上限按【实际写入】还是按【自报大小】 ----------

func TestAuditZipBombCountsRealBytesNotDeclared(t *testing.T) {
	_, dest := canary(t)
	// 100 MiB 的零，deflate 之后几十 KB。limit 给 1 MiB。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("bomb")
	chunk := make([]byte, 1<<20)
	for i := 0; i < 100; i++ {
		w.Write(chunk)
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "bomb.zip")
	os.WriteFile(p, buf.Bytes(), 0o644)

	_, err := extractTo(p, dest, 1<<20)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("zip 炸弹应该被 ErrTooLarge 拦下，实际 %v", err)
	}
	mustBeEmpty(t, dest)
	// 落盘的字节不能远远超出上限
}

func TestAuditTarLyingSizeHeader(t *testing.T) {
	_, dest := canary(t)
	// 手搓一个 tar 头：Size 声明 10，实际内容远不止（靠连续两个成员）。
	// Go 的 tar reader 会按声明截断，所以这里主要验"声明小实际大"不会让上限失效。
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	big := strings.Repeat("A", 3<<20)
	tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(big))})
	tw.Write([]byte(big))
	tw.Close()
	p := filepath.Join(t.TempDir(), "big.tar")
	os.WriteFile(p, raw.Bytes(), 0o644)

	_, err := extractTo(p, dest, 1<<20)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超上限应该 ErrTooLarge，实际 %v", err)
	}
	mustBeEmpty(t, dest)
}

// ---------- F. 权限位 ----------

func TestAuditSetuidSetgidStickyAreStripped(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "perm.tar", false, []advEntry{
		{name: "suid", typ: tar.TypeReg, mode: 0o4755, body: "x"},
		{name: "sgid", typ: tar.TypeReg, mode: 0o2755, body: "x"},
		{name: "sticky", typ: tar.TypeReg, mode: 0o1755, body: "x"},
		{name: "all", typ: tar.TypeReg, mode: 0o7777, body: "x"},
		{name: "ww", typ: tar.TypeReg, mode: 0o666, body: "x"},
	})
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	for _, n := range []string{"suid", "sgid", "sticky", "all", "ww"} {
		fi, err := os.Lstat(filepath.Join(dest, n))
		if err != nil {
			t.Fatalf("%s 不见了：%v", n, err)
		}
		m := fi.Mode()
		if m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Fatalf("%s 带出了特殊位：%v", n, m)
		}
		if p := m.Perm(); p != 0o755 && p != 0o644 {
			t.Fatalf("%s 权限 %o 不是两档之一", n, p)
		}
		if p := m.Perm(); p&0o022 != 0 {
			t.Fatalf("%s 是组/其他可写：%o", n, p)
		}
	}
}

func TestAuditZipSetuidStripped(t *testing.T) {
	_, dest := canary(t)
	src := buildZip(t, "suid.zip", []zipEntry{
		{name: "s", mode: os.ModeSetuid | 0o755, body: "x"},
	})
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	fi, _ := os.Lstat(filepath.Join(dest, "s"))
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("zip setuid 带出来了：%v", fi.Mode())
	}
}

// umask 之后 chmod 是不是反而放宽了权限
func TestAuditChmodWidensPastUmask(t *testing.T) {
	old := umaskSet(0o077)
	defer umaskSet(old)
	_, dest := canary(t)
	src := buildAdvTar(t, "um.tar", false, []advEntry{
		{name: "d", typ: tar.TypeDir, mode: 0o755},
		{name: "d/f", typ: tar.TypeReg, mode: 0o755, body: "x"},
	})
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	fd, _ := os.Lstat(filepath.Join(dest, "d"))
	ff, _ := os.Lstat(filepath.Join(dest, "d", "f"))
	t.Logf("umask 077 下：目录 %o 文件 %o", fd.Mode().Perm(), ff.Mode().Perm())
}

// ---------- G. 清理逻辑会不会删到界外 ----------

func TestAuditCleanupCannotDeleteOutsideDest(t *testing.T) {
	outside, dest := canary(t)
	// dest 里预先放一个指向界外的符号链接（模拟"目标目录里本来就有别人的东西"），
	// 再让包在这条链上做文章，最后故意超上限触发 cleanup。
	if err := os.Symlink(filepath.Dir(dest), filepath.Join(dest, "esc")); err != nil {
		t.Fatal(err)
	}
	src := buildAdvTar(t, "cl.tar", false, []advEntry{
		{name: "esc/SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
	})
	_, err := Extract(src, dest)
	t.Logf("err=%v", err)
	canaryIntact(t, outside)
}

func TestAuditCleanupDoesNotFollowPackageSymlinkOut(t *testing.T) {
	outside, dest := canary(t)
	src := buildAdvTar(t, "cl2.tar", false, []advEntry{
		{name: "keep", typ: tar.TypeReg, mode: 0o644, body: "a"},
		{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "keep"},
		{name: "boom", typ: tar.TypeReg, mode: 0o644, body: strings.Repeat("B", 4096)},
	})
	_, err := extractTo(src, dest, 64)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("想要 ErrTooLarge，实际 %v", err)
	}
	mustBeEmpty(t, dest)
	canaryIntact(t, outside)
}

// ---------- H. 符号链接逃逸的各种变体 ----------

func TestAuditSymlinkEscapeVariants(t *testing.T) {
	cases := []struct {
		name    string
		entries []advEntry
	}{
		{"直接指绝对路径", []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "/etc/passwd"},
		}},
		{"直接指上一层", []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "../SECRET"},
		}},
		{"点自己再回头", []advEntry{
			{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "x/up", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
			{name: "x/up/SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
		}},
		{"深目录里的链接回头", []advEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755},
			{name: "a/b", typ: tar.TypeDir, mode: 0o755},
			{name: "a/b/l", typ: tar.TypeSymlink, mode: 0o777, link: "../.."},
			{name: "a/b/l/../SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
		}},
		{"链接目标里夹一个回头的 ..", []advEntry{
			{name: "d", typ: tar.TypeDir, mode: 0o755},
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "d/../.."},
		}},
		{"链接指向另一个链接再出界", []advEntry{
			{name: "a", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "b", typ: tar.TypeSymlink, mode: 0o777, link: "a/.."},
		}},
		{"多级回头但落点在界内", []advEntry{
			{name: "p", typ: tar.TypeDir, mode: 0o755},
			{name: "p/q", typ: tar.TypeDir, mode: 0o755},
			{name: "p/q/l", typ: tar.TypeSymlink, mode: 0o777, link: "../.."},
			{name: "p/q/l/SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned-inside"},
		}},
		{"反斜杠当分隔符的链接目标", []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: `..\..\SECRET`},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outside, dest := canary(t)
			src := buildAdvTar(t, "esc.tar", false, c.entries)
			files, err := Extract(src, dest)
			t.Logf("files=%v err=%v", files, err)
			canaryIntact(t, outside)
			// 无论成功失败，界外都不能多出东西
			root := filepath.Dir(dest)
			ents, _ := os.ReadDir(root)
			for _, e := range ents {
				if e.Name() != "SECRET" && e.Name() != "dest" {
					t.Fatalf("界外多出了 %s", e.Name())
				}
			}
		})
	}
}

// ---------- I. 硬链接 ----------

func TestAuditHardlinkVariants(t *testing.T) {
	cases := []struct {
		name    string
		entries []advEntry
	}{
		{"指界外绝对路径", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "/etc/passwd"},
		}},
		{"指界外相对路径", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "../SECRET"},
		}},
		{"指一个还没出现的成员", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "later"},
			{name: "later", typ: tar.TypeReg, mode: 0o644, body: "x"},
		}},
		{"穿过包内符号链接指出去", []advEntry{
			{name: "d", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "d/../SECRET"},
		}},
		{"指一个包内符号链接", []advEntry{
			{name: "f", typ: tar.TypeReg, mode: 0o644, body: "x"},
			{name: "s", typ: tar.TypeSymlink, mode: 0o777, link: "f"},
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "s"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outside, dest := canary(t)
			src := buildAdvTar(t, "hl.tar", false, c.entries)
			files, err := Extract(src, dest)
			t.Logf("files=%v err=%v", files, err)
			canaryIntact(t, outside)
		})
	}
}

// dest 里预先躺着一个指向界外的符号链接，包用硬链接去链它
func TestAuditHardlinkThroughPreexistingSymlink(t *testing.T) {
	outside, dest := canary(t)
	if err := os.Symlink(outside, filepath.Join(dest, "pre")); err != nil {
		t.Fatal(err)
	}
	src := buildAdvTar(t, "pre.tar", false, []advEntry{
		{name: "h", typ: tar.TypeLink, mode: 0o644, link: "pre"},
	})
	_, err := Extract(src, dest)
	if err == nil {
		t.Fatalf("硬链接到界外的既有链接应该被拒")
	}
	t.Logf("err=%v", err)
	canaryIntact(t, outside)
}

// ---------- J. 超长路径 ----------

func TestAuditVeryLongPath(t *testing.T) {
	_, dest := canary(t)
	deep := strings.Repeat("d/", 900) + "f"
	src := buildAdvTar(t, "deep.tar", true, []advEntry{
		{name: deep, typ: tar.TypeReg, mode: 0o644, body: "x", format: tar.FormatPAX},
	})
	files, err := Extract(src, dest)
	t.Logf("深度 900 -> files=%d err=%v", len(files), err)
	// 只要不 panic、不写到界外就算过
}

func TestAuditLongSingleComponent(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "longc.tar", true, []advEntry{
		{name: strings.Repeat("x", 5000), typ: tar.TypeReg, mode: 0o644, body: "x", format: tar.FormatPAX},
	})
	files, err := Extract(src, dest)
	t.Logf("单段 5000 字符 -> files=%v err=%v", files, err)
}

// ---------- K. 非常规成员 ----------

func TestAuditIrregularAndGlobalHeaders(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "irr.tar", false, []advEntry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader, format: tar.FormatPAX,
			pax: map[string]string{"comment": "hi"}},
		{name: "real.txt", typ: tar.TypeReg, mode: 0o644, body: "ok"},
	})
	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	if err != nil {
		t.Fatalf("git archive 生成的 tar 都带 pax_global_header，不该整包失败：%v", err)
	}
}

func TestAuditEmptyNameGlobalHeader(t *testing.T) {
	_, dest := canary(t)
	// 有些打包器写的全局头名字是空的
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	tw.WriteHeader(&tar.Header{Name: "", Typeflag: tar.TypeXGlobalHeader,
		Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": "x"}})
	tw.WriteHeader(&tar.Header{Name: "real.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	tw.Write([]byte("ok"))
	tw.Close()
	p := filepath.Join(t.TempDir(), "g.tar")
	os.WriteFile(p, raw.Bytes(), 0o644)
	files, err := Extract(p, dest)
	t.Logf("空名全局头 -> files=%v err=%v", files, err)
}

func TestAuditGNUSparseFileIsHandled(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "sp.tar", false, []advEntry{
		{name: "s", typ: tar.TypeGNUSparse, mode: 0o644, format: tar.FormatGNU},
	})
	files, err := Extract(src, dest)
	t.Logf("GNUSparse -> files=%v err=%v", files, err)
}

// ---------- L. zip 的成员顺序把戏 ----------

func TestAuditZipSymlinkThenFileSameName(t *testing.T) {
	outside, dest := canary(t)
	src := buildZip(t, "so.zip", []zipEntry{
		{name: "sub/", mode: os.ModeDir | 0o755},
		{name: "a", mode: os.ModeSymlink | 0o777, body: "sub"},
		{name: "a", mode: 0o644, body: "overwrite"},
	})
	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	canaryIntact(t, outside)
	if err == nil {
		fi, _ := os.Lstat(filepath.Join(dest, "a"))
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("同名的普通文件没能把符号链接换掉，a 还是个链接")
		}
	}
}

// 空目标 / 只有斜杠的链接目标
func TestAuditDegenerateLinkTargets(t *testing.T) {
	for _, tgt := range []string{"", " ", "/", "//", ".", "./"} {
		outside, dest := canary(t)
		src := buildAdvTar(t, "d.tar", false, []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: tgt},
		})
		files, err := Extract(src, dest)
		t.Logf("target=%q -> files=%v err=%v", tgt, files, err)
		canaryIntact(t, outside)
	}
}
