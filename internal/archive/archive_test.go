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
	"testing"
)

// ---------- archive-building helpers (the malicious archives are hand-rolled here too) ----------

type tarEntry struct {
	name string
	typ  byte
	mode int64
	link string
	body string
}

func reg(name, body string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeReg, mode: 0o644, body: body}
}

// buildTar writes entries as a tar (wrapped in gzip when gz=true) and returns the path.
func buildTar(t *testing.T, name string, gz bool, entries []tarEntry) string {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     e.mode,
			Linkname: e.link,
			Size:     int64(len(e.body)),
		}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header failed: %v", err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing tar content failed: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar failed: %v", err)
	}

	out := raw.Bytes()
	if gz {
		var zbuf bytes.Buffer
		zw := gzip.NewWriter(&zbuf)
		if _, err := zw.Write(out); err != nil {
			t.Fatalf("gzip write failed: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close failed: %v", err)
		}
		out = zbuf.Bytes()
	}

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatalf("writing the file failed: %v", err)
	}
	return p
}

type zipEntry struct {
	name string
	mode os.FileMode
	body string // for a symlink this is the target
}

func buildZip(t *testing.T, name string, entries []zipEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.SetMode(e.mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("writing zip header failed: %v", err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing zip content failed: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip failed: %v", err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing the file failed: %v", err)
	}
	return p
}

func mustBeEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s failed: %v", dir, err)
	}
	if len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("%s should be empty after a refusal, still holds: %v", dir, names)
	}
}

// ---------- IsArchive ----------

func TestIsArchive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"tinyserve_1.2.0_linux_amd64.tar.gz", true},
		{"x.TGZ", true},
		{"x.tar", true},
		{"x.zip", true},
		{"x.tar.xz", true}, // recognised, but Extract reports ErrUnsupported explicitly
		{"/a/b/c.tar.gz", true},
		{"tinyserve", false},
		{"tool.exe", false},
		{"x.gz", false},     // bare gzip is not an archive; install it as a single file
		{"x.tar.bz2", true}, // bzip2 decompression is in the standard library, so this one we handle
		{"", false},
	}
	for _, c := range cases {
		if got := IsArchive(c.name); got != c.want {
			t.Errorf("IsArchive(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------- happy path ----------

func TestExtractTarGzHappyPath(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755}, // the usual first entry from GNU tar
		{name: "bin", typ: tar.TypeDir, mode: 0o755},
		{name: "bin/tinyserve", typ: tar.TypeReg, mode: 0o4755, body: "ELF..."}, // carries setuid
		reg("README.md", "hi"),
		reg("share/doc/a/b.txt", "deep"), // no explicit entries for the intermediate dirs
	})
	dest := t.TempDir()

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	want := []string{
		filepath.Join("bin", "tinyserve"),
		"README.md",
		filepath.Join("share", "doc", "a", "b.txt"),
	}
	if strings.Join(files, "|") != strings.Join(want, "|") {
		t.Errorf("file list = %v, want %v", files, want)
	}

	if b, err := os.ReadFile(filepath.Join(dest, "share/doc/a/b.txt")); err != nil || string(b) != "deep" {
		t.Errorf("wrong content in the deep file: %q, %v", b, err)
	}

	// Two permission tiers only, and setuid must never survive
	fi, err := os.Stat(filepath.Join(dest, "bin/tinyserve"))
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("executable permissions = %o, want 755", fi.Mode().Perm())
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("setuid/setgid/sticky survived: %v", fi.Mode())
	}
	fi, err = os.Stat(filepath.Join(dest, "README.md"))
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("regular file permissions = %o, want 644", fi.Mode().Perm())
	}
}

func TestExtractPlainTarAndZip(t *testing.T) {
	tarSrc := buildTar(t, "ok.tar", false, []tarEntry{reg("a.txt", "tar")})
	dest := t.TempDir()
	if _, err := Extract(tarSrc, dest); err != nil {
		t.Fatalf("extracting .tar failed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(b) != "tar" {
		t.Errorf("wrong .tar content: %q", b)
	}

	zipSrc := buildZip(t, "ok.zip", []zipEntry{
		{name: "bin/", mode: os.ModeDir | 0o755},
		{name: "bin/tool", mode: 0o755, body: "bin"},
		{name: "notes.txt", mode: 0o600, body: "zip"},
	})
	dest = t.TempDir()
	files, err := Extract(zipSrc, dest)
	if err != nil {
		t.Fatalf("extracting zip failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("zip returned %v, want two files", files)
	}
	fi, err := os.Stat(filepath.Join(dest, "bin/tool"))
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("executable permissions from zip = %o, want 755", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dest, "notes.txt"))
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("0600 should land in the 644 tier, got %o", fi.Mode().Perm())
	}
}

