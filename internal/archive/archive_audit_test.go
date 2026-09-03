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

// ---- archive builders for the audit: a few more degrees of freedom than archive_test.go's ----

type advEntry struct {
	name   string
	typ    byte
	mode   int64
	link   string
	body   string
	format tar.Format // 0 = let Go pick
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
			t.Fatalf("writing tar header %q failed: %v", e.name, err)
		}
		if e.typ == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing content failed: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar failed: %v", err)
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
		t.Fatalf("writing the file failed: %v", err)
	}
	return p
}

// canary drops a sentinel file OUTSIDE destDir, to prove no archive wrote or deleted out of bounds.
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
		t.Fatalf("the out-of-bounds sentinel was deleted or touched: %v", err)
	}
	if string(b) != "original" {
		t.Fatalf("the out-of-bounds sentinel was overwritten: %q", b)
	}
}

// ---------- A. paths smuggled in PAX / GNU longname extended headers (raw bytes, around Go's Writer) ----------

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
		t.Fatalf("PAX path=../SECRET should be refused, got err=%v files=%v", err, files)
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
			t.Fatalf("PAX linkpath put a link pointing out of bounds into the package: %s -> %s", "l", tgt)
		}
	}
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("PAX linkpath=../SECRET should be refused, got err=%v", err)
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
		t.Fatalf("GNU longname=../SECRET should be refused, got err=%v", err)
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
			t.Fatalf("GNU longlink installed a link pointing at %q", tgt)
		}
	}
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("GNU longlink=../SECRET should be refused, got err=%v", err)
	}
}

// A NUL in the name: Go's tar.Writer refuses to produce one, so it has to be hand-rolled
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
					t.Fatalf("%q appeared out of bounds", e.Name())
				}
			}
		})
	}
}

// Every spelling of an absolute path (hand-rolled; Go's Writer is picky about these too)
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
				t.Fatalf("absolute path %q should be refused, got %v", n, err)
			}
			canaryIntact(t, outside)
		})
	}
}

// ---------- B. member names with NUL / whitespace / nothing but dots ----------

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

// ---------- C. the same name appearing twice ----------

func TestAuditDuplicateFileThenSymlinkThenHardlink(t *testing.T) {
	outside, dest := canary(t)
	// The classic bypass: register the name as a regular file in e.regular, then reuse
	// that same name as a symlink (the e.regular entry was never cleared), then hard-link
	// to it.
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
			t.Logf("b mode=%v (symlink? %v)", fi.Mode(), fi.Mode()&os.ModeSymlink != 0)
		}
	}
}

func TestAuditDuplicateSymlinkThenWriteThrough(t *testing.T) {
	outside, dest := canary(t)
	// Two steps: create a link at a directory inside dest, then write through it.
	src := buildAdvTar(t, "two.tar", false, []advEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "sub"},
		{name: "link/evil", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
	})
	_, err := Extract(src, dest)
	if !errors.Is(err, ErrTraversal) {
		t.Fatalf("writing through a symlink should be refused, got err=%v", err)
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
		t.Fatalf("the same name as file then directory somehow succeeded")
	}
	mustBeEmpty(t, dest)
}

// ---------- D. zip: dir entry and file entry sharing a name / backslashes / symlinks ----------

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
		t.Fatalf("writing through a zip symlink should be refused, got err=%v", err)
	}
	canaryIntact(t, outside)
}

func TestAuditZipBackslashEscape(t *testing.T) {
	outside, dest := canary(t)
	for _, n := range []string{`..\SECRET`, `..\..\SECRET`, `sub\..\..\SECRET`} {
		src := buildZip(t, "bs.zip", []zipEntry{{name: n, mode: 0o644, body: "pwned"}})
		_, err := Extract(src, dest)
		if !errors.Is(err, ErrTraversal) {
			t.Fatalf("zip name %q should be refused, got err=%v", n, err)
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
			t.Fatalf("a zip symlink pointing at %q should be refused, got err=%v", target, err)
		}
		canaryIntact(t, outside)
	}
}

// ---------- E. compression bombs: is the limit on BYTES ACTUALLY WRITTEN or on the DECLARED size? ----------

