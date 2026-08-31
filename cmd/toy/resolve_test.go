package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// 挑版本是 install / update 都要走的那一步，选错了就装错东西。
func TestPickVersion(t *testing.T) {
	tags := []string{"v1.0.0", "release-junk", "v2.0.0-rc.1", "v1.2.0"}

	v, err := rsvPickVersion(tags, "")
	if err != nil || v.Raw != "v1.2.0" {
		t.Fatalf("不带 --version 应该挑最高的正式版 v1.2.0，得到 %q（err=%v）", v.Raw, err)
	}
	// 用户写 1.2.0 和写 v1.2.0 指的是同一个 tag。
	if v, err := rsvPickVersion(tags, "1.2.0"); err != nil || v.Raw != "v1.2.0" {
		t.Fatalf("1.2.0 应该对上 tag v1.2.0，得到 %q（err=%v）", v.Raw, err)
	}
	// 指名一个不存在的版本要报错，并且把有哪些版本说出来。
	_, err = rsvPickVersion(tags, "v9.9.9")
	if err == nil || !strings.Contains(err.Error(), "v1.2.0") {
		t.Fatalf("没有的版本要报错并列出现有版本，得到 %v", err)
	}
}

// 两个索引名转义之后不能撞到同一份缓存 —— 撞了会在离线时拿出别人的索引。
func TestIndexCacheNamesDoNotCollide(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a, b := rsvIndexCache(st, "a/b"), rsvIndexCache(st, "a:b"); a == b {
		t.Fatalf("%q 和 %q 撞到了同一个缓存文件 %s", "a/b", "a:b", a)
	}
}

func TestLoadIndexesCachesAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "idx.json")
	body := `{"version":1,"packages":[{"id":"Derrity/tinyserve","source":"github:Derrity/tinyserve"}]}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := store.Config{Indexes: []store.IndexRef{{Name: "local", URL: "file://" + src}}}

	idxs, err := loadIndexes(context.Background(), st, cfg)
	if err != nil || len(idxs) != 1 {
		t.Fatalf("应该加载出 1 个索引，得到 %d 个（err=%v）", len(idxs), err)
	}

	// 源没了也要能用上次缓存的那份，而不是变成「搜不到这个包」。
	os.Remove(src)
	idxs, err = loadIndexes(context.Background(), st, cfg)
	if err != nil || len(idxs) != 1 {
		t.Fatalf("源不可达时应该回退到缓存，得到 %d 个（err=%v）", len(idxs), err)
	}
	if _, ok := idxs[0].Find("derrity/tinyserve"); !ok {
		t.Fatal("缓存里的条目没读出来")
	}

	// 缓存也没有时，全部失败要如实报错 —— 不能静默地返回一个空结果。
	os.RemoveAll(st.IndexDir())
	if _, err := loadIndexes(context.Background(), st, cfg); err == nil {
		t.Fatal("索引全拉不到还说成功了")
	}
}

// TestRsvApplySignatureOnlyTrustsAVerifiedSignature 是铁律 8 在索引这条路上的落点：
// 「✓ 已签名」只能在真的验过签名之后才打得出来。索引是个谁都能 fork、谁都能提 PR
// 的 JSON 文件，往条目里抄一把公钥是零成本的 —— 见到 key 就打勾等于把这三个字
// 白送给任何一个能改索引的人。
func TestRsvApplySignatureOnlyTrustsAVerifiedSignature(t *testing.T) {
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const id, source = "Derrity/tinyserve", "github:Derrity/tinyserve"
	raw := []byte("name = \"tinyserve\"\n")
	pub := hex.EncodeToString(kp.Public)
	good := sign.Sign(kp.Private, autSignMessage(id, source, raw))

	other, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		entry index.Entry
		raw   []byte
		want  string // 想要的 Trust
	}{
		{"验过的签名才算已签名",
			index.Entry{ID: id, Source: source, Key: pub, Sig: good}, raw, store.TrustSigned},
		{"有 key 没 sig 不算",
			index.Entry{ID: id, Source: source, Key: pub}, raw, store.TrustHost},
		{"toy.toml 被换过就验不过",
			index.Entry{ID: id, Source: source, Key: pub, Sig: good},
			[]byte("name = \"tinyserve\"\n# 偷偷加一行\n"), store.TrustHost},
		{"别人的钥匙配这个签名也不算",
			index.Entry{ID: id, Source: source, Key: hex.EncodeToString(other.Public), Sig: good},
			raw, store.TrustHost},
		{"key 不是合法公钥",
			index.Entry{ID: id, Source: source, Key: "zzzz", Sig: good}, raw, store.TrustHost},
		{"没认领过名字就没有签名这回事",
			index.Entry{ID: id, Source: source}, raw, store.TrustHost},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := &resolved{Trust: store.TrustHost, Raw: c.raw}
			rsvApplySignature("测试索引", c.entry, res)
			if res.Trust != c.want {
				t.Fatalf("Trust = %q，想要 %q", res.Trust, c.want)
			}
			if c.want != store.TrustSigned && res.Signer != "" {
				t.Fatalf("没验过就不该给出签名者，得到 %q", res.Signer)
			}
			if c.want == store.TrustSigned && res.Signer == "" {
				t.Fatal("验过了却说不出是谁签的")
			}
		})
	}
}