// ---------- attack cases ----------

func TestExtractRejectsEscapes(t *testing.T) {
	cases := []struct {
		name string
		mk   func(t *testing.T) string
	}{
		{"tar relative traversal", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("../../etc/passwd", "pwned")})
		}},
		{"tar absolute path", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("/etc/passwd", "pwned")})
		}},
		{"tar traversal in the middle", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("a/b/../../../out.txt", "pwned")})
		}},
		{"tar directory entry traversal", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{{name: "../evil", typ: tar.TypeDir, mode: 0o755}})
		}},
		{"symlink pointing up a level", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "../../etc"},
			})
		}},
		{"symlink pointing at an absolute path", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "/etc/passwd"},
			})
		}},
		{"deep symlink climbing back up", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "a/b", typ: tar.TypeDir, mode: 0o755},
				{name: "a/b/link", typ: tar.TypeSymlink, mode: 0o777, link: "../../../outside"},
			})
		}},
		{"hard link pointing outside the package", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "../../etc/passwd"},
			})
		}},
		{"hard link pointing at an absolute path", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "/etc/passwd"},
			})
		}},
		{"zip backslash", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: `..\evil`, mode: 0o644, body: "pwned"}})
		}},
		{"zip backslash, several levels", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: `..\..\etc\passwd`, mode: 0o644, body: "pwned"}})
		}},
		{"zip relative traversal", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: "../evil.txt", mode: 0o644, body: "pwned"}})
		}},
		{"zip symlink out of bounds", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{
				{name: "link", mode: os.ModeSymlink | 0o777, body: "../../etc"},
			})
		}},
		{"empty member name", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: "  ", mode: 0o644, body: "x"}})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.mk(t)
			dest := t.TempDir()
			files, err := Extract(src, dest)
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("want ErrTraversal, got err=%v files=%v", err, files)
			}
			if files != nil {
				t.Errorf("a refused archive must not return a file list, got %v", files)
			}
			mustBeEmpty(t, dest) // whole archive refused = not one byte left behind
		})
	}
}

// TestPrefixConfusion pins the separator-aware prefix comparison: with a sibling
// directory next to dest whose name starts with it, a plain prefix check (no separator)
// treats /.../destevil as something inside /.../dest.
func TestPrefixConfusion(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	sibling := filepath.Join(root, "destevil")
	for _, d := range []string{dest, sibling} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("creating the directory failed: %v", err)
		}
	}
	src := buildTar(t, "e.tar.gz", true, []tarEntry{reg("../destevil/pwned", "x")})

	if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
		t.Fatalf("want ErrTraversal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("the sibling directory was written into: %v", err)
	}
	mustBeEmpty(t, dest)
}

// ---------- links inside the package are allowed ----------

func TestSymlinkInsideDestIsAllowed(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		reg("sub/real.txt", "hello"),
		{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "sub/real.txt"},
		{name: "sub/back", typ: tar.TypeSymlink, mode: 0o777, link: "../sub/real.txt"}, // takes a detour, stays inside
	})
	dest := t.TempDir()

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("symlinks inside the package should be allowed, got %v", err)
	}
	if len(files) != 3 {
		t.Errorf("file list = %v, want 3 entries (the file plus two links)", files)
	}
	for _, name := range []string{"link", filepath.Join("sub", "back")} {
		p := filepath.Join(dest, name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink", name)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "hello" {
			t.Errorf("reading through %s gave %q, %v", name, b, err)
		}
	}
}

func TestHardlinkInsideDestIsAllowed(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		reg("real.txt", "hello"),
		{name: "hl.txt", typ: tar.TypeLink, mode: 0o644, link: "real.txt"},
	})
	dest := t.TempDir()

	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("hard links inside the package should be allowed, got %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "hl.txt")); err != nil || string(b) != "hello" {
		t.Errorf("hard link content = %q, %v", b, err)
	}
}

// ---------- irregular members: skipped, not an error ----------

