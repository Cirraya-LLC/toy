package main

// --force 收窄之后它还管着五件事（哈希 / 公钥 / 遮蔽 / 信任降级 / 来源）。
// 铁律 8 要求信任状态如实打印给用户看 —— 一次**静默的**放行等于没有这道检查，
// 所以这里逐条钉住「真的放行时必须说出放行了什么」。
// 哈希和公钥那两条在 hash_audit_test.go / sign_audit_test.go 里，这里是剩下三条。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
)

// 来源变了：索引条目只是个指针，改一行 source 就能把已装的命令换成另一个仓库的
// 内容。--force 放行得了，但不能不吭声。
func TestForceAnnouncesASourceChange(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	fh.publish(t, "bob", "tool", "v2.0.0", "#!/bin/sh\necho MOVED\n")
	_, _, writeIdx := audIndexSetup(t, fh)

	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	writeIdx("fake:bob/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("来源变了却一声不吭地装上了")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("确认之后还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force 放行") || !strings.Contains(out, "fake:bob/tool") {
		t.Fatalf("--force 把来源变更放行了，输出里却看不出换到了哪个仓库：\n%s", out)
	}
}

// 信任降级：索引条目上的 sig 被摘掉，「✓ 已签名」会悄悄变成「· 来源 …」。
// TOFU 记的是「上次是什么样」，所以这一条也要停下来，放行时也要说。
func TestForceAnnouncesATrustDowngrade(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, _ := audIndexSetup(t, fh)

	raw, err := os.ReadFile(filepath.Join(fh.root, "alice", "tool", "raw", "v1.0.0", "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	idxPath := sgnSignedIndexPath(t, home)
	sgnWriteSignedIndex(t, idxPath, "alice/tool", "fake:alice/tool", raw, sgnKey(t))
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}

	// 同一条记录，sig 和 key 被摘掉了。
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
		{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInstall(context.Background(), []string{"--reinstall", "alice/tool"}); err == nil {
		t.Fatal("那个勾悄悄没掉了，却照装不误")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("确认之后还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force 放行") || !strings.Contains(out, "签名") {
		t.Fatalf("--force 放行了一次信任降级，输出里却没说这个包不再是已签名的了：\n%s", out)
	}
}

// 命令名遮蔽（铁律 5）：`bin = "sudo"` 那一类。--force 是它唯一的出口，
// 那就更要说清楚顶掉的是 PATH 上的哪个文件。
func TestForceAnnouncesAShadowedCommand(t *testing.T) {
	p := i1New(t, "name = \"shadowpkg\"\n[script]\nshadowme = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\necho MINE\n", 0o644)

	other := t.TempDir()
	victim := filepath.Join(other, "shadowme")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// i1New 已经把 TOY_HOME 指到 p.home 了；bin 排在前面是文档教的写法。
	t.Setenv("PATH", filepath.Join(p.home, "bin")+string(os.PathListSeparator)+other)

	url := "file://" + filepath.Join(p.dir, "toy.toml")
	if err := runInstall(context.Background(), []string{url}); err == nil {
		t.Fatal("不给 --force 就把 PATH 上的 shadowme 顶掉了（铁律 5）")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", url}); err != nil {
			t.Fatalf("--force 之后还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force 放行") || !strings.Contains(out, victim) {
		t.Fatalf("--force 顶掉了 PATH 上的 %s，输出里却没说顶掉的是谁：\n%s", victim, out)
	}
}

// `toy install -h` 必须把 --reinstall 和 --force 的区别说清楚。
// 原来 -h 把 flag 包的输出丢进 io.Discard，用户只拿到一行语法串 ——
// 而这两个开关的分工恰恰只写在各自的 usage 里，等于谁也看不到。
func TestInstallHelpExplainsTheTwoSwitches(t *testing.T) {
	out := captureStderr(t, func() {
		if err := runInstall(t.Context(), []string{"-h"}); !errors.Is(err, errUsage) {
			t.Fatalf("-h 该走 usage 那条路：%v", err)
		}
	})
	for _, want := range []string{"-reinstall", "-force", "再装一次", "安全检查"} {
		if !strings.Contains(out, want) {
			t.Errorf("`toy install -h` 里没有 %q，用户无从知道该用哪个开关：\n%s", want, out)
		}
	}
}