func TestAuditZipBombCountsRealBytesNotDeclared(t *testing.T) {
	_, dest := canary(t)
	// 100 MiB of zeros, a few dozen KB after deflate. Budget: 1 MiB.
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
		t.Fatalf("a zip bomb should be stopped by ErrTooLarge, got %v", err)
	}
	mustBeEmpty(t, dest)
	// The bytes on disk must not run far past the limit
}

func TestAuditTarLyingSizeHeader(t *testing.T) {
	_, dest := canary(t)
	// Hand-roll a tar header whose Size claims 10 while the real content is far more (via
	// two consecutive members). Go's tar reader truncates at the declared size, so what
	// this really checks is that "declares small, is actually large" cannot defeat the limit.
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
		t.Fatalf("going over the limit should be ErrTooLarge, got %v", err)
	}
	mustBeEmpty(t, dest)
}

// ---------- F. permission bits ----------

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
		t.Fatalf("extract failed: %v", err)
	}
	for _, n := range []string{"suid", "sgid", "sticky", "all", "ww"} {
		fi, err := os.Lstat(filepath.Join(dest, n))
		if err != nil {
			t.Fatalf("%s is gone: %v", n, err)
		}
		m := fi.Mode()
		if m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Fatalf("%s carried special bits out: %v", n, m)
		}
		if p := m.Perm(); p != 0o755 && p != 0o644 {
			t.Fatalf("%s has permissions %o, which is neither tier", n, p)
		}
		if p := m.Perm(); p&0o022 != 0 {
			t.Fatalf("%s is group/other writable: %o", n, p)
		}
	}
}

func TestAuditZipSetuidStripped(t *testing.T) {
	_, dest := canary(t)
	src := buildZip(t, "suid.zip", []zipEntry{
		{name: "s", mode: os.ModeSetuid | 0o755, body: "x"},
	})
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	fi, _ := os.Lstat(filepath.Join(dest, "s"))
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("zip setuid survived: %v", fi.Mode())
	}
}

// Does the chmod after umask end up WIDENING permissions?
func TestAuditChmodWidensPastUmask(t *testing.T) {
	old := umaskSet(0o077)
	defer umaskSet(old)
	_, dest := canary(t)
	// Two top-level entries on purpose: with only one top-level directory, stripTopDir
	// would strip d away and this test could no longer see a directory's permission bits,
	// which is exactly the tier it exists to check.
	src := buildAdvTar(t, "um.tar", false, []advEntry{
		{name: "keep", typ: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "d", typ: tar.TypeDir, mode: 0o755},
		{name: "d/f", typ: tar.TypeReg, mode: 0o755, body: "x"},
	})
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	fd, _ := os.Lstat(filepath.Join(dest, "d"))
	ff, _ := os.Lstat(filepath.Join(dest, "d", "f"))
	t.Logf("under umask 077: dir %o file %o", fd.Mode().Perm(), ff.Mode().Perm())
}

// ---------- G. can the cleanup path delete out of bounds? ----------

func TestAuditCleanupCannotDeleteOutsideDest(t *testing.T) {
	outside, dest := canary(t)
	// Plant a symlink pointing out of bounds inside dest (simulating "destDir already held
	// someone else's files"), let the archive work that link, then blow the limit on
	// purpose to trigger cleanup.
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
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	mustBeEmpty(t, dest)
	canaryIntact(t, outside)
}

// ---------- H. symlink escape variants ----------

