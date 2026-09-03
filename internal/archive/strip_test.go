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

// The cargo-dist shape: the whole tree wrapped in one "version + target triple"
// directory. [bin] cannot express that name (single string, does not vary by platform,
// does not expand {version}), so the entire modern Rust CLI shelf only becomes
// packageable once that layer is stripped.
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
		t.Fatalf("extract failed: %v", err)
	}
	slices.Sort(files)
	want := []string{"README.md", "complete/rg.bash", "rg"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v (stripped, without that directory)", files, want)
	}
	// [bin] rg = "rg" has to land on a real executable - that is the entire point of the strip.
	fi, err := os.Stat(filepath.Join(dest, "rg"))
	if err != nil {
		t.Fatalf("dest/rg missing after the strip: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("dest/rg has no execute bit: %v", fi.Mode())
	}
	// That directory must really be gone; it cannot be both kept and stripped.
	if _, err := os.Lstat(filepath.Join(dest, "ripgrep-15.2.0-x86_64-unknown-linux-musl")); !os.IsNotExist(err) {
		t.Errorf("top-level directory still there: %v", err)
	}
}

// The test is "exactly one top-level entry AND it is a directory". Every other shape is
// left alone: stripping the wrong layer points [bin] at a path that does not exist, and
// that package cannot be installed at all.
func TestStripOnlyWhenExactlyOneTopLevelDir(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		want    []string
	}{{
		name: "two top-level dirs: stripping either one drops the other",
		entries: []tarEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755}, reg("a/x", "1"),
			{name: "b", typ: tar.TypeDir, mode: 0o755}, reg("b/y", "2"),
		},
		want: []string{"a/x", "b/y"},
	}, {
		name: "one dir plus a top-level file",
		entries: []tarEntry{
			{name: "a", typ: tar.TypeDir, mode: 0o755}, reg("a/x", "1"),
			reg("LICENSE", "MIT"),
		},
		want: []string{"LICENSE", "a/x"},
	}, {
		name:    "the single entry is a file (the bare-binary shape)",
		entries: []tarEntry{{name: "tool", typ: tar.TypeReg, mode: 0o755, body: "ELF"}},
		want:    []string{"tool"},
	}, {
		name: "the single entry is a symlink: DirEntry.IsDir() is lstat, never follow it",
		entries: []tarEntry{
			{name: "here", typ: tar.TypeSymlink, mode: 0o777, link: "."},
		},
		want: []string{"here"},
	}, {
		name:    "the single entry is an empty dir: stripping empties dest, that is not one layer less",
		entries: []tarEntry{{name: "empty", typ: tar.TypeDir, mode: 0o755}},
		want:    nil,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := t.TempDir()
			files, err := Extract(buildTar(t, "x.tar.gz", true, c.entries), dest)
			if err != nil {
				t.Fatalf("extract failed: %v", err)
			}
			slices.Sort(files)
			if strings.Join(files, ",") != strings.Join(c.want, ",") {
				t.Fatalf("paths = %v, want %v", files, c.want)
			}
		})
	}
	// The empty-dir case also has to keep the directory: "no strip" must not mean "deleted".
	dest := t.TempDir()
	if _, err := Extract(buildTar(t, "e.tar.gz", true,
		[]tarEntry{{name: "empty", typ: tar.TypeDir, mode: 0o755}}), dest); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("an empty directory must not be stripped: %v %v", fi, err)
	}
}

// A top-level directory named `..`, or any other climbing name, never reaches the strip
// at all: resolve() refuses the whole archive before anything hits disk (iron rule 7).
// Stripping is not a second place where names need sanitising - the only names it ever
// sees come from os.ReadDir, and those are always a single path element.
func TestStripNeverSeesATraversingTopDir(t *testing.T) {
	for _, name := range []string{"..", "../", "../evil", "/abs", `..\evil`, "a/../.."} {
		dest := t.TempDir()
		_, err := Extract(buildTar(t, "bad.tar.gz", true, []tarEntry{
			{name: name, typ: tar.TypeDir, mode: 0o755},
			reg(strings.TrimSuffix(name, "/")+"/payload", "pwned"),
		}), dest)
		if !errors.Is(err, ErrTraversal) {
			t.Errorf("top-level dir %q: want ErrTraversal, got %v", name, err)
		}
		mustBeEmpty(t, dest)
	}
}

