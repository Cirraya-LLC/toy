package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 审计（铁律 5 的盲区）：遮蔽检查只挡「PATH 上【已经存在】的同名程序」。
// 而 toy 自己会 exec 一个从 PATH 解析的名字：git（internal/host.Tags 用
// exec.CommandContext(ctx,"git",…)，autGitRemote 和 doctor 也一样）。
//
// 机器上还没装 git 的时候（macOS 没装 Xcode CLT、slim 容器、CI 基础镜像都很常见），
// LookPath("git") 找不到东西 → 不算遮蔽 → 一个 toy.toml 写 [bin] git = "payload"
// 就能把 ~/.toy/bin/git 占下来，而 ~/.toy/bin 正是 toy doctor 教用户放在 PATH 最前面的目录。
// 下一次任何一条 `toy install github:…` / `toy update` / `toy doctor` 都会执行它。
func TestLinkLetsAPackageClaimTheNameToyItselfExecutes(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// 一台没装 git 的机器。
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	pkg := filepath.Join(s.Root, "pkg", "evil")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(pkg, "payload")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	link, err := s.Link("git", payload, false) // 注意：没有 --force
	if err != nil {
		return // 修好了：toy 拒绝把自己要执行的名字交出去
	}
	t.Fatalf("没有 --force 就把 %s 建出来了 —— 它指向包里的 %s，"+
		"而 toy 自己 exec 的 \"git\" 就是从 PATH 上解析的（BinDir 通常还是 PATH 第一项）。"+
		"下一条 toy install github:… 就会跑这个文件（铁律 1）", link, payload)
}

// --force 也不许放行：bin = "git" 跟 bin = "sudo" 不是一类风险，
// 用户对着「确认要覆盖再加 --force」判断不出后果。
func TestForceCannotClaimTheNameToyItselfExecutes(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(s.Root, "pkg", "evil")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(pkg, "payload")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "GIT"} {
		if link, err := s.Link(name, payload, true); err == nil {
			t.Fatalf("--force 把 %s 建出来了 —— toy 自己 exec 的 git 就从 PATH 上解析", link)
		}
	}
}
