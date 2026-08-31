package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/store"
)

// 索引里的 id 是 "Derrity/tinyserve"，安装清单里的 id 是 "github:Derrity/tinyserve"，
// 两头对不上就会让每一条搜索结果都显示成「没装过」。
func TestInstalledIndexMatchesBothShapes(t *testing.T) {
	idx := qryInstalledIndex([]store.Manifest{{
		ID: "github:Derrity/tinyserve", Name: "tinyserve", Version: "1.2.3",
		Host: "github", Owner: "Derrity", Repo: "tinyserve",
	}})

	for _, key := range []string{
		"github:Derrity/tinyserve", // 清单自己的 id
		"Derrity/tinyserve",        // 索引条目的 id
		"derrity/TINYSERVE",        // owner/repo 在托管方那边就是大小写不敏感的
	} {
		m, ok := qryLookupInstalled(idx, key)
		if !ok || m.Version != "1.2.3" {
			t.Fatalf("%q 应该查得到已装的 1.2.3，得到 ok=%v version=%q", key, ok, m.Version)
		}
	}
	if _, ok := qryLookupInstalled(idx, "someone/else"); ok {
		t.Fatal("没装过的包不该被查出来")
	}
}

// 铁律 8：认不出的信任等级不能被渲染成安全的那一档，
// 「说是已签名却给不出签名者」同理 —— 那等于替一把不知道是谁的钥匙背书。
func TestTrustLabelNeverOverclaims(t *testing.T) {
	if s := qryTrustLabel(store.TrustSigned, ""); strings.HasPrefix(s, "✓") {
		t.Fatalf("没有签名者时不该打勾，得到 %q", s)
	}
	if s := qryTrustLabel("totally-fine", ""); !strings.Contains(s, "totally-fine") || strings.HasPrefix(s, "✓") {
		t.Fatalf("不认识的等级要如实说出来且不打勾，得到 %q", s)
	}
	if s := qryTrustLabel(store.TrustSigned, "a3f9…"); !strings.Contains(s, "a3f9…") {
		t.Fatalf("已签名要带上指纹，得到 %q", s)
	}
	if s := qryTrustLabel(store.TrustUnverified, ""); strings.HasPrefix(s, "✓") {
		t.Fatalf("未签名不该打勾，得到 %q", s)
	}
}

// 索引里的 desc 是别人写的：\r + 一段 ANSI 就能把刚打出来的信任等级那一行
// 盖掉重写，而 \t 和 \n 会把表格的列算错。进 tabwriter 之前必须转义掉。
func TestCellEscapesControlBytes(t *testing.T) {
	got := qryCell("\r\x1b[32m✓ 已签名\tx\ny")
	for _, bad := range []string{"\r", "\x1b", "\t", "\n"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q 还留着控制字符 %q", got, bad)
		}
	}
	if !strings.Contains(got, `\x1b`) {
		t.Fatalf("转义要看得见（\\xNN），删掉等于替对方把痕迹擦干净了：%q", got)
	}
	if s := "普通描述"; qryCell(s) != s {
		t.Fatalf("正常文本不该被动过，得到 %q", qryCell(s))
	}
}

func TestTruncKeepsRunesIntact(t *testing.T) {
	if got := qryTrunc("一二三四五", 3); got != "一二三…" {
		t.Fatalf("按 rune 截断，得到 %q", got)
	}
	if got := qryTrunc("短", 10); got != "短" {
		t.Fatalf("不超长就别动它，得到 %q", got)
	}
}

func TestIndexURLAcceptsBarePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "index.json")
	got, err := qryIndexURL(p)
	if err != nil || got != "file://"+filepath.ToSlash(p) {
		t.Fatalf("裸路径应该当本地文件处理，得到 %q（err=%v）", got, err)
	}
	if got, err := qryIndexURL("https://example.com/index.json"); err != nil || got != "https://example.com/index.json" {
		t.Fatalf("https 地址应该原样收下，得到 %q（err=%v）", got, err)
	}
	// scheme 不在白名单里的一律拒 —— 索引地址会被直接交给 fetch。
	if _, err := qryIndexURL("ftp://example.com/index.json"); err == nil {
		t.Fatal("ftp:// 不该被收下")
	}
	if _, err := qryIndexURL("   "); err == nil {
		t.Fatal("空地址不该被收下")
	}
}