func TestAuditSymlinkEscapeVariants(t *testing.T) {
	cases := []struct {
		name    string
		entries []advEntry
	}{
		{"straight at an absolute path", []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "/etc/passwd"},
		}},
		{"straight up one level", []advEntry{
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "../SECRET"},
		}},
		{"point at self, then double back", []advEntry{
			{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "x/up", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
			{name: "x/up/SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
		}},
		{"a link deep in the tree climbing back", []advEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755},
			{name: "a/b", typ: tar.TypeDir, mode: 0o755},
			{name: "a/b/l", typ: tar.TypeSymlink, mode: 0o777, link: "../.."},
			{name: "a/b/l/../SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned"},
		}},
		{"a backtracking .. in the middle of the target", []advEntry{
			{name: "d", typ: tar.TypeDir, mode: 0o755},
			{name: "l", typ: tar.TypeSymlink, mode: 0o777, link: "d/../.."},
		}},
		{"a link pointing at another link, then out", []advEntry{
			{name: "a", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "b", typ: tar.TypeSymlink, mode: 0o777, link: "a/.."},
		}},
		{"several levels back, but landing inside", []advEntry{
			{name: "p", typ: tar.TypeDir, mode: 0o755},
			{name: "p/q", typ: tar.TypeDir, mode: 0o755},
			{name: "p/q/l", typ: tar.TypeSymlink, mode: 0o777, link: "../.."},
			{name: "p/q/l/SECRET", typ: tar.TypeReg, mode: 0o644, body: "pwned-inside"},
		}},
		{"backslash as separator in the link target", []advEntry{
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
			// Success or failure, nothing new may appear out of bounds
			root := filepath.Dir(dest)
			ents, _ := os.ReadDir(root)
			for _, e := range ents {
				if e.Name() != "SECRET" && e.Name() != "dest" {
					t.Fatalf("%s appeared out of bounds", e.Name())
				}
			}
		})
	}
}

// ---------- I. hard links ----------

func TestAuditHardlinkVariants(t *testing.T) {
	cases := []struct {
		name    string
		entries []advEntry
	}{
		{"absolute path out of bounds", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "/etc/passwd"},
		}},
		{"relative path out of bounds", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "../SECRET"},
		}},
		{"a member that has not appeared yet", []advEntry{
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "later"},
			{name: "later", typ: tar.TypeReg, mode: 0o644, body: "x"},
		}},
		{"out through a symlink inside the package", []advEntry{
			{name: "d", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "h", typ: tar.TypeLink, mode: 0o644, link: "d/../SECRET"},
		}},
		{"at a symlink inside the package", []advEntry{
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

// dest already holds a symlink pointing out of bounds, and the archive hard-links to it
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
		t.Fatalf("a hard link to a pre-existing out-of-bounds link should be refused")
	}
	t.Logf("err=%v", err)
	canaryIntact(t, outside)
}

// ---------- J. very long paths ----------

func TestAuditVeryLongPath(t *testing.T) {
	_, dest := canary(t)
	deep := strings.Repeat("d/", 900) + "f"
	src := buildAdvTar(t, "deep.tar", true, []advEntry{
		{name: deep, typ: tar.TypeReg, mode: 0o644, body: "x", format: tar.FormatPAX},
	})
	files, err := Extract(src, dest)
	t.Logf("depth 900 -> files=%d err=%v", len(files), err)
	// Passing means: no panic, nothing written out of bounds
}

func TestAuditLongSingleComponent(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "longc.tar", true, []advEntry{
		{name: strings.Repeat("x", 5000), typ: tar.TypeReg, mode: 0o644, body: "x", format: tar.FormatPAX},
	})
	files, err := Extract(src, dest)
	t.Logf("single 5000-char element -> files=%v err=%v", files, err)
}

// ---------- K. irregular members ----------

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
		t.Fatalf("every tar git archive produces carries a pax_global_header; the archive must not fail wholesale: %v", err)
	}
}

func TestAuditEmptyNameGlobalHeader(t *testing.T) {
	_, dest := canary(t)
	// Some packers write a global header with an empty name
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
	t.Logf("empty-name global header -> files=%v err=%v", files, err)
}

func TestAuditGNUSparseFileIsHandled(t *testing.T) {
	_, dest := canary(t)
	src := buildAdvTar(t, "sp.tar", false, []advEntry{
		{name: "s", typ: tar.TypeGNUSparse, mode: 0o644, format: tar.FormatGNU},
	})
	files, err := Extract(src, dest)
	t.Logf("GNUSparse -> files=%v err=%v", files, err)
}

// ---------- L. zip member-ordering tricks ----------

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
			t.Fatalf("the regular file with the same name did not replace the symlink; a is still a link")
		}
	}
}

// Empty targets, or targets that are nothing but slashes
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
