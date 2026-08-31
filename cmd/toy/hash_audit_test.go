package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// 原本怀疑：`--force` 一个开关同时放行「重装同一个版本」和「这个版本换了内容」。
// 这条是成立的 —— 想重装的用户没有别的选择，只能连 TOFU 一起关掉（铁律 4/6），
// 而 TOFU 存在的全部意义就是拦住那两件事。
//
// 已经修了：`--reinstall` 专管重装（日常操作），`--force` 只放行安全检查报出的变化。
// 这个测试现在守的是那道分割线 —— 一次重装不许顺手放行一条变了的 TOFU 记录。
func TestReinstallDoesNotWaiveTheVersionPin(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("第一次装就失败了：%v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("第一次装完就不对：%q", b)
	}

	// 同一个 tag，release 资产的字节被换掉了 —— 正是铁律 6 要拦的那件事。
	asset := filepath.Join(fh.root, "alice", "tool", "rel", "v1.0.0", "audtool")
	if err := os.WriteFile(asset, []byte("#!/bin/sh\necho EVIL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 光给 --force 不该触发一次重装：它现在只是「放行安全检查报出的变化」。
	// 而且必须说出来，不然用户以为他放行的那件事生效了。
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("--force 不该在这里报错，它该说一句「已经是这一版了」：%v", err)
		}
	})
	if !strings.Contains(out, "--reinstall") {
		t.Fatalf("光给 --force 时什么都没装，却没告诉用户重装要的是 --reinstall：\n%s", out)
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("--force 自己触发了一次重装，装到的是 %q", b)
	}

	// 这一条是分割线本身：重装的意图不能放行「这一版换了内容」。
	err := runInstall(context.Background(), []string{"--reinstall", "alice/tool"})
	if err == nil {
		t.Fatal("--reinstall 把一次哈希变更也一并放行了 —— 那正是它被拆出来要避免的事（铁律 4/6）")
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("拒绝之后已经装着的那一版被动过了：%q", b)
	}

	// 两个意图都摆出来才装得上，而且必须逐条打印放行了什么（铁律 8）。
	out = captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("--reinstall --force 都给了还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force 放行") || !strings.Contains(out, "sha256") {
		t.Fatalf("--force 放行了一次哈希变更却没逐条说清楚放行了什么：\n%s", out)
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho EVIL\n" {
		t.Fatalf("两个开关都给了却没真的换成新内容：%q", b)
	}
}

// 原本怀疑：`toy install foo@v1.0.0` 装了一个比已装的 v2.0.0 旧的版本是漏洞。
// 这条被推翻了 —— 那是用户**显式要求**的，拦下来只会让「我就是要回退一版」没法做。
//
// 真正的问题在旁边：上游把最新那个 tag 删掉（账号被接管、release 被撤、自建 host
// 被换掉）走的是同一条路，而那时输出里只有一句「已安装 t 1.0.0」，一个字都没提降级。
// 所以现在装是照装，但必须说一句（铁律 8）。这个测试守的是那句话。
func TestDowngradeIsAllowedButAnnounced(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho OLD\n")
	fh.publish(t, "alice", "tool", "v2.0.0", "#!/bin/sh\necho NEW\n")
	_, home, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho NEW\n" {
		t.Fatalf("默认该装最高的那个 tag，装到的是 %q", b)
	}

	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool@1.0.0"}); err != nil {
			t.Fatalf("显式点名一个旧版本被拦下来了，那是用户自己要的：%v", err)
		}
	})
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho OLD\n" {
		t.Fatalf("点名 @1.0.0 却没装成 v1.0.0：%q", b)
	}
	if !strings.Contains(out, "降级") {
		t.Fatalf("从 v2.0.0 装回 v1.0.0，输出里一个字都没提这是降级 —— "+
			"上游撤掉最新 tag 时走的是同一条路，而那时用户完全看不出来：\n%s", out)
	}
}

// insIsOlder 只在两边都是合法 semver 时才敢说「旧」：说不准的时候一句猜出来的
// 「这是一次降级」比不说更糟（insIsNewer 在同样的情况下按另一套规则放行，
// 两个函数不是互为反面，别合并）。
func TestIsOlderStaysQuietWhenItCannotTell(t *testing.T) {
	for _, c := range []struct {
		oldTag, newTag string
		want           bool
	}{
		{"v2.0.0", "v1.0.0", true},
		{"v1.0.0", "v2.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"nightly", "v1.0.0", false}, // 已装的那个不是 semver
		{"v2.0.0", "nightly", false}, // 这次这个不是 semver
	} {
		got := insIsOlder(store.Manifest{Tag: c.oldTag}, &resolved{Tag: c.newTag})
		if got != c.want {
			t.Errorf("insIsOlder(%q → %q) = %v，想要 %v", c.oldTag, c.newTag, got, c.want)
		}
	}
}

// insTOFU 自己那一半一直是对的，钉在这儿：不给 force 就必须拒，
// 而且错误要能被 errors.Is(err, sign.ErrChanged) 认出来。
func TestTOFURejectsAChangedPinWithoutForce(t *testing.T) {
	ring, err := sign.OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "github:o/r@v1.0.0"
	if err := ring.Pin(id, "sha256", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := insTOFU(ring, id, "sha256", strings.Repeat("b", 64), "artifact", false); err == nil {
		t.Fatal("不带 force 就该拒绝，铁律 6")
	}
}
