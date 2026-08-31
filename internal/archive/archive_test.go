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

// ---------- 造包的小工具（恶意包也是在这里手搓出来的） ----------

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

// buildTar 把 entries 写成一个 tar（gz=true 时再套一层 gzip），返回文件路径。
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
			t.Fatalf("写 tar 头失败：%v", err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("写 tar 内容失败：%v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar 失败：%v", err)
	}

	out := raw.Bytes()
	if gz {
		var zbuf bytes.Buffer
		zw := gzip.NewWriter(&zbuf)
		if _, err := zw.Write(out); err != nil {
			t.Fatalf("gzip 写失败：%v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip 关失败：%v", err)
		}
		out = zbuf.Bytes()
	}

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	return p
}

type zipEntry struct {
	name string
	mode os.FileMode
	body string // 符号链接时这里就是目标
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
			t.Fatalf("写 zip 头失败：%v", err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("写 zip 内容失败：%v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关 zip 失败：%v", err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	return p
}

func mustBeEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", dir, err)
	}
	if len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("拒绝之后 %s 应该是空的，实际还剩：%v", dir, names)
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
		{"x.tar.xz", true}, // 认得出来，但 Extract 会明确报 ErrUnsupported
		{"/a/b/c.tar.gz", true},
		{"tinyserve", false},
		{"tool.exe", false},
		{"x.gz", false}, // 裸 gzip 不是包，按单文件处理
		{"x.tar.bz2", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsArchive(c.name); got != c.want {
			t.Errorf("IsArchive(%q) = %v，想要 %v", c.name, got, c.want)
		}
	}
}

// ---------- 正常路径 ----------

func TestExtractTarGzHappyPath(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755}, // GNU tar 常见的第一条
		{name: "bin", typ: tar.TypeDir, mode: 0o755},
		{name: "bin/tinyserve", typ: tar.TypeReg, mode: 0o4755, body: "ELF..."}, // 带 setuid
		reg("README.md", "hi"),
		reg("share/doc/a/b.txt", "deep"), // 中间目录没有显式条目
	})
	dest := t.TempDir()

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("Extract 失败：%v", err)
	}

	want := []string{
		filepath.Join("bin", "tinyserve"),
		"README.md",
		filepath.Join("share", "doc", "a", "b.txt"),
	}
	if strings.Join(files, "|") != strings.Join(want, "|") {
		t.Errorf("返回的文件列表 = %v，想要 %v", files, want)
	}

	if b, err := os.ReadFile(filepath.Join(dest, "share/doc/a/b.txt")); err != nil || string(b) != "deep" {
		t.Errorf("深层文件内容不对：%q, %v", b, err)
	}

	// 权限只有两档，setuid 绝不能带出来
	fi, err := os.Stat(filepath.Join(dest, "bin/tinyserve"))
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("可执行文件权限 = %o，想要 755", fi.Mode().Perm())
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("setuid/setgid/sticky 被带出来了：%v", fi.Mode())
	}
	fi, err = os.Stat(filepath.Join(dest, "README.md"))
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("普通文件权限 = %o，想要 644", fi.Mode().Perm())
	}
}