func TestIrregularMembersAreSkipped(t *testing.T) {
	src := buildTar(t, "odd.tar.gz", true, []tarEntry{
		{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
		{name: "dev/loop0", typ: tar.TypeBlock, mode: 0o660},
		{name: "run/pipe", typ: tar.TypeFifo, mode: 0o600},
		reg("real.txt", "ok"),
	})
	dest := t.TempDir()

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("irregular members should be skipped, not error out, got %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("file list = %v, want real.txt only", files)
	}
	if _, err := os.Lstat(filepath.Join(dest, "run/pipe")); !os.IsNotExist(err) {
		t.Errorf("the FIFO must not be created: %v", err)
	}

	zsrc := buildZip(t, "odd.zip", []zipEntry{
		{name: "sock", mode: os.ModeSocket | 0o600},
		{name: "fifo", mode: os.ModeNamedPipe | 0o600},
		{name: "dev", mode: os.ModeDevice | 0o600},
		{name: "real.txt", mode: 0o644, body: "ok"},
	})
	dest = t.TempDir()
	files, err = Extract(zsrc, dest)
	if err != nil {
		t.Fatalf("zip irregular members should be skipped, got %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("zip file list = %v, want real.txt only", files)
	}
}

// ---------- size limit ----------

func TestTooLargeAbortsAndCleansUp(t *testing.T) {
	big := strings.Repeat("A", 300)
	for _, tc := range []struct {
		name string
		src  func(t *testing.T) string
	}{
		{"tar", func(t *testing.T) string {
			return buildTar(t, "bomb.tar.gz", true, []tarEntry{
				{name: "d", typ: tar.TypeDir, mode: 0o755},
				reg("d/small.txt", "small"),
				reg("d/big.txt", big),
			})
		}},
		{"zip", func(t *testing.T) string {
			return buildZip(t, "bomb.zip", []zipEntry{
				{name: "d/small.txt", mode: 0o644, body: "small"},
				{name: "d/big.txt", mode: 0o644, body: big},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			files, err := extractTo(tc.src(t), dest, 100)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("want ErrTooLarge, got err=%v files=%v", err, files)
			}
			mustBeEmpty(t, dest) // everything already written, dirs created along the way included, must be cleaned
		})
	}
}

func TestUnderLimitStillWorks(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		reg("a.txt", strings.Repeat("x", 40)),
		reg("b.txt", strings.Repeat("y", 40)),
	})
	dest := t.TempDir()
	if _, err := extractTo(src, dest, 100); err != nil {
		t.Fatalf("staying within the limit must not error: %v", err)
	}
}

// ---------- formats and arguments ----------

func TestUnsupportedFormats(t *testing.T) {
	dest := t.TempDir()
	// .tar.bz2 came off this list: compress/bzip2 is in the standard library.
	// .tar.xz stays -- xz is not, and a library for it would break §10.
	for _, name := range []string{"pkg.tar.xz", "tinyserve"} {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("whatever"), 0o644); err != nil {
			t.Fatalf("writing the file failed: %v", err)
		}
		if _, err := Extract(p, dest); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: want ErrUnsupported, got %v", name, err)
		}
	}
}

func TestDestDirMustExist(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{reg("a.txt", "x")})
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Extract(src, missing); err == nil {
		t.Fatal("a missing destDir should be an error")
	}

	notDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatalf("writing the file failed: %v", err)
	}
	if _, err := Extract(src, notDir); err == nil {
		t.Fatal("a destDir that is a file should be an error")
	}
}

func TestBrokenGzip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(p, []byte("not gzip at all"), 0o644); err != nil {
		t.Fatalf("writing the file failed: %v", err)
	}
	if _, err := Extract(p, t.TempDir()); err == nil {
		t.Fatal("broken gzip should be an error")
	}
}

// TestWithin pins the separator-aware prefix comparison on its own.
// Extract cannot exercise it: filepath.Join Cleans first, so the ".." check masks it,
// which leaves this as the only place that layer can be verified.
func TestWithin(t *testing.T) {
	const dest = "/tmp/x/dest"
	cases := []struct {
		p    string
		want bool
	}{
		{"/tmp/x/dest", true},
		{"/tmp/x/dest/bin/tool", true},
		{"/tmp/x/destevil", false}, // a plain prefix check would call this one of our own
		{"/tmp/x/destevil/pwned", false},
		{"/tmp/x/des", false},
		{"/tmp/x", false},
		{"/etc/passwd", false},
		{"/tmp/x/destdest", false},
	}
	for _, c := range cases {
		if got := within(dest, c.p); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", dest, c.p, got, c.want)
		}
	}
}

