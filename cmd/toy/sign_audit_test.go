package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// sgnKey 造一把测试用的 ed25519 密钥对。
func sgnKey(t *testing.T) sign.KeyPair {
	t.Helper()
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

const sgnTOML = "name = \"tinyserve\"\n[bin]\ntinyserve = \"tinyserve\"\n[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n"

// 审计 S1：签名只证明「有人签过这条记录」，不证明「是这个仓库的主人签的」。
//
// 索引是一个谁都能 fork、谁都能提 PR 的 JSON。攻击者拿自己的私钥去签
// **别人的** id + source + toy.toml —— 签名当然验得过，因为消息里没有任何
// 东西把 key 和 owner 绑在一起。客户端于是打出
//
//	✓ 已签名 · Derrity (key <攻击者的指纹>)
//
// 括号外那个名字是仓库 owner，括号里那把钥匙是攻击者的。铁律 8 要求信任等级
// 如实打印，而这一行读起来就是「Derrity 签的」。
func TestAuditAnyoneCanSignSomeoneElsesEntry(t *testing.T) {
	attacker := sgnKey(t)
	e := index.Entry{
		ID:     "Derrity/tinyserve",
		Source: "github:Derrity/tinyserve",
		Key:    hex.EncodeToString(attacker.Public),
	}
	e.Sig = sign.Sign(attacker.Private, autSignMessage(e.ID, e.Source, []byte(sgnTOML)))

	res := insTestResolved(t, e.Source, "v1.0.0")
	res.Raw = []byte(sgnTOML)
	res.Entry = &e
	rsvApplySignature("default", e, res)

	if res.Trust != store.TrustSigned {
		return // 客户端连勾都不打了，更没得撒谎
	}
	// 勾还在的话，那一行能说的只有「索引里登记的这把钥匙签过这一版」。
	// 把仓库 owner 印在勾后面就是撒谎：签名消息里没有一个字节把它俩绑起来。
	detail := insTrustDetail(res)
	if strings.HasPrefix(detail, "Derrity ") || strings.Contains(detail, "Derrity (key") {
		t.Fatalf("攻击者用自己的钥匙签了 Derrity 的条目，客户端照样打「✓ 已签名 · %s」——"+
			"key 和 owner 之间没有任何绑定，这个勾只说明「有人签过」", detail)
	}
	if !strings.Contains(detail, res.Signer) {
		t.Fatalf("信任那一行至少要说清楚是哪一把钥匙签的：%q", detail)
	}
}

// 审计 S2：作者公钥的 TOFU 钉在 host:owner/repo 上，而那一串是从**索引条目的
// source** 推出来的。改掉 source 就等于换了一个身份，于是换钥匙这件事根本
// 不会被 Check 看到 —— 规格 §4 说「公钥首次安装钉住，之后变了要大声警告并拒绝」。
//
// 剧本：Derrity/tinyserve 已装、已签名、公钥 K 已钉住。攻击者提一个 PR，
// 把这一条的 source 改成自己的仓库、key/sig 换成自己的。`toy update` 会走
// m.IndexID 再查一次索引（insUpdateOne），拿到新 source，然后：
//   - insTrustDowngraded 只比等级，两边都是 signed，过；
//   - insTOFU 用新的 insPackageID 当 id，钥匙环里没有这一条，TrustNew，过。
//
// 整个过程没有任何一句话提到「作者公钥变了」。
func TestAuditChangingSourceRotatesTheKeyPinIdentity(t *testing.T) {
	ring, err := sign.OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victim := sgnKey(t)
	attacker := sgnKey(t)
	victimKey := hex.EncodeToString(victim.Public)
	attackerKey := hex.EncodeToString(attacker.Public)

	// 第一次安装：source 指向真作者，公钥 K 钉住。
	old := insTestResolved(t, "github:Derrity/tinyserve", "v1.0.0")
	old.Entry = &index.Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Key: victimKey}
	if err := ring.Pin(insKeyID(old), "key", victimKey); err != nil {
		t.Fatal(err)
	}

	// 索引条目被改：同一个 id，source 换成攻击者的仓库，key/sig 也是攻击者的。
	e := index.Entry{ID: "Derrity/tinyserve", Source: "github:evil/tinyserve", Key: attackerKey}
	e.Sig = sign.Sign(attacker.Private, autSignMessage(e.ID, e.Source, []byte(sgnTOML)))
	next := insTestResolved(t, e.Source, "v9.9.9")
	next.Raw = []byte(sgnTOML)
	next.Entry = &e
	rsvApplySignature("default", e, next)
	if next.Trust != store.TrustSigned {
		t.Fatalf("攻击者的签名该是验得过的，实际 trust=%q", next.Trust)
	}

	// 这就是 insPerform 里那一次检查。
	trust, pinned, err := ring.Check(insKeyID(next), "key", attackerKey)
	if err != nil {
		t.Fatal(err)
	}
	if trust == sign.TrustChanged {
		return // 修好了
	}
	t.Fatalf("同一个索引 id %q 的作者公钥从 %s… 换成了 %s…，"+
		"但 TOFU 的 id 跟着 source 从 %q 变成了 %q，于是 Check 返回 %v（钉住的旧值 %q）—— "+
		"换钥匙这件事一句话都没有",
		e.ID, victimKey[:8], attackerKey[:8],
		insKeyID(old), insKeyID(next), trust, pinned)
}