func TestExtractPlainTarAndZip(t *testing.T) {
	tarSrc := buildTar(t, "ok.tar", false, []tarEntry{reg("a.txt", "tar")})
	dest := t.TempDir()
	if _, err := Extract(tarSrc, dest); err != nil {
		t.Fatalf("解 .tar 失败：%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(b) != "tar" {
		t.Errorf(".tar 内容不对：%q", b)
	}

	zipSrc := buildZip(t, "ok.zip", []zipEntry{
		{name: "bin/", mode: os.ModeDir | 0o755},
		{name: "bin/tool", mode: 0o755, body: "bin"},
		{name: "notes.txt", mode: 0o600, body: "zip"},
	})
	dest = t.TempDir()
	files, err := Extract(zipSrc, dest)
	if err != nil {
		t.Fatalf("解 zip 失败：%v", err)
	}
	if len(files) != 2 {
		t.Fatalf("zip 返回 %v，想要两个文件", files)
	}
	fi, err := os.Stat(filepath.Join(dest, "bin/tool"))
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("zip 里的可执行文件权限 = %o，想要 755", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dest, "notes.txt"))
	if err != nil {
		t.Fatalf("stat 失败：%v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("0600 应该归到 644 那一档，实际 %o", fi.Mode().Perm())
	}
}

// ---------- 攻击性用例 ----------

func TestExtractRejectsEscapes(t *testing.T) {
	cases := []struct {
		name string
		mk   func(t *testing.T) string
	}{
		{"tar 相对穿越", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("../../etc/passwd", "pwned")})
		}},
		{"tar 绝对路径", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("/etc/passwd", "pwned")})
		}},
		{"tar 中段穿越", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{reg("a/b/../../../out.txt", "pwned")})
		}},
		{"tar 目录条目穿越", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{{name: "../evil", typ: tar.TypeDir, mode: 0o755}})
		}},
		{"符号链接指向上层", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "../../etc"},
			})
		}},
		{"符号链接指向绝对路径", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "/etc/passwd"},
			})
		}},
		{"深层符号链接爬回上层", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "a/b", typ: tar.TypeDir, mode: 0o755},
				{name: "a/b/link", typ: tar.TypeSymlink, mode: 0o777, link: "../../../outside"},
			})
		}},
		{"硬链接指向包外", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "../../etc/passwd"},
			})
		}},
		{"硬链接指向绝对路径", func(t *testing.T) string {
			return buildTar(t, "e.tar.gz", true, []tarEntry{
				{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "/etc/passwd"},
			})
		}},
		{"zip 反斜杠", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: `..\evil`, mode: 0o644, body: "pwned"}})
		}},
		{"zip 反斜杠多层", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: `..\..\etc\passwd`, mode: 0o644, body: "pwned"}})
		}},
		{"zip 相对穿越", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: "../evil.txt", mode: 0o644, body: "pwned"}})
		}},
		{"zip 符号链接出界", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{
				{name: "link", mode: os.ModeSymlink | 0o777, body: "../../etc"},
			})
		}},
		{"空成员名", func(t *testing.T) string {
			return buildZip(t, "e.zip", []zipEntry{{name: "  ", mode: 0o644, body: "x"}})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.mk(t)
			dest := t.TempDir()
			files, err := Extract(src, dest)
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("想要 ErrTraversal，实际 err=%v files=%v", err, files)
			}
			if files != nil {
				t.Errorf("被拒的包不该返回文件列表，实际 %v", files)
			}
			mustBeEmpty(t, dest) // 整个包被拒 = 一个字节都不该留下
		})
	}
}

// TestPrefixConfusion 钉住"带分隔符的前缀比较"：dest 旁边有个名字以它开头的兄弟目录，
// 光比前缀（不带分隔符）会把 /…/destevil 当成 /…/dest 里面的东西。
func TestPrefixConfusion(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	sibling := filepath.Join(root, "destevil")
	for _, d := range []string{dest, sibling} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
	}
	src := buildTar(t, "e.tar.gz", true, []tarEntry{reg("../destevil/pwned", "x")})

	if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
		t.Fatalf("想要 ErrTraversal，实际 %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("兄弟目录被写进去了：%v", err)
	}
	mustBeEmpty(t, dest)
}

// ---------- 包内链接是允许的 ----------

func TestSymlinkInsideDestIsAllowed(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		reg("sub/real.txt", "hello"),
		{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "sub/real.txt"},
		{name: "sub/back", typ: tar.TypeSymlink, mode: 0o777, link: "../sub/real.txt"}, // 绕一圈但没出界
	})
	dest := t.TempDir()

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("包内符号链接应该放行，实际 %v", err)
	}
	if len(files) != 3 {
		t.Errorf("文件列表 = %v，想要 3 条（文件 + 两个链接）", files)
	}
	for _, name := range []string{"link", filepath.Join("sub", "back")} {
		p := filepath.Join(dest, name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s 没建出来：%v", name, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s 不是符号链接", name)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "hello" {
			t.Errorf("顺着 %s 读到 %q, %v", name, b, err)
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
		t.Fatalf("包内硬链接应该放行，实际 %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "hl.txt")); err != nil || string(b) != "hello" {
		t.Errorf("硬链接内容 = %q, %v", b, err)
	}
}

// ---------- 非常规成员：跳过，不报错 ----------

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
		t.Fatalf("非常规成员应该被跳过而不是报错，实际 %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("文件列表 = %v，想要只有 real.txt", files)
	}
	if _, err := os.Lstat(filepath.Join(dest, "run/pipe")); !os.IsNotExist(err) {
		t.Errorf("FIFO 不该被建出来：%v", err)
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
		t.Fatalf("zip 非常规成员应该被跳过，实际 %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("zip 文件列表 = %v，想要只有 real.txt", files)
	}
}

// ---------- 体积上限 ----------

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
				t.Fatalf("想要 ErrTooLarge，实际 err=%v files=%v", err, files)
			}
			mustBeEmpty(t, dest) // 已经写出去的（含中途建的目录）必须清干净
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
		t.Fatalf("正好在上限内不该报错：%v", err)
	}
}

// ---------- 格式与参数 ----------

