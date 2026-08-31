package ref

import (
	"errors"
	"strings"
	"testing"
)

func TestParseOK(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Ref
	}{
		{"索引形态", "Derrity/tinyserve", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve"}},
		{"索引形态带版本", "Derrity/tinyserve@v1.2.3", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve", Version: "v1.2.3"}},
		{"host 形态", "github:Derrity/tinyserve", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve"}},
		{"host 形态带版本", "github:Derrity/tinyserve@v1.2.3", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve", Version: "v1.2.3"}},
		{"host 名大小写不敏感", "GitHub:Derrity/tinyserve", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve"}},
		{"别的 host", "codeberg:a/b", Ref{Kind: KindHost, Host: "codeberg", Owner: "a", Repo: "b"}},
		{"host 名可以带点和横线", "git.example-1:a/b", Ref{Kind: KindHost, Host: "git.example-1", Owner: "a", Repo: "b"}},
		{"两边空白", "  Derrity/tinyserve  ", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve"}},
		{"名字里的点下划线横线", "a.b_c-d/e.f_g-h", Ref{Kind: KindIndex, Owner: "a.b_c-d", Repo: "e.f_g-h"}},
		{"预发布版本", "a/b@v1.2.3-rc.1", Ref{Kind: KindIndex, Owner: "a", Repo: "b", Version: "v1.2.3-rc.1"}},
		{"带构建元数据的版本", "a/b@1.0.0+build.5", Ref{Kind: KindIndex, Owner: "a", Repo: "b", Version: "1.0.0+build.5"}},
		{"https", "https://example.com/toy.toml", Ref{Kind: KindURL, URL: "https://example.com/toy.toml"}},
		{"http", "http://example.com/x/toy.toml", Ref{Kind: KindURL, URL: "http://example.com/x/toy.toml"}},
		{"file", "file:///home/u/toy.toml", Ref{Kind: KindURL, URL: "file:///home/u/toy.toml"}},
		{"scheme 规范成小写", "HTTPS://example.com/toy.toml", Ref{Kind: KindURL, URL: "https://example.com/toy.toml"}},
		{"URL 里的 @ 不是版本号", "https://u@example.com/toy.toml", Ref{Kind: KindURL, URL: "https://u@example.com/toy.toml"}},
		// 主机名大小写不敏感：不统一小写的话，只差大小写的两个地址会是两条 TOFU 记录，
		// Example.COM 就能绕过 example.com 已经钉住的 hash（规格 §4）。
		{"主机名规范成小写", "https://Example.COM/Toy.TOML", Ref{Kind: KindURL, URL: "https://example.com/Toy.TOML"}},
		{"主机名带端口也规范", "HTTPS://Example.COM:8443/toy.toml", Ref{Kind: KindURL, URL: "https://example.com:8443/toy.toml"}},
		{"file 允许 localhost 主机名", "file://localhost/home/u/toy.toml", Ref{Kind: KindURL, URL: "file://localhost/home/u/toy.toml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q) 报错了：%v", c.in, err)
			}
			c.want.Raw = c.in
			if got != c.want {
				t.Fatalf("Parse(%q)\n得到 %+v\n期望 %+v", c.in, got, c.want)
			}
			// String() 的契约是「可以再被 Parse 的规范形式」，所以每一种被接受的
			// 形态都要能原地往返，不能只抽查几条。
			again, err := Parse(got.String())
			if err != nil {
				t.Fatalf("Parse(%q).String() = %q 又解析不了了：%v", c.in, got.String(), err)
			}
			again.Raw = got.Raw
			if again != got {
				t.Fatalf("Parse(%q) 往返不一致：\n第一次 %+v\n再解析 %+v", c.in, got, again)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空", ""},
		{"只有空白", "   "},
		{"没有斜杠", "tinyserve"},
		{"两个斜杠", "a/b/c"},
		{"结尾斜杠", "a/b/"},
		{"空的 owner", "/b"},
		{"空的 repo", "a/"},
		{"空段", "a//b"},
		{"owner 是点点", "../b"},
		{"repo 是点点", "a/.."},
		{"owner 是点", "./b"},
		{"repo 是点", "a/."},
		{"repo 里有反斜杠", `a/b\c`},
		{"owner 里有反斜杠", `a\b/c`},
		{"repo 里有编码过的斜杠不算白名单字符", "a/b%2Fc"},
		{"名字里有空格", "a b/c"},
		{"名字里有冒号", "a/b:c"},
		{"非 ASCII", "作者/包"},
		{"owner 太长", strings.Repeat("a", 101) + "/b"},
		{"repo 太长", "a/" + strings.Repeat("b", 101)},
		{"Windows 盘符不是 host 前缀", "C:foo"},
		{"Windows 盘符带反斜杠", `C:\Users\x\toy.toml`},
		{"Windows 盘符带斜杠", "C:/Users/x/toy.toml"},
		{"单字母 host", "g:a/b"},
		{"host 是空的", ":a/b"},
		{"host 以数字开头", "9git:a/b"},
		{"host 后面没有 owner/repo", "github:tinyserve"},
		{"host 后面是空的", "github:"},
		{"版本号是空的", "a/b@"},
		{"版本号是点点", "a/b@.."},
		{"版本号里有斜杠", "a/b@v1/2"},
		{"版本号里有第二个 @", "a/b@v1@v2"},
		{"版本号太长", "a/b@" + strings.Repeat("1", 101)},
		{"不认的 scheme", "ftp://example.com/toy.toml"},
		{"git scheme", "git://example.com/x.git"},
		{"ssh scheme", "ssh://git@example.com/a/b"},
		{"https 缺主机名", "https:///toy.toml"},
		{"file 缺路径", "file://toy.toml"},
		// file:// 后面的主机名会被读的人整个忽略（只用 u.Path），所以一个长得像远程
		// 地址的 ref 实际读的是本机文件 —— 那是最不该悄悄发生的一种「说一套做一套」。
		{"file 带主机名", "file://evil.com/etc/passwd"},
		{"file 带主机名大写", "FILE://EVIL.COM/etc/passwd"},
		{"只有端口没有主机名", "https://:8443/toy.toml"},
		{"只有 userinfo 没有主机名", "https://user@/toy.toml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err == nil {
				t.Fatalf("Parse(%q) 本该被拒绝，却返回了 %+v", c.in, got)
			}
			if !errors.Is(err, ErrBadRef) {
				t.Fatalf("Parse(%q) 的错误没有包住 ErrBadRef：%v", c.in, err)
			}
			// 出错时必须是干净的零值：忘了检查 error 的调用方不能拿到一个
			// 半填好的 Ref（Host 对了 Owner 是空的那种），那会变成一个静默的坏请求。
			if got != (Ref{}) {
				t.Fatalf("Parse(%q) 报错的同时还返回了半填好的 Ref：%+v", c.in, got)
			}
		})
	}
}

func TestID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Derrity/tinyserve", "Derrity/tinyserve"},
		{"github:Derrity/tinyserve@v1.2.3", "Derrity/tinyserve"},
		{"https://example.com/toy.toml", ""}, // 裸 URL 没有索引 key
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q)：%v", c.in, err)
		}
		if got := r.ID(); got != c.want {
			t.Errorf("Parse(%q).ID() = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Derrity/tinyserve", "Derrity/tinyserve"},
		{"Derrity/tinyserve@v1.2.3", "Derrity/tinyserve@v1.2.3"},
		{"github:Derrity/tinyserve", "github:Derrity/tinyserve"},
		{"github:Derrity/tinyserve@v1.2.3", "github:Derrity/tinyserve@v1.2.3"},
		{"GitHub:Derrity/tinyserve", "github:Derrity/tinyserve"},
		{"  a/b  ", "a/b"},
		{"https://example.com/toy.toml", "https://example.com/toy.toml"},
		{"file:///home/u/toy.toml", "file:///home/u/toy.toml"},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q)：%v", c.in, err)
		}
		if got := r.String(); got != c.want {
			t.Fatalf("Parse(%q).String() = %q，期望 %q", c.in, got, c.want)
		}
		// 规范形式必须还能再被 Parse，而且得到同一个 Ref（Raw 除外）。
		again, err := Parse(r.String())
		if err != nil {
			t.Fatalf("Parse(%q).String() = %q 又解析不了了：%v", c.in, r.String(), err)
		}
		again.Raw, r.Raw = "", ""
		if again != r {
			t.Fatalf("往返不一致：%+v vs %+v", again, r)
		}
	}
}

// 错误文本要说人话：至少得把出问题的那个输入念出来，不然用户不知道是哪一条。
func TestErrorMentionsInput(t *testing.T) {
	for _, in := range []string{"a/b/c", "C:foo", "ftp://x/y", "a/b@"} {
		_, err := Parse(in)
		if err == nil {
			t.Fatalf("Parse(%q) 本该报错", in)
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf("Parse(%q) 的错误里没提到输入本身：%v", in, err)
		}
	}
}
