package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 原本的指控（现已修复，这里翻成回归测试）：
// host.Tags 用 exec.CommandContext(ctx,"git",…) 按**名字**执行 git，名字是从调用
// 进程的 PATH 上解析的。而 ~/.toy/bin 既是包能往里写软链的目录，又是 doctor 教
// 用户放在 PATH 最前面的那个目录 —— 一个 toy.toml 写 [bin] git = "payload" 就能
// 占下这个名字，装的那一刻一行没跑（铁律 1 的字面意思成立），下一条 toy 命令却把
// 它当 git 执行了。
//
// 修法有两半：internal/store 那一半拒绝把包链成 `git`（连 --force 都不放行）；
// 这一半是 lookupGit —— 自己走一遍 PATH、解析出绝对路径、**跳过 ~/.toy/bin**。
// 两半都要有：store 那道只挡新装的包，这道连一个已经躺在 bin/ 里的 git 也不跑。
//
// 下面守的是这一半。注意「PATH 上随便哪个目录里的假 git 会被执行」不在射程内 ——
// 那只是 PATH 本来的语义，用户自己的 PATH 是用户自己的事；危险的是**包能写进去**
// 的那个目录。
func TestTagsRefusesGitFromToyBinDir(t *testing.T) {
	// bin/ 是 PATH 上唯一的 git，就像 doctor 教用户排的那样在最前面。
	t.Run("只有 bin/ 里那个", func(t *testing.T) {
		binDir, marker := toyBinWithFakeGit(t)
		t.Setenv("PATH", binDir)

		_, err := Tags(context.Background(), "https://example.invalid/o/r")

		mustNotHaveRun(t, marker, filepath.Join(binDir, "git"))
		if err == nil {
			t.Fatal("想要报错")
		}
		// 铁律 8：不许含糊成一句「没找到 git」。用户得知道这不是环境没装好。
		if !strings.Contains(err.Error(), "攻击") || !strings.Contains(err.Error(), binDir) {
			t.Errorf("报错没说清这是一次可能的攻击、也没指出是哪个文件：%v", err)
		}
	})

	// 后面还有一个真的 git：必须跑那个。这一条钉的是「解析出绝对路径再 exec」——
	// 只要还按名字 exec，PATH 上排第一的仍然是 bin/ 里那个假货。
	t.Run("bin/ 排在真 git 前面", func(t *testing.T) {
		binDir, marker := toyBinWithFakeGit(t)
		real := t.TempDir()
		script := "#!/bin/sh\necho \"deadbee\trefs/tags/v1.0.0\"\n"
		if err := os.WriteFile(filepath.Join(real, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+real)

		tags, err := Tags(context.Background(), "https://example.invalid/o/r")

		mustNotHaveRun(t, marker, filepath.Join(binDir, "git"))
		if err != nil {
			t.Fatalf("Tags 报错: %v", err)
		}
		if len(tags) != 1 || tags[0] != "v1.0.0" {
			t.Fatalf("tags = %#v —— 跑的不是 %s 里那个真 git", tags, real)
		}
	})
}

// toyBinWithFakeGit 造一个 TOY_HOME，往它的 bin/ 里放一个「跑起来就留痕迹」的假
// git —— 就是一个包通过 [bin] git = "payload" 装出来的样子。
func toyBinWithFakeGit(t *testing.T) (binDir, marker string) {
	t.Helper()
	home := t.TempDir()
	binDir = filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(home, "it-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOY_HOME", home)
	return binDir, marker
}

func mustNotHaveRun(t *testing.T, marker, fake string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("toy 执行了 %s —— 那是某个包装进 bin/ 的文件（铁律 1）", fake)
	}
}

// bin/ 里躺着一个假 git 不该让整个 toy 罢工 —— 跳过它，继续找真的那个。
func TestLookupGitSkipsToyBinAndKeepsLooking(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	real := t.TempDir()
	for _, d := range []string{binDir, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+real)

	got, err := lookupGit()
	if err != nil {
		t.Fatalf("lookupGit 报错: %v", err)
	}
	if want := filepath.Join(real, "git"); got != want {
		t.Fatalf("lookupGit = %q，想要 %q（bin/ 里那个必须被跳过）", got, want)
	}
}

// PATH 里的相对项（"." 和那个到处都是的空项）指的是 toy 进程当前所在的目录。
// 在一个别人写得进去的目录里跑 toy，不该等于把铁律 1 交出去。
func TestLookupGitSkipsRelativePATHEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("TOY_HOME", t.TempDir())
	t.Setenv("PATH", "."+string(os.PathListSeparator)) // "." 和空项，两种写法都在

	if got, err := lookupGit(); err == nil {
		t.Fatalf("lookupGit = %q —— 那是从当前目录捡来的 git", got)
	}
}