func TestUnsupportedFormats(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{"pkg.tar.xz", "tinyserve", "pkg.tar.bz2"} {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("whatever"), 0o644); err != nil {
			t.Fatalf("落盘失败：%v", err)
		}
		if _, err := Extract(p, dest); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s：想要 ErrUnsupported，实际 %v", name, err)
		}
	}
}

func TestDestDirMustExist(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{reg("a.txt", "x")})
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := Extract(src, missing); err == nil {
		t.Fatal("destDir 不存在时应该报错")
	}

	notDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	if _, err := Extract(src, notDir); err == nil {
		t.Fatal("destDir 是文件时应该报错")
	}
}

func TestBrokenGzip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := os.WriteFile(p, []byte("not gzip at all"), 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	if _, err := Extract(p, t.TempDir()); err == nil {
		t.Fatal("坏 gzip 应该报错")
	}
}

// TestWithin 单独钉住"前缀比较带分隔符"这一条。
// 走 Extract 测不出来：filepath.Join 会先 Clean，".." 那道判定把它遮住了，
// 所以这层防御只能在这里验。
func TestWithin(t *testing.T) {
	const dest = "/tmp/x/dest"
	cases := []struct {
		p    string
		want bool
	}{
		{"/tmp/x/dest", true},
		{"/tmp/x/dest/bin/tool", true},
		{"/tmp/x/destevil", false}, // 只比前缀就会误判成自己人
		{"/tmp/x/destevil/pwned", false},
		{"/tmp/x/des", false},
		{"/tmp/x", false},
		{"/etc/passwd", false},
		{"/tmp/x/destdest", false},
	}
	for _, c := range cases {
		if got := within(dest, c.p); got != c.want {
			t.Errorf("within(%q, %q) = %v，想要 %v", dest, c.p, got, c.want)
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
			t.Errorf("safeMode(%o) = %o，想要 %o", c.in, got, c.want)
		}
	}
}

// ---------- 符号链接当路径中间段（词法校验拦不住的那一类） ----------

// TestSymlinkDirComponentEscape 是这个包最要紧的一条回归。
// 三条成员单独看每一条都通过了词法校验，合起来却把文件写到了 destDir 外面：
//
//	x        -> "."   词法落点 dest/x，在界内
//	x/up     -> ".."  词法 Join(dest/x, "..") = dest，在界内；真实落点是 dest/up -> dest 的父目录
//	x/up/pwned        词法 dest/x/up/pwned，在界内；真实写到 destDir 外面
//
// 根因是词法校验按【词法】父目录解析链接目标，内核按【真实】父目录解析。
// 只要有一个链接指得比自己浅（x -> "."），两边深度就错开一层，后面一个 ".." 就爬出去了。
// 所以"放行的链接必定指向内部、跟随它们也逃不出去"这条归纳不成立。
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
		{"多跳", func(t *testing.T) string { // 一条链接不够就多绕几跳
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
				t.Fatalf("建目录失败：%v", err)
			}
			files, err := Extract(tc.mk(t), dest)
			if !errors.Is(err, ErrTraversal) {
				t.Fatalf("想要 ErrTraversal，实际 err=%v files=%v", err, files)
			}
			if _, err := os.Stat(filepath.Join(root, "pwned")); !os.IsNotExist(err) {
				t.Fatalf("destDir 外面被写了东西：%v", err)
			}
			mustBeEmpty(t, dest)
		})
	}
}

// TestPreexistingSymlinkDirIsRefused：destDir 里本来就躺着一个符号链接目录时，
// 包不能借它把东西写到外面去。上一次安装、别的工具、用户自己都可能留下这种链接。
func TestPreexistingSymlinkDirIsRefused(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "pre")); err != nil {
		t.Fatalf("建链接失败：%v", err)
	}
	// 深一层也要拦住：Lstat 一个深层路径会把中间的链接悄悄解析掉。
	if err := os.Mkdir(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	for _, member := range []string{"pre/pwned", "pre/deep/pwned"} {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{reg(member, "PWNED")})
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("成员 %s：想要 ErrTraversal，实际 %v", member, err)
		}
	}
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", outside, err)
	}
	if len(ents) != 1 || ents[0].Name() != "deep" {
		t.Fatalf("destDir 外面被写了东西：%v", ents)
	}
}

