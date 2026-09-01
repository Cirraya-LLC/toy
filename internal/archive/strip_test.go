package archive

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cargo-dist 的形状：整棵树套在一层「版本号 + target triple」的目录里。
// 那个目录名是 [bin] 表达不了的（单个字符串、不按平台变、不展开 {version}），
// 所以现代 Rust CLI 那一整块全靠这一层剥掉才收得了。
func TestStripsTheSingleTopLevelDir(t *testing.T) {
	src := buildTar(t, "ripgrep-15.2.0-x86_64-unknown-linux-musl.tar.gz", true, []tarEntry{
		{name: "ripgrep-15.2.0-x86_64-unknown-linux-musl", typ: tar.TypeDir, mode: 0o755},
		{name: "ripgrep-15.2.0-x86_64-unknown-linux-musl/rg", typ: tar.TypeReg, mode: 0o755, body: "ELF"},
		reg("ripgrep-15.2.0-x86_64-unknown-linux-musl/README.md", "hi"),
		{name: "ripgrep-15.2.0-x86_64-unknown-linux-musl/complete", typ: tar.TypeDir, mode: 0o755},
		reg("ripgrep-15.2.0-x86_64-unknown-linux-musl/complete/rg.bash", "compdef"),
	})
	dest := t.TempDir()
	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	slices.Sort(files)
	want := []string{"README.md", "complete/rg.bash", "rg"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("返回的路径 = %v，想要 %v（剥完的，不带那层目录）", files, want)
	}
	// [bin] rg = "rg" 要真的指到一个可执行文件上 —— 这才是剥这一层的全部目的。
	fi, err := os.Stat(filepath.Join(dest, "rg"))
	if err != nil {
		t.Fatalf("剥完之后 dest/rg 不在：%v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("dest/rg 没有执行位：%v", fi.Mode())
	}
	// 那层目录必须真的没了，不能既留着又剥。
	if _, err := os.Lstat(filepath.Join(dest, "ripgrep-15.2.0-x86_64-unknown-linux-musl")); !os.IsNotExist(err) {
		t.Errorf("顶层目录还在：%v", err)
	}
}

// 判据是「恰好一个顶层条目而且它是目录」。别的形状一律不动 ——
// 剥错一层会让 [bin] 指到一个不存在的路径上，而那是装不上的。
func TestStripOnlyWhenExactlyOneTopLevelDir(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		want    []string
	}{{
		name: "两个顶层目录：剥哪个都得丢掉另一个",
		entries: []tarEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755}, reg("a/x", "1"),
			{name: "b", typ: tar.TypeDir, mode: 0o755}, reg("b/y", "2"),
		},
		want: []string{"a/x", "b/y"},
	}, {
		name: "一个目录加一个顶层文件",
		entries: []tarEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755}, reg("a/x", "1"),
			reg("LICENSE", "MIT"),
		},
		want: []string{"LICENSE", "a/x"},
	}, {
		name:    "唯一那个是文件（裸二进制那种包）",
		entries: []tarEntry{{name: "tool", typ: tar.TypeReg, mode: 0o755, body: "ELF"}},
		want:    []string{"tool"},
	}, {
		name: "唯一那个是符号链接：DirEntry.IsDir() 是 lstat 语义，不许跟着它走",
		entries: []tarEntry{
			{name: "here", typ: tar.TypeSymlink, mode: 0o777, link: "."},
		},
		want: []string{"here"},
	}, {
		name:    "唯一那个是空目录：剥完 dest 就空了，那不是少一层",
		entries: []tarEntry{{name: "empty", typ: tar.TypeDir, mode: 0o755}},
		want:    nil,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := t.TempDir()
			files, err := Extract(buildTar(t, "x.tar.gz", true, c.entries), dest)
			if err != nil {
				t.Fatalf("解压失败：%v", err)
			}
			slices.Sort(files)
			if strings.Join(files, ",") != strings.Join(c.want, ",") {
				t.Fatalf("返回的路径 = %v，想要 %v", files, c.want)
			}
		})
	}
	// 空目录那条还要确认目录本身留着 —— 不能"没剥"却把它删了。
	dest := t.TempDir()
	if _, err := Extract(buildTar(t, "e.tar.gz", true,
		[]tarEntry{{name: "empty", typ: tar.TypeDir, mode: 0o755}}), dest); err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("空目录不该被剥掉：%v %v", fi, err)
	}
}

// 顶层目录叫 `..`（或者任何往上爬的名字）根本走不到剥这一步：resolve() 在写盘
// 之前就拒了整个包（铁律 7）。剥目录不是第二个需要清洗名字的地方 ——
// 它拿到的名字只来自 os.ReadDir，那永远是单个路径段。
func TestStripNeverSeesATraversingTopDir(t *testing.T) {
	for _, name := range []string{"..", "../", "../evil", "/abs", `..\evil`, "a/../.."} {
		dest := t.TempDir()
		_, err := Extract(buildTar(t, "bad.tar.gz", true, []tarEntry{
			{name: name, typ: tar.TypeDir, mode: 0o755},
			reg(strings.TrimSuffix(name, "/")+"/payload", "pwned"),
		}), dest)
		if !errors.Is(err, ErrTraversal) {
			t.Errorf("顶层目录 %q：想要 ErrTraversal，实际 %v", name, err)
		}
		mustBeEmpty(t, dest)
	}
}