// 审计 S3：`toy search --json` 的 "signed": true 只看条目里有没有一把
// 格式合法的公钥，**从来不验签名**。规格 §5 原话：「只有 key 不算已签名」。
// 于是任何人往别人的条目里抄一把公钥（连签名都不用伪造），机器可读的输出
// 就会说这个包是已签名的。
func TestAuditSearchJSONCallsAnythingWithAKeySigned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	// 一把合法的公钥 + 一段根本验不过的签名。
	bogus := sgnKey(t)
	idx := index.Index{Version: 1, Packages: []index.Entry{{
		ID:     "Derrity/tinyserve",
		Source: "github:Derrity/tinyserve",
		Desc:   "一个很小的静态文件服务器",
		Key:    hex.EncodeToString(bogus.Public),
		Sig:    "AAAA", // 不是有效签名，长度都不对
	}}}
	data, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(idxPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// 先确认这个 sig 真的验不过，免得测的是别的东西。
	if err := sign.Verify(hex.EncodeToString(bogus.Public),
		autSignMessage(idx.Packages[0].ID, idx.Packages[0].Source, []byte(sgnTOML)), "AAAA"); err == nil {
		t.Fatal("这段签名不该验得过")
	}

	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(store.Config{
		Indexes: []store.IndexRef{{Name: "t", URL: "file://" + idxPath}},
	}); err != nil {
		t.Fatal(err)
	}

	out := sgnCaptureStdout(t, func() {
		if err := runSearch(t.Context(), []string{"--json", "tinyserve"}); err != nil {
			t.Fatalf("search 失败：%v", err)
		}
	})

	var got struct {
		Results []struct {
			ID     string `json:"id"`
			Signed bool   `json:"signed"`
			HasKey bool   `json:"has_key"`
			FP     string `json:"key_fingerprint"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	if len(got.Results) != 1 {
		t.Fatalf("想要一条结果，得到 %d 条：%s", len(got.Results), out)
	}
	if got.Results[0].Signed {
		t.Fatalf("签名验不过，`toy search --json` 却说 %q 是 signed:true —— "+
			"这一格只看了 key 在不在（cmd/toy/query.go 的 rsvFingerprint 那一行）", got.Results[0].ID)
	}
	// 「有一把公钥」本身是有用的信息，别顺手删掉 —— 只是它不叫「已签名」。
	if !got.Results[0].HasKey || got.Results[0].FP == "" {
		t.Fatalf("条目里确实有一把合法公钥，has_key/key_fingerprint 该照实说：%s", out)
	}
}

// 原本怀疑：`--force` 一个开关管着六件事，用户为了「重装同一个版本」或者
// 「我知道这个命令名会遮蔽」打上它，顺带就静默接受了「作者换了一把公钥」——
// 而那正是规格 §4 说要「大声警告并拒绝」的。
//
// 这条是成立的，已经修了：`--reinstall` 专管重装，`--force` 只放行安全检查报出的
// 变化，而且真的放行时逐条打印放行了什么（铁律 8）。
// 这个测试现在守的是换钥匙那一条 —— 一次重装不许把它一并放行。
func TestReinstallDoesNotWaiveAKeySwap(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, _ := audIndexSetup(t, fh)

	raw, err := os.ReadFile(filepath.Join(fh.root, "alice", "tool", "raw", "v1.0.0", "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	idxPath := sgnSignedIndexPath(t, home)
	a, b := sgnKey(t), sgnKey(t)
	write := func(kp sign.KeyPair) {
		sgnWriteSignedIndex(t, idxPath, "alice/tool", "fake:alice/tool", raw, kp)
	}

	write(a)
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.FindByName("audtool")
	if err != nil {
		t.Fatal(err)
	}
	if m.Trust != store.TrustSigned {
		t.Fatalf("这一版该是已签名的，实际是 %v —— 后面的换钥匙就无从谈起", m.Trust)
	}

	// 索引里那把公钥被换成了另一把（新签名自洽，所以信任等级没有降级，
	// 唯一能拦住它的就是 TOFU 记的「上次是哪把钥匙」）。
	write(b)

	err = runInstall(context.Background(), []string{"--reinstall", "alice/tool"})
	if err == nil {
		t.Fatal("--reinstall 把「作者换了一把公钥」也一并放行了 —— " +
			"重装是日常操作，不该顺手接受一个换了的作者身份（规格 §4）")
	}
	if !errors.Is(err, sign.ErrChanged) {
		t.Fatalf("拒是拒了，但错误不带 sign.ErrChanged，调用方分不清这是哪一种拒绝：%v", err)
	}

	// --force 才放行，而且必须说清楚放行的是换钥匙这件事。
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("--reinstall --force 都给了还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "author public key") {
		t.Fatalf("--force 放行了一次换钥匙却没说是放行了什么：\n%s", out)
	}
}

// sgnSignedIndexPath 把 home 的配置指到一个新的索引文件上（沿用 fake 托管方）。
func sgnSignedIndexPath(t *testing.T, home string) string {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signed.json")
	cfg.Indexes = []store.IndexRef{{Name: "signed", URL: "file://" + path}}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// sgnWriteSignedIndex 写一条【带签名】的索引条目。签的就是 autSignMessage 那三段。
func sgnWriteSignedIndex(t *testing.T, path, id, source string, raw []byte, kp sign.KeyPair) {
	t.Helper()
	e := index.Entry{
		ID:     id,
		Source: source,
		Desc:   "d",
		Key:    hex.EncodeToString(kp.Public),
		Sig:    sign.Sign(kp.Private, autSignMessage(id, source, raw)),
	}
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{e}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 审计 S5（对照，不是问题）：签名消息 id+"\n"+source+"\n"+toy.toml 缺长度前缀，
// 但 id 和 source 都被白名单挡住了换行和空格，能在边界上挪动的只有
// source 尾部的空白（ref.Parse 会 TrimSpace，index.validEntry 因此放行），
// 而它只能吞掉 toy.toml 开头的空行 —— TOML 语义不变。留个测试钉住这个前提：
// 一旦 source 允许了内部空白或换行，拼接就真的有歧义了。
func TestAuditSignMessageBoundariesStayUnambiguous(t *testing.T) {
	for _, bad := range []string{
		"github:a/b\nname = \"x\"",
		"github:a/b name",
		"https://x/toy.toml\nname",
	} {
		idx, skipped, err := index.Load(sgnIndexJSON(t, bad), "t")
		if err != nil {
			continue
		}
		if skipped == 0 || len(idx.Packages) != 0 {
			t.Fatalf("source %q 带内部空白/换行却被索引接受了 —— "+
				"这样 source 和 toy.toml 之间的分界就可以任意挪动，"+
				"一条签名能对上两组不同的输入", bad)
		}
	}
	// 尾部空白确实过得去，记在案。
	idx, _, err := index.Load(sgnIndexJSON(t, "github:a/b \n"), "t")
	if err == nil && len(idx.Packages) == 1 {
		t.Logf("source 尾部空白被接受（%q）：只能吞掉 toy.toml 开头的空白，语义不变",
			idx.Packages[0].Source)
	}
}

func sgnIndexJSON(t *testing.T, source string) []byte {
	t.Helper()
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
		{ID: "a/b", Source: source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sgnCaptureStdout 抓一段代码往 os.Stdout 写的东西。
func sgnCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	defer func() { os.Stdout = old; f.Close() }()
	fn()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