// TestHardlinkMustTargetAPackageFile：os.Link 会跟随目标路径中间的符号链接，
// 所以硬链接目标只认"本次解压自己写出来的普通文件"。
func TestHardlinkMustTargetAPackageFile(t *testing.T) {
	t.Run("穿过符号链接把包外文件拽进来", func(t *testing.T) {
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOP SECRET"), 0o600); err != nil {
			t.Fatalf("落盘失败：%v", err)
		}
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
			{name: "x/up", typ: tar.TypeSymlink, mode: 0o777, link: ".."},
			{name: "stolen", typ: tar.TypeLink, mode: 0o644, link: "x/up/secret.txt"},
		})
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("想要 ErrTraversal，实际 %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dest, "stolen")); !os.IsNotExist(err) {
			t.Fatal("包外的文件被硬链进来了")
		}
		mustBeEmpty(t, dest)
	})

	t.Run("指向包里的符号链接也不行", func(t *testing.T) {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			reg("real.txt", "hi"),
			{name: "sym", typ: tar.TypeSymlink, mode: 0o777, link: "real.txt"},
			{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "sym"},
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("想要 ErrTraversal，实际 %v", err)
		}
		mustBeEmpty(t, dest)
	})

	t.Run("目标还没出现就引用它要响亮地失败", func(t *testing.T) {
		src := buildTar(t, "e.tar.gz", true, []tarEntry{
			{name: "hl", typ: tar.TypeLink, mode: 0o644, link: "later.txt"},
			reg("later.txt", "hi"),
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
			t.Fatalf("想要 ErrTraversal，实际 %v", err)
		}
		mustBeEmpty(t, dest) // 不能留下半个包
	})

	t.Run("链上再挂一条硬链接是允许的", func(t *testing.T) {
		src := buildTar(t, "ok.tar.gz", true, []tarEntry{
			reg("real.txt", "hello"),
			{name: "a", typ: tar.TypeLink, mode: 0o644, link: "real.txt"},
			{name: "b", typ: tar.TypeLink, mode: 0o644, link: "a"},
		})
		dest := t.TempDir()
		if _, err := Extract(src, dest); err != nil {
			t.Fatalf("硬链接指向前面的硬链接应该放行，实际 %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(dest, "b")); err != nil || string(b) != "hello" {
			t.Errorf("b = %q, %v", b, err)
		}
	})
}

// ---------- 成员数上限（字节上限管不住 inode） ----------

func TestTooManyEntriesIsRefused(t *testing.T) {
	// 全用同名目录条目：计数照涨，盘上只建一个目录，测试才跑得快。
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
				t.Fatalf("想要 ErrTooLarge，实际 %v", err)
			}
			mustBeEmpty(t, dest)
		})
	}
}

// TestSymlinkTargetCannotDoubleBack：错位也能出现在链接【目标】那一侧。
// x -> "." 之后 "x/../outside" 词法算出来是 destDir 里，内核走的却是 destDir 的父目录 ——
// 不写出去，但包里会留下一条能读到包外的链接，而它可能被 store 软链上 PATH。
func TestSymlinkTargetCannotDoubleBack(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	src := buildTar(t, "e.tar.gz", true, []tarEntry{
		{name: "x", typ: tar.TypeSymlink, mode: 0o777, link: "."},
		{name: "esc", typ: tar.TypeSymlink, mode: 0o777, link: "x/../outside.txt"},
	})
	if _, err := Extract(src, dest); !errors.Is(err, ErrTraversal) {
		t.Fatalf("想要 ErrTraversal，实际 %v", err)
	}
	mustBeEmpty(t, dest)

	// 开头连着的 .. 是从链接自己的父目录往上爬，那条父链保证全是真目录，不该误伤。
	ok := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		reg("sub/real.txt", "hello"),
		{name: "sub/deep", typ: tar.TypeDir, mode: 0o755},
		{name: "sub/deep/back", typ: tar.TypeSymlink, mode: 0o777, link: "../../sub/real.txt"},
	})
	dest2 := t.TempDir()
	if _, err := Extract(ok, dest2); err != nil {
		t.Fatalf("开头的 .. 不该被误伤：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest2, "sub", "deep", "back")); err != nil || string(b) != "hello" {
		t.Errorf("顺着链接读 = %q, %v", b, err)
	}
}

// TestLegitSymlinkedDirLayoutStillWorks：常见的 framework/版本目录布局不能被误伤。
func TestLegitSymlinkedDirLayoutStillWorks(t *testing.T) {
	src := buildTar(t, "ok.tar.gz", true, []tarEntry{
		{name: "Versions/A", typ: tar.TypeDir, mode: 0o755},
		reg("Versions/A/lib.so", "ELF"),
		{name: "Versions/Current", typ: tar.TypeSymlink, mode: 0o777, link: "A"},
		{name: "libfoo.so", typ: tar.TypeSymlink, mode: 0o777, link: "Versions/Current/lib.so"},
	})
	dest := t.TempDir()
	if _, err := Extract(src, dest); err != nil {
		t.Fatalf("正经的版本目录布局被误伤了：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "libfoo.so")); err != nil || string(b) != "ELF" {
		t.Errorf("顺着链接读 = %q, %v", b, err)
	}
}