// Stripping a layer moves every RELATIVE symlink's resolution origin up by one, and that
// is the one escape surface stripping introduces on its own. A link that climbs to the
// package root and walks back down through the top-level directory name ends up pointing
// outside destDir once stripped. Such a package is left unstripped ([bin] then fails
// loudly); a link that reads outside destDir is never acceptable.
func TestStripRefusedWhenItWouldPointALinkOutside(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Before the strip: dest/top/deep/back -> dest/top (inside the package, legal).
	// After the strip:  dest/deep/back     -> root/top (out of bounds) - so: no strip.
	src := buildTar(t, "sneaky.tar.gz", true, []tarEntry{
		{name: "top", typ: tar.TypeDir, mode: 0o755},
		reg("top/real.txt", "hello"),
		{name: "top/deep", typ: tar.TypeDir, mode: 0o755},
		{name: "top/deep/back", typ: tar.TypeSymlink, mode: 0o777, link: "../../top/real.txt"},
	})
	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if !slices.Contains(files, filepath.Join("top", "deep", "back")) {
		t.Fatalf("this package must not be stripped: %v", files)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "top", "deep", "back")); err != nil || string(b) != "hello" {
		t.Errorf("in an unstripped package that link must still point at the file inside it: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "deep")); !os.IsNotExist(err) {
		t.Errorf("stripped halfway: %v", err)
	}
}

// The other direction: a self-contained relative link (macOS's Versions/Current -> A
// kind) still resolves inside after the strip, and must not be caught by the rule above.
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
		t.Fatalf("extract failed: %v", err)
	}
	if slices.Contains(files, filepath.Join("pkg-1.2.3", "libfoo.so")) {
		t.Fatalf("this package should have had a layer stripped: %v", files)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "libfoo.so")); err != nil || string(b) != "ELF" {
		t.Errorf("reading through the link after the strip = %q, %v", b, err)
	}
}

// zip takes the other extraction path, and the strip happens after the two paths meet -
// one mechanism, not two (zip packages wrap a directory just the same).
func TestStripWorksForZipToo(t *testing.T) {
	src := buildZip(t, "tool-1.2.3.zip", []zipEntry{
		{name: "tool-1.2.3/", mode: os.ModeDir | 0o755},
		{name: "tool-1.2.3/tool", mode: 0o755, body: "ELF"},
	})
	dest := t.TempDir()
	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if strings.Join(files, ",") != "tool" {
		t.Fatalf("paths = %v, want [tool]", files)
	}
	if _, err := os.Stat(filepath.Join(dest, "tool")); err != nil {
		t.Errorf("dest/tool missing: %v", err)
	}
}

// An inner entry named like the top-level directory itself (`pkg/pkg`, which really
// happens when the package name matches the directory name). Hoisting straight up would
// collide (rename onto a non-empty dir is ENOTEMPTY, onto an empty one EISDIR), and that
// would make a perfectly good package uninstallable - so the layer is moved aside first.
func TestStripHandlesAnInnerEntryNamedLikeTheTopDir(t *testing.T) {
	dest := t.TempDir()
	files, err := Extract(buildTar(t, "same.tar.gz", true, []tarEntry{
		{name: "pkg", typ: tar.TypeDir, mode: 0o755},
		{name: "pkg/pkg", typ: tar.TypeReg, mode: 0o755, body: "ELF"},
		reg("pkg/README.md", "hi"),
	}), dest)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	slices.Sort(files)
	if strings.Join(files, ",") != "README.md,pkg" {
		t.Fatalf("paths = %v, want [README.md pkg]", files)
	}
	b, err := os.ReadFile(filepath.Join(dest, "pkg"))
	if err != nil || string(b) != "ELF" {
		t.Errorf("dest/pkg should be the executable: %q %v", b, err)
	}
	// The staging name must not be left behind in the package.
	if _, err := os.Lstat(filepath.Join(dest, "pkg.toy-strip")); !os.IsNotExist(err) {
		t.Errorf("staging directory not cleaned up: %v", err)
	}
}
