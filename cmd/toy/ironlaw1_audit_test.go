package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Cirraya-LLC/toy/internal/host"
	"strings"
	"testing"
)

// The attack surface of iron law 1: walk a real install end to end and see
// whether a package can get hold of execution. Uses the bare-URL path over
// file://, so it needs neither the network nor git.

func i1Plat() string { return runtime.GOOS + "/" + runtime.GOARCH }

type i1Pkg struct{ dir, home string }

func i1New(t *testing.T, toyTOML string) *i1Pkg {
	t.Helper()
	p := &i1Pkg{dir: t.TempDir(), home: t.TempDir()}
	if err := os.WriteFile(filepath.Join(p.dir, "toy.toml"), []byte(toyTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOY_HOME", p.home)
	return p
}

func (p *i1Pkg) file(t *testing.T, name, body string, perm os.FileMode) {
	t.Helper()
	full := filepath.Join(p.dir, name)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
}

func (p *i1Pkg) targz(t *testing.T, name string, build func(*tar.Writer)) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (p *i1Pkg) install(t *testing.T, extra ...string) error {
	t.Helper()
	return runInstall(context.Background(), append([]string{"file://" + filepath.Join(p.dir, "toy.toml")}, extra...))
}

func i1Reg(tw *tar.Writer, name string, mode int64, body string) {
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(body))})
	tw.Write([]byte(body))
}

// L1: after installing, whatever the symlink in ~/.toy/bin finally resolves to
// must still live inside ~/.toy. Resolving to /bin/sh or to a file in the user's
// home means typing that command runs something from outside the package.
func TestIronLaw1LinkResolvesInsideToyHome(t *testing.T) {
	p := i1New(t, "name = \"i1tool\"\n[bin]\ni1tool = \"inner/bin\"\n[artifact]\n\""+i1Plat()+"\" = \"a.tar.gz\"\n")
	p.targz(t, "a.tar.gz", func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "inner", Linkname: "..", Mode: 0o777})
		i1Reg(tw, "bin", 0o755, "#!/bin/sh\n")
	})
	err := p.install(t)
	link := filepath.Join(p.home, "bin", "i1tool")
	if err != nil {
		if _, lerr := os.Lstat(link); lerr == nil {
			t.Fatalf("the install failed but left the symlink %s behind", link)
		}
		return
	}
	real, rerr := filepath.EvalSymlinks(link)
	if rerr != nil {
		t.Fatalf("the symlink does not resolve: %v", rerr)
	}
	root, _ := filepath.EvalSymlinks(p.home)
	if !strings.HasPrefix(real+string(os.PathSeparator), root+string(os.PathSeparator)) {
		t.Fatalf("i1tool on PATH ends up at %s, which is outside %s", real, root)
	}
}

// L2: the [script] form has to land inside the package directory, and without setuid.
func TestIronLaw1ScriptStaysInsidePkgDir(t *testing.T) {
	p := i1New(t, "name = \"i1script\"\n[script]\ni1script = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\n", 0o644)
	if err := p.install(t); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(p.home, "bin", "i1script"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(p.home)
	if !strings.HasPrefix(real, filepath.Join(root, "pkg")+string(os.PathSeparator)) {
		t.Fatalf("the script landed at %s, not under %s/pkg", real, root)
	}
	fi, _ := os.Stat(real)
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Fatalf("it carries setuid/setgid: %v", fi.Mode())
	}
}

// L3: iron law 5 — a package must not shadow a command already on PATH.
func TestIronLaw1CannotShadowExistingCommand(t *testing.T) {
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "i1shadow"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := i1New(t, "name = \"i1sh\"\n[script]\ni1shadow = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\n", 0o644)
	if err := p.install(t); err == nil {
		t.Fatal("installed a symlink that shadows i1shadow on PATH")
	}
}

// L4: the "git" toy itself executes — with no git on PATH, can a package claim
// that name? Once it has, the very next `toy install github:…` / `toy update` /
// `toy doctor` executes that downloaded file (host.Tags, autGitRemote and
// qryCheckGit all resolve it through PATH).
func TestIronLaw1PackageCanClaimTheGitToyItselfRuns(t *testing.T) {
	empty := t.TempDir() // a PATH with no git: a fresh machine that has not installed it yet
	t.Setenv("PATH", empty)
	p := i1New(t, "name = \"i1git\"\n[script]\ngit = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\necho PWNED\n", 0o644)
	if err := p.install(t); err != nil {
		t.Logf("refused (good): %v", err)
		return
	}
	link := filepath.Join(p.home, "bin", "git")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("not created? %v", err)
	}
	t.Fatalf("the package claimed %s. host.Tags / autGitRemote / qryCheckGit execute exactly "+
		"the \"git\" they find on PATH — once the user follows the docs and puts ~/.toy/bin on PATH, "+
		"the next toy command runs this downloaded file. Not one warning at install time", link)
}

// L5: finish L4 — after installing, actually let toy go execute it and see
// whether that file runs.
func TestIronLaw1ClaimedGitActuallyGetsExecuted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	bin := filepath.Join(home, "bin")
	// The user put ~/.toy/bin on PATH, as the README and toy doctor tell them to.
	// This machine has no git yet (qryCheckGit suggests installing it, but that
	// does not block install).
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	os.WriteFile(filepath.Join(dir, "toy.toml"),
		[]byte("name = \"i1git2\"\n[script]\ngit = \"s.sh\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "s.sh"),
		[]byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o644)

	if err := runInstall(context.Background(), []string{"file://" + filepath.Join(dir, "toy.toml")}); err != nil {
		t.Skipf("nothing to test if it will not install (%v)", err)
	}
	// The user then does the most ordinary thing there is: installs another
	// package. Version discovery goes through host.Tags, which execs the "git"
	// on PATH.
	_, _ = host.Tags(context.Background(), "https://github.com/golang/go")
	link := filepath.Join(bin, "git")
	fi, lerr := os.Stat(link)
	t.Logf("link=%s stat=%v mode=%v", link, lerr, fi)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("toy executed the downloaded script itself (%s appeared) — iron law 1 is broken: "+
			"nothing ran at install time, but the next toy command ran it", marker)
	}
	t.Log("not executed (this machine's doctor may not reach the exec-git step)")
}
