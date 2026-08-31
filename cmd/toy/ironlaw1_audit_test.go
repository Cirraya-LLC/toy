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

// 铁律 1 的攻击面：从头到尾走一次真实安装，看包能不能把执行权拿到手。
// 用 file:// 那条裸地址路，不需要网络也不需要 git。

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

// L1：装完之后 ~/.toy/bin 里那条软链最终解析到的东西必须还在 ~/.toy 里。
// 解析到 /bin/sh 或用户主目录里的文件 = 用户敲这个命令跑的是包外的东西。
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
			t.Fatalf("安装失败却留下了软链 %s", link)
		}
		return
	}
	real, rerr := filepath.EvalSymlinks(link)
	if rerr != nil {
		t.Fatalf("软链解析不了：%v", rerr)
	}
	root, _ := filepath.EvalSymlinks(p.home)
	if !strings.HasPrefix(real+string(os.PathSeparator), root+string(os.PathSeparator)) {
		t.Fatalf("PATH 上的 i1tool 最终指向 %s，跑出了 %s", real, root)
	}
}

// L2：[script] 形态的落地路径必须在包目录里，且不带 setuid。
func TestIronLaw1ScriptStaysInsidePkgDir(t *testing.T) {
	p := i1New(t, "name = \"i1script\"\n[script]\ni1script = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\n", 0o644)
	if err := p.install(t); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(p.home, "bin", "i1script"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(p.home)
	if !strings.HasPrefix(real, filepath.Join(root, "pkg")+string(os.PathSeparator)) {
		t.Fatalf("脚本落在 %s，不在 %s/pkg 里", real, root)
	}
	fi, _ := os.Stat(real)
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Fatalf("带了 setuid/setgid：%v", fi.Mode())
	}
}

// L3：铁律 5 —— 包不能顶掉 PATH 上已有的命令。
func TestIronLaw1CannotShadowExistingCommand(t *testing.T) {
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, "i1shadow"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := i1New(t, "name = \"i1sh\"\n[script]\ni1shadow = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\n", 0o644)
	if err := p.install(t); err == nil {
		t.Fatal("装上了一条会顶掉 PATH 上 i1shadow 的软链")
	}
}

// L4：toy 自己要执行的 "git" —— PATH 上没有 git 时，一个包能不能把这个名字占下来？
// 占下来之后，下一条 `toy install github:…` / `toy update` / `toy doctor`
// 就会执行这个下载来的文件（host.Tags / autGitRemote / qryCheckGit 都走 PATH）。
func TestIronLaw1PackageCanClaimTheGitToyItselfRuns(t *testing.T) {
	empty := t.TempDir() // 一个没有 git 的 PATH：模拟「还没装 git」的新机器
	t.Setenv("PATH", empty)
	p := i1New(t, "name = \"i1git\"\n[script]\ngit = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\necho PWNED\n", 0o644)
	if err := p.install(t); err != nil {
		t.Logf("被拒（好事）：%v", err)
		return
	}
	link := filepath.Join(p.home, "bin", "git")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("没建出来？%v", err)
	}
	t.Fatalf("包把 %s 占下来了。而 host.Tags / autGitRemote / qryCheckGit 执行的就是 "+
		"PATH 上的 \"git\" —— 用户按文档把 ~/.toy/bin 加进 PATH 之后，"+
		"下一条 toy 命令就会跑这个下载来的文件。安装时一个警告都没有", link)
}

// L5：把 L4 走完 —— 装完之后真的让 toy 自己去执行它，看那个文件跑没跑。
func TestIronLaw1ClaimedGitActuallyGetsExecuted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	bin := filepath.Join(home, "bin")
	// 用户按 README / toy doctor 说的把 ~/.toy/bin 加进 PATH。
	// 这台机器上还没装 git（qryCheckGit 会提示去装，但那不拦着 install）。
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	os.WriteFile(filepath.Join(dir, "toy.toml"),
		[]byte("name = \"i1git2\"\n[script]\ngit = \"s.sh\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "s.sh"),
		[]byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o644)

	if err := runInstall(context.Background(), []string{"file://" + filepath.Join(dir, "toy.toml")}); err != nil {
		t.Skipf("装不上就谈不上后面（%v）", err)
	}
	// 用户接着做一件最普通的事：装另一个包。版本发现走 host.Tags，
	// 而它 exec 的就是 PATH 上的 "git"。
	_, _ = host.Tags(context.Background(), "https://github.com/golang/go")
	link := filepath.Join(bin, "git")
	fi, lerr := os.Stat(link)
	t.Logf("link=%s stat=%v mode=%v", link, lerr, fi)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("下载来的脚本被 toy 自己执行了（%s 出现了）—— 铁律 1 破了："+
			"安装那一刻没跑，但下一条 toy 命令跑了它", marker)
	}
	t.Log("没被执行（可能这台机器上 doctor 没走到 exec git 那一步）")
}