func TestSafeMode(t *testing.T) {
	cases := []struct {
		in   os.FileMode
		want os.FileMode
	}{
		{0o777, 0o755},
		{0o755, 0o755},
		{0o700, 0o755},
		{0o644, 0o644},
		{0o600, 0o644},
		{0o000, 0o644},
		{os.ModeSetuid | 0o4755, 0o755},
		{os.ModeSetgid | 0o2644, 0o644},
	}
	for _, c := range cases {
		if got := safeMode(c.in); got != c.want {
			t.Errorf("safeMode(%o) = %o, want %o", c.in, got, c.want)
		}
	}
}

// ---------- symlinks as path components (the class the lexical check cannot catch) ----------

// TestSymlinkDirComponentEscape is the most important regression in this package.
// Each of the three members passes the lexical check on its own, yet together they write
// a file outside destDir:
//
//	x        -> "."   lexically dest/x, inside
//	x/up     -> ".."  lexically Join(dest/x, "..") = dest, inside; really dest/up -> destDir's parent
//	x/up/pwned        lexically dest/x/up/pwned, inside; really written outside destDir
//
// The root cause: the lexical check resolves a link target against the LEXICAL parent
// while the kernel uses the REAL parent. One link pointing shallower than itself
// (x -> ".") is enough to skew the two depths by a level, and the next ".." climbs out.
// So "links we let through point inside, therefore following them cannot escape" does
// not hold.
func TestSymlinkDirComponentEscape(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(t *testing.T) string
	}{
		{"tar", func(t *testing.T) string {
			return buildTar(t, "evil.tar.gz", true, []tarEntry{
				{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
				{name: "x/up", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
				reg("x/up/pwned", "PWNED"),
			})
		}},
		{"zip", func(t *testing.T) string {
			return buildZip(t, "evil.zip", []zipEntry{
				{name: "x", mode: os.ModeSymlink | 0o777, body: "."},
				{name: "x/up", mode: os.ModeSymlink | 0o777, body: ".."},
				{name: "x/up/pwned", mode: 0o644, body: "PWNED"},
			})
		}},
		{"multiple hops", func(t *testing.T) string { // if one link is not enough, take a few more
			return buildTar(t, "evil.tar.gz", true, []tarEntry{
				{name: "a", typ: tar.TypeDir, mode: 0o755},
				{name: "a/b", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
				{name: "a/b/c", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
				reg("a/b/c/pwned", "PWNED"),
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "dest")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatalf("creating the directory failed: %v", err)
			}
			files, err := Extract(tc.mk(t), dest)
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("want ErrTraversal, got err=%v files=%v", err, files)
			}
			if _, err := os.Stat(filepath.Join(root, "pwned")); !os.IsNotExist(err) {
				t.Fatalf("something was written outside destDir: %v", err)
			}
			mustBeEmpty(t, dest)
		})
	}
}

// TestPreexistingSymlinkDirIsRefused: when destDir already holds a symlinked directory,
// a package must not use it to write outside. A previous install, another tool, or the
// user himself can all leave such a link behind.
func TestPreexistingSymlinkDirIsRefused(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("creating the directory failed: %v", err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "pre")); err != nil {
		t.Fatalf("creating the link failed: %v", err)
	}
	// One level deeper has to be caught too: Lstat on a deep path silently resolves the links in the middle.
	if err := os.Mkdir(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatalf("creating the directory failed: %v", err)
	}
	for _, member := range []string{"pre/pwned", "pre/deep/pwned"} {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{reg(member, "PWNED")})
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("member %s: want ErrTraversal, got %v", member, err)
		}
	}
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("reading %s failed: %v", outside, err)
	}
	if len(ents) != 1 || ents[0].Name() != "deep" {
		t.Fatalf("something was written outside destDir: %v", ents)
	}
}

