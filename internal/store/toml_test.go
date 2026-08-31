package store

import (
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
)

func TestTOMLStringRoundTrip(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"普通", "tinyserve"},
		{"中文", "一个很小的静态文件服务器"},
		{"双引号", `他说"你好"`},
		{"反斜杠", `C:\path\to`},
		{"井号", "https://example.com/x#frag"},
		{"换行制表", "a\nb\tc\r\n"},
		{"控制字符", "a\x00\x01b"},
		{"等号", "k = v = w"},
		{"方括号", "a[b]c"},
		{"空串", ""},
		{"emoji", "🧸 toy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quoted := tomlQuote(tc.in)
			got, rest, err := scanTOMLString(quoted)
			if err != nil {
				t.Fatalf("scan %s: %v", quoted, err)
			}
			if rest != "" {
				t.Fatalf("剩下 %q，应该读干净", rest)
			}
			if got != tc.in {
				t.Fatalf("got %q，want %q（编码成 %s）", got, tc.in, quoted)
			}
		})
	}
}

func TestDecodeTOML(t *testing.T) {
	src := `
# 注释整行
mirrors = ["a", "b"]   # 行尾注释
note = "带 # 号的字符串不能被注释切掉"
empty = []

[[index]]
name = "default"
url  = "https://example.com/i.json"

[[index]]
name = "本地"
url  = "file:///tmp/i.json"

[hosts.github]
raw = "https://raw.example.com/{owner}"

[hosts."有空格 的名字"]
raw = "x"

multi = [
  "第一行",
  "第二行",   # 数组里也能有注释
]
`
	doc, err := decodeTOML([]byte(src))
	if err != nil {
		t.Fatalf("decodeTOML: %v", err)
	}
	if got := strings.Join(doc.root.strs("mirrors"), ","); got != "a,b" {
		t.Errorf("mirrors = %q", got)
	}
	if got := doc.root.str("note"); !strings.Contains(got, "#") {
		t.Errorf("note = %q —— 引号里的 # 被当成注释切掉了", got)
	}
	if got := doc.root.strs("empty"); got == nil || len(got) != 0 {
		t.Errorf("empty = %v，想要空数组（不是 nil）", got)
	}
	if got := len(doc.arrs["index"]); got != 2 {
		t.Fatalf("[[index]] 有 %d 条，想要 2 条", got)
	}
	if doc.arrs["index"][1].str("name") != "本地" {
		t.Errorf("第二条 index = %+v", doc.arrs["index"][1])
	}
	hosts := doc.subsUnder("hosts")
	if len(hosts) != 2 {
		t.Fatalf("hosts 有 %d 个，想要 2 个", len(hosts))
	}
	if hosts["有空格 的名字"] == nil {
		t.Errorf("带引号的表名没被去引号：%v", hosts)
	}
	// 跨行数组：它写在最后一个表头之后，所以属于那个表
	if got := strings.Join(hosts["有空格 的名字"].strs("multi"), "|"); got != "第一行|第二行" {
		t.Errorf("跨行数组 = %q", got)
	}
}

func TestDecodeTOMLRejectsBrokenInput(t *testing.T) {
	tests := []struct{ name, src string }{
		{"字符串没闭合", `name = "abc`},
		{"表头没闭合", "[hosts.github\n"},
		{"表数组没闭合", "[[index]\n"},
		{"既不是表头也不是赋值", "just some words\n"},
		{"空表名", "[]\n"},
		{"空段表名", "[a..b]\n"},
		{"不认识的转义", `name = "a\qb"`},
		{"数组没闭合", `x = ["a", "b"`},
		{"坏的 \\u", `name = "a\uZZZZ"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeTOML([]byte(tc.src)); err == nil {
				t.Fatalf("%q 应该报错", tc.src)
			}
		})
	}
}

func TestDecodeTOMLUnicodeEscapes(t *testing.T) {
	doc, err := decodeTOML([]byte(`a = "\u4F60\u597D"` + "\n" + `b = "\U0001F9F8"`))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.root.str("a"); got != "你好" {
		t.Errorf("\\u 解错了：%q", got)
	}
	if got := doc.root.str("b"); got != "🧸" {
		t.Errorf("\\U 解错了：%q", got)
	}
}

// 编码出来的东西必须自己能再读回来 —— 这是清单能被 uninstall 用的前提。
func TestEncodedConfigIsDecodable(t *testing.T) {
	c := Config{
		Indexes: []IndexRef{{Name: `带"引号"`, URL: "file:///tmp/a#b.json"}},
		Mirrors: []string{"a\nb", ""},
		Hosts:   map[string]host.Host{"我的 git": {Raw: "r", Clone: "c", Release: "l", Web: "w"}},
	}
	doc, err := decodeTOML(encodeConfig(c))
	if err != nil {
		t.Fatalf("自己编码的东西自己读不回来：%v\n%s", err, encodeConfig(c))
	}
	if doc.arrs["index"][0].str("name") != `带"引号"` {
		t.Errorf("index.name = %q", doc.arrs["index"][0].str("name"))
	}
	if doc.arrs["index"][0].str("url") != "file:///tmp/a#b.json" {
		t.Errorf("index.url = %q —— URL 里的 # 被当注释了", doc.arrs["index"][0].str("url"))
	}
	if got := doc.subsUnder("hosts")["我的 git"]; got == nil || got.str("release") != "l" {
		t.Errorf("hosts = %v", doc.subsUnder("hosts"))
	}
	if got := strings.Join(doc.root.strs("mirrors"), "|"); got != "a\nb|" {
		t.Errorf("mirrors = %q", got)
	}
}

func TestTomlKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"github", "github"},
		{"my-git_2", "my-git_2"},
		{"有中文", `"有中文"`},
		{"有 空格", `"有 空格"`},
		{"a.b", `"a.b"`},
		{"", `""`},
	}
	for _, tc := range tests {
		if got := tomlKey(tc.in); got != tc.want {
			t.Errorf("tomlKey(%q) = %s，want %s", tc.in, got, tc.want)
		}
	}
}