// 剥掉一层会把每条【相对】符号链接的解析起点上移一层 —— 这是剥目录唯一自己
// 引入的逃逸面。先爬到包根、再顺着顶层目录名走回来的链接，剥完就指到 destDir
// 外面去了。这种包宁可不剥（[bin] 会响亮地失败），也绝不留一条指到界外的链接。
func TestStripRefusedWhenItWouldPointALinkOutside(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 剥之前：dest/top/deep/back → dest/top（包内，合法）。
	// 剥之后：dest/deep/back → root/top（界外）—— 所以不许剥。
	src := buildTar(t, "sneaky.tar.gz", true, []tarEntry{
		{name: "top", typ: tar.TypeDir, mode: 0o755},
		reg("top/real.txt", "hello"),
		{name: "top/deep", typ: tar.TypeDir, mode: 0o755},
		{name: "top/deep/back", typ: tar.TypeSymlink, mode: 0o777, link: "../../top/real.txt"},
	})
	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	if !slices.Contains(files, filepath.Join("top", "deep", "back")) {
		t.Fatalf("这个包不该被剥：%v", files)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "top", "deep", "back")); err != nil || string(b) != "hello" {
		t.Errorf("没剥的包里那条链接该照旧指着包内的文件：%q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "deep")); !os.IsNotExist(err) {
		t.Errorf("剥了一半：%v", err)
	}
}

// 反过来：包内自己指自己的相对链接（macOS 的 Versions/Current -> A 那种）
// 剥完仍然落在界内，不许被上面那条误伤。
func TestStripKeepsSelfContainedLinks(t *testing.T) {
	dest := t.TempDir()
	files, err := Extract(buildTar(t, "fw.tar.gz", true, []tarEntry{
		{name: "pkg-1.2.3", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg-1.2.3/Versions", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg-1.2.3/Versions/A", typ: tar.TypeDir, mode: 0o755},
		reg("pkg-1.2.3/Versions/A/lib.so", "ELF"),
		{name: "pkg-1.2.3/Versions/Current", typ: tar.TypeSymlink, mode: 0o777, link: "A"},
		{name: "pkg-1.2.3/libfoo.so", typ: tar.TypeSymlink, mode: 0o777, link: "Versions/Current/lib.so"},
	}), dest)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	if slices.Contains(files, filepath.Join("pkg-1.2.3", "libfoo.so")) {
		t.Fatalf("这个包该被剥掉一层：%v", files)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "libfoo.so")); err != nil || string(b) != "ELF" {
		t.Errorf("剥完顺着链接读 = %q, %v", b, err)
	}
}

// zip 走的是另一条解压路径，剥这一层是在两条路径汇合之后做的 —— 一个机制，
// 不是两个（zip 的包同样套一层目录）。
func TestStripWorksForZipToo(t *testing.T) {
	src := buildZip(t, "tool-1.2.3.zip", []zipEntry{
		{name: "tool-1.2.3/", mode: os.ModeDir | 0o755},
		{name: "tool-1.2.3/tool", mode: 0o755, body: "ELF"},
	})
	dest := t.TempDir()
	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	if strings.Join(files, ",") != "tool" {
		t.Fatalf("返回的路径 = %v，想要 [tool]", files)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Errorf("dest/tool 不在：%v", err)
	}
}

// 顶层目录里有个东西和它自己同名（`pkg/pkg`，包名和目录名一样时真的会这样）。
// 直接往上搬会撞在一起（rename 到非空目录 ENOTEMPTY、到空目录 EISDIR），
// 那会让一个本来正常的包整个装不上 —— 所以先把那层挪开再搬。
func TestStripHandlesAnInnerEntryNamedLikeTheTopDir(t *testing.T) {
	dest := t.TempDir()
	files, err := Extract(buildTar(t, "same.tar.gz", true, []tarEntry{
		{name: "pkg", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg/pkg", typ: tar.TypeReg, mode: 0o755, body: "ELF"},
		reg("pkg/README.md", "hi"),
	}), dest)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	slices.Sort(files)
	if strings.Join(files, ",") != "README.md,pkg" {
		t.Fatalf("返回的路径 = %v，想要 [README.md pkg]", files)
	}
	b, err := os.ReadFile(filepath.Join(dest, "pkg"))
	if err != nil || string(b) != "ELF" {
		t.Errorf("dest/pkg 该是那个可执行文件：%q %v", b, err)
	}
	// 挪开用的那个临时名字不许留在包里。
	if _, err := os.Lstat(filepath.Join(dest, "pkg.toy-strip")); !os.IsNotExist(err) {
		t.Errorf("临时目录没清掉：%v", err)
	}
}
