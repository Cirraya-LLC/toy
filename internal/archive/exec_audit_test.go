package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Iron rule 1/7 attack surface: can what we unpack point outside the package, and can it carry setuid out?

func audWriteTar(t *testing.T, dir string, build func(*tar.Writer)) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "pkg.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func audReg(t *testing.T, tw *tar.Writer, name string, mode int64, body string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

// A1: do setuid / setgid bits survive? If they do, an installer can create setuid files.
func TestAuditSetuidIsStripped(t *testing.T) {
	tmp := t.TempDir()
	src := audWriteTar(t, tmp, func(tw *tar.Writer) {
		// 04755: setuid + rwxr-xr-x
		audReg(t, tw, "boom", 04755, "#!/bin/sh\n")
		audReg(t, tw, "sgid", 02755, "#!/bin/sh\n")
		audReg(t, tw, "sticky", 01755, "#!/bin/sh\n")
	})
	dest := filepath.Join(tmp, "out")
	os.MkdirAll(dest, 0o755)
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	for _, n := range []string{"boom", "sgid", "sticky"} {
		fi, err := os.Lstat(filepath.Join(dest, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s came out carrying special bits: %v", n, fi.Mode())
		}
	}
}

// A2: symlinks pointing outside the package (absolute path, or all the way up with ..).
func TestAuditSymlinkCannotEscape(t *testing.T) {
	for _, target := range []string{
		"/bin/sh",
		"/etc/passwd",
		"../../../../../../../../etc/passwd",
		"..",
	} {
		tmp := t.TempDir()
		src := audWriteTar(t, tmp, func(tw *tar.Writer) {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink, Name: "tool", Linkname: target, Mode: 0o777,
			}); err != nil {
				t.Fatal(err)
			}
		})
		dest := filepath.Join(tmp, "out")
		os.MkdirAll(dest, 0o755)
		if _, err := Extract(src, dest); err == nil {
			got, _ := os.Readlink(filepath.Join(dest, "tool"))
			t.Errorf("link target %q was let through; the package now holds tool -> %q", target, got)
		}
	}
}

// A3: the three-step bypass from the comments (x -> ".", x/up -> "..", x/up/pwned).
func TestAuditSymlinkLadderCannotEscape(t *testing.T) {
	tmp := t.TempDir()
	src := audWriteTar(t, tmp, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "x", Linkname: ".", Mode: 0o777})
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "x/up", Linkname: "..", Mode: 0o777})
		audReg(t, tw, "x/up/pwned", 0o755, "owned\n")
	})
	root := filepath.Join(tmp, "root")
	dest := filepath.Join(root, "dest")
	os.MkdirAll(dest, 0o755)
	if _, err := Extract(src, dest); err == nil {
		t.Error("the three-step symlink ladder was let through")
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); err == nil {
		t.Fatal("wrote outside destDir")
	}
}

// A4: a hard link pointing at a system file.
func TestAuditHardlinkCannotGrabSystemFiles(t *testing.T) {
	tmp := t.TempDir()
	src := audWriteTar(t, tmp, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "tool", Linkname: "/etc/passwd", Mode: 0o755})
	})
	dest := filepath.Join(tmp, "out")
	os.MkdirAll(dest, 0o755)
	if _, err := Extract(src, dest); err == nil {
		t.Error("a hard link to /etc/passwd was let through")
	}
}

// A5: path traversal.
func TestAuditTraversalRejected(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/tmp/evil", `..\evil`} {
		tmp := t.TempDir()
		src := audWriteTar(t, tmp, func(tw *tar.Writer) { audReg(t, tw, name, 0o755, "x") })
		dest := filepath.Join(tmp, "out")
		os.MkdirAll(dest, 0o755)
		if _, err := Extract(src, dest); err == nil {
			t.Errorf("member name %q was let through", name)
		}
	}
}
