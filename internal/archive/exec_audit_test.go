package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 铁律 1/7 攻击面：解压出来的东西能不能指到包外面、能不能带出 setuid。

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

// A1：setuid / setgid 位有没有被带出来。带出来 = 一个装包器能造 setuid 文件。
func TestAuditSetuidIsStripped(t *testing.T) {
	tmp := t.TempDir()
	src := audWriteTar(t, tmp, func(tw *tar.Writer) {
		// 04755：setuid + rwxr-xr-x
		audReg(t, tw, "boom", 04755, "#!/bin/sh\n")
		audReg(t, tw, "sgid", 02755, "#!/bin/sh\n")
		audReg(t, tw, "sticky", 01755, "#!/bin/sh\n")
	})
	dest := filepath.Join(tmp, "out")
	os.MkdirAll(dest, 0o755)
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	for _, n := range []string{"boom", "sgid", "sticky"} {
		fi, err := os.Lstat(filepath.Join(dest, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s 解出来带了特殊位：%v", n, fi.Mode())
		}
	}
}

// A2：符号链接指到包外面（绝对路径 / 一路 ..）。
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
			t.Errorf("链接目标 %q 被放行了，包里现在有一条 tool -> %q", target, got)
		}
	}
}

// A3：注释里那个三段式绕过（x -> "."，x/up -> ".."，x/up/pwned）。
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
		t.Error("三段式符号链接梯子被放行了")
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); err == nil {
		t.Fatal("写到 destDir 外面去了")
	}
}

// A4：硬链接指向系统文件。
func TestAuditHardlinkCannotGrabSystemFiles(t *testing.T) {
	tmp := t.TempDir()
	src := audWriteTar(t, tmp, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "tool", Linkname: "/etc/passwd", Mode: 0o755})
	})
	dest := filepath.Join(tmp, "out")
	os.MkdirAll(dest, 0o755)
	if _, err := Extract(src, dest); err == nil {
		t.Error("硬链接到 /etc/passwd 被放行了")
	}
}

// A5：路径穿越。
func TestAuditTraversalRejected(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/tmp/evil", `..\evil`} {
		tmp := t.TempDir()
		src := audWriteTar(t, tmp, func(tw *tar.Writer) { audReg(t, tw, name, 0o755, "x") })
		dest := filepath.Join(tmp, "out")
		os.MkdirAll(dest, 0o755)
		if _, err := Extract(src, dest); err == nil {
			t.Errorf("成员名 %q 被放行了", name)
		}
	}
}
