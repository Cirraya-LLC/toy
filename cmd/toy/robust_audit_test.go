package main

// 正确性 / 健壮性审计。四条各自证明一个具体的坏结果，不是风格意见。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// 审计 1：Ctrl-C 打在 `git ls-remote` 上时，取消信号被 %v 吃掉了。
//
// host.Tags 在 ctx 已取消时用 fmt.Errorf("…（%v）", ctxErr) 包装 —— %v 不是 %w，
// 所以 errors.Is(err, context.Canceled) 是 false。后果：
//   - runUpdate 里那条 `case errors.Is(err, context.Canceled): return err` 永远
//     进不去，Ctrl-C 之后它会继续把剩下每一个包都跑一遍（每个立刻失败），
//     最后以「N 个包没能更新」+ 退出码 1 收场，而不是「已取消」+ 130。
//   - runInstall 同理：main 里那条 context.Canceled 分支走不到。
func TestCanceledLsRemoteStillLooksLikeAPlainFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := host.Tags(ctx, "https://github.com/golang/go")
	if err == nil {
		t.Fatal("ctx 已取消，Tags 不该成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ctrl-C 之后 host.Tags 的错误不带 context.Canceled，"+
			"runUpdate 会把它当成一个普通失败继续往下跑：%v", err)
	}
}

// 审计 2：pkg 目录那一段用的是 toy.toml 里的 name，不是仓库名 —— 同一个 owner
// 下两个仓库只要 name 撞了，就共用一个安装目录。
//
// insPerform 会先 insRemovePkgDir(pkgDir) 再解压，所以装第二个包会把第一个包的
// 文件整个删掉：第一个包的清单还在、`toy list` 照样列出来，但它的软链全部悬空。
// 卸载第二个包同样会把第一个包的目录一起删走。
func TestTwoReposWithTheSameToyfileNameShareOnePkgDir(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := insTestResolved(t, "github:alice/tool-rs", "v1.0.0")
	b := insTestResolved(t, "github:alice/tool-go", "v1.0.0")
	// 两个仓库的 toy.toml 都写 name = "tool"（完全合法，toyfile.Parse 不会拦）。
	tf := &toyfile.Toyfile{Name: "tool"}
	dirA := st.PkgDir(insHostSeg(a), insOwnerSeg(a), insNameSeg(a, tf), insDirVersion(a))
	dirB := st.PkgDir(insHostSeg(b), insOwnerSeg(b), insNameSeg(b, tf), insDirVersion(b))
	if dirA != dirB {
		return // 已经修好了
	}
	// 证明后果：装 A，再装 B 的第一步 insRemovePkgDir 就把 A 的文件删了。
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dirA, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := insRemovePkgDir(st, dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Fatalf("以为 A 的可执行文件还在？err=%v", err)
	}
	t.Fatalf("github:alice/tool-rs 和 github:alice/tool-go 共用 %s —— "+
		"装后一个会静默删掉前一个的文件，而前一个的清单和软链都还在。"+
		"pkg 那一段该用仓库名（或带上 repo），不是 toy.toml 里的 name", dirA)
}

// 审计 3：索引里的 source 一变，装出来的是【另一个 id】的清单，而旧清单原地留着
// —— 但 insCleanOld 已经把旧的 pkg 目录删了。
//
// 索引维护者把仓库从一处挪到另一处（完全正常的维护动作）就会触发：`toy update`
// 现在会先拦下来要 --force（那是另一条审计），用户确认之后装一次，如果这时不删
// 旧清单，`toy list` 里那个包就会出现两次、其中一份指向一个已经被删掉的目录，
// `toy uninstall <名字>` 直接报「对上了 2 个已装的包」。
func TestSourceChangeLeavesAStaleManifestBehind(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho HERE\n")
	fh.publish(t, "bob", "tool", "v2.0.0", "#!/bin/sh\necho MOVED\n")

	st, _, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}

	// 仓库搬家了。来源变了必须先被拦下 —— 那一条是审计 1。
	writeIdx("fake:bob/tool")
	if err := runUpdate(context.Background(), nil); err == nil {
		t.Fatal("来源变了却一声不吭地升级了")
	}
	// 用户核对过，确认这次搬家是真的。
	if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
		t.Fatalf("确认之后还是装不上：%v", err)
	}

	ms, err := st.ListManifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		t.Fatalf("搬完家之后有 %d 份清单（%v），应该只剩 1 份 —— insPerform 换了 ID 却没删旧清单；"+
			"`toy uninstall tool` 现在会说「对上了 %d 个已装的包」", len(ms), ids, len(ms))
	}
}

// 审计 4（低）：[script] 和 [bin] 并存时，[bin] 被静默丢弃。
// toyfile.Parse 允许两段并存并且逐条校验 [bin]，但 insLink 在 IsScript() 时
// 只看 tf.Script —— 作者声明的命令一个都不会出现，也没有任何地方说一声。
func TestScriptFormSilentlyDropsBin(t *testing.T) {
	tf, err := toyfile.Parse([]byte(`
name = "gitwip"
[bin]
wip = "gitwip.sh"
[script]
gitwip = "gitwip.sh"
`))
	if err != nil {
		t.Skipf("这份 toy.toml 已经被拒了，问题没了：%v", err)
	}
	if tf.IsScript() && len(tf.Bin) > 0 {
		t.Logf("作者声明了 [bin] wip=%q，script 形态下它被静默丢掉了", tf.Bin["wip"])
	}
}
