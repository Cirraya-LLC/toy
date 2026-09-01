package main

import (
	"strings"
	"testing"
)

// 内联条目必须与版本无关：它住在索引里、跟着上游最新 tag 走，而 [sha256]
// 钉的是某一个版本的字节。留着的话上游一发新版，每个用户都会拿到
// 「哈希对不上，拒绝安装」—— 读起来完全像投毒警告，实际只是作者正常发版。
func TestInlineStripsSHA256(t *testing.T) {
	raw := []byte(`name = "fd"
desc = "找文件"

[bin]
fd = "fd"

[artifact]
"linux/amd64" = "fd_{version}_linux_amd64.tar.gz"

# 有 sha256 就强制校验，这几行是现算的。
[sha256]
"linux/amd64" = "aa"

[script]
x = "y"
`)
	got, ok := autStripSHA256(raw)
	if !ok {
		t.Fatal("有 [sha256] 却报告没去掉")
	}
	s := string(got)
	if strings.Contains(s, "[sha256]") || strings.Contains(s, `"linux/amd64" = "aa"`) {
		t.Fatalf("[sha256] 段没去干净：\n%s", s)
	}
	if strings.Contains(s, "有 sha256 就强制校验") {
		t.Fatalf("紧挨着的说明注释该一起去掉，否则会指着一个不存在的段落：\n%s", s)
	}
	// 其余字节一字不动 —— 客户端验签名验的就是这份原文。
	for _, must := range []string{`name = "fd"`, `[bin]`, `fd = "fd"`, `[artifact]`,
		`"linux/amd64" = "fd_{version}_linux_amd64.tar.gz"`, `[script]`, `x = "y"`} {
		if !strings.Contains(s, must) {
			t.Fatalf("误伤了 %q：\n%s", must, s)
		}
	}
}

func TestInlineLeavesFileWithoutSHA256Alone(t *testing.T) {
	raw := []byte("name = \"fd\"\n[bin]\nfd = \"fd\"\n")
	got, ok := autStripSHA256(raw)
	if ok {
		t.Fatal("没有 [sha256] 却报告去掉了东西")
	}
	if string(got) != string(raw) {
		t.Fatalf("不该改动：%q", got)
	}
}