// TestHardlinkMustTargetAPackageFile: os.Link follows symlinks in the middle of the
// target path, so a hard link target must be a regular file this extraction wrote itself.
func TestHardlinkMustTargetAPackageFile(t *testing.T) {
	t.Run("dragging an outside file in through a symlink", func(t *testing.T) {
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatalf("creating the directory failed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOP SECRET"), 0o600); err != nil {
			t.Fatalf("writing the file failed: %v", err)
		}
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "x/up", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
			{name: "stolen", typ: tar.TypeLink, mode: 0o644, link: "x/up/secret.txt"},
		})
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("want ErrTraversal, got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dest, "stolen")); !os.IsNotExist(err) {
			t.Fatal("a file outside the package was hard-linked in")
		}
		mustBeEmpty(t, dest)
	})

	t.Run("pointing at a symlink inside the package is not allowed either", func(t *testing.T) {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			reg("real.txt", "hi"),
			{name: "sym", typ: tar.TypeSymlink, mode: 0o777, link: "real.txt"},
			{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "sym"},
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("want ErrTraversal, got %v", err)
		}
		mustBeEmpty(t, dest)
	})

	t.Run("referencing a target that has not appeared yet must fail loudly", func(t *testing.T) {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "later.txt"},
			reg("later.txt", "hi"),
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("want ErrTraversal, got %v", err)
		}
		mustBeEmpty(t, dest) // no half a package left behind
	})

	t.Run("chaining another hard link onto one is allowed", func(t *testing.T) {
		src := buildTar(t, "ok.tar.gz", true, []tarEntry{
			reg("real.txt", "hello"),
			{name: "a", typ: tar.TypeLink, mode: 0o644, link: "real.txt"},
			{name: "b", typ: tar.TypeLink, mode: 0o644, link: "a"},
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); err != nil {
			t.Fatalf("a hard link to an earlier hard link should be allowed, got %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(dest, "b")); err != nil || string(b) != "hello" {
			t.Errorf("b = %q, %v", b, err)
		}
	})
}

// ---------- member count limit (a byte limit does nothing about inodes) ----------

func TestTooManyEntriesIsRefused(t *testing.T) {
	// All entries share one directory name: the count still climbs, only one directory hits disk, and the test stays fast.
	tarEntries := make([]tarEntry, maxEntries+1)
	zipEntries := make([]zipEntry, maxEntries+1)
	for i := range tarEntries {
		tarEntries[i] = tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755}
		zipEntries[i] = zipEntry{name: "d/", mode: os.ModeDir | 0o755}
	}
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"tar", buildTar(t, "many.tar", false, tarEntries)},
		{"zip", buildZip(t, "many.zip", zipEntries)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			if _, err := Extract(tc.src, dest); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("want ErrTooLarge, got %v", err)
			}
			mustBeEmpty(t, dest)
		})
	}
}

// TestSymlinkTargetCannotDoubleBack: the skew can appear on the link TARGET side too.
// After x -> ".", "x/../outside" is lexically inside destDir while the kernel walks into
// destDir's parent. Nothing is written outside, but the package is left holding a link
// that reads outside it - and store may symlink that onto PATH.
func TestSymlinkTargetCannotDoubleBack(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("creating the directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatalf("writing the file failed: %v", err)
	}
	src := buildTar(t, "e.tar.gz", true, []tarEntry{
		{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
		{name: "esc", typ: tar.TypeSymlink, mode: 0o777, link: "x/../outside.txt"},
	})
	if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
		t.Fatalf("want ErrTraversal, got %v", err)
	}
	mustBeEmpty(t, dest)

	// Leading ".."s climb from the link's own parent, and that parent chain is guaranteed to be real directories, so no collateral.
	ok := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		reg("sub/real.txt", "hello"),
		{name: "sub/deep", typ: tar.TypeDir, mode: 0o755},
		{name: "sub/deep/back", typ: tar.TypeSymlink, mode: 0o777, link: "../../sub/real.txt"},
	})
	dest2 := t.TempDir()
	if _, err := Extract(ok, dest2); err != nil {
		t.Fatalf("leading .. must not be caught: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest2, "sub", "deep", "back")); err != nil || string(b) != "hello" {
		t.Errorf("reading through the link = %q, %v", b, err)
	}
}

// TestLegitSymlinkedDirLayoutStillWorks: the common framework/versioned-directory layout must not be caught.
func TestLegitSymlinkedDirLayoutStillWorks(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "Versions/A", typ: tar.TypeDir, mode: 0o755},
		reg("Versions/A/lib.so", "ELF"),
		{name: "Versions/Current", typ: tar.TypeSymlink, mode: 0o777, link: "A"},
		{name: "libfoo.so", typ: tar.TypeSymlink, mode: 0o777, link: "Versions/Current/lib.so"},
	})
	dest := t.TempDir()
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("a legitimate versioned directory layout was caught: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "libfoo.so")); err != nil || string(b) != "ELF" {
		t.Errorf("reading through the link = %q, %v", b, err)
	}
}
