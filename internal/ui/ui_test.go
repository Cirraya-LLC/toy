package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// plainOutput 关掉颜色，好逐字断言。
func plainOutput(t *testing.T) { t.Helper(); t.Setenv("NO_COLOR", "1") }

// clearNoColor 真的把 NO_COLOR 删掉。t.Setenv 只能设值不能删，
// 但它已经登记好了还原钩子，所以先 Setenv 再 Unsetenv 是安全的。
func clearNoColor(t *testing.T) {
	t.Helper()
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
}

// capture 把本包的输出接到一个 buffer 上，跑完还回去。
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := out
	var buf bytes.Buffer
	out = &buf
	defer func() { out = old }()
	fn()
	return buf.String()
}

// sink 是 fetch.Sink 的形状。这里重抄一遍而不是 import internal/fetch，
// 是因为依赖方向必须朝下（fetch 不认识 ui），反过来连测试里也别建。
type sink interface {
	Start(name string, total int64)
	Add(n int64)
	Done(err error)
}

var _ sink = (*Progress)(nil)

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "size unknown"},
		{-1, "size unknown"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1.0 KB"},
		{12_300_000, "12.3 MB"},
		{1_500_000_000, "1.5 GB"},
		{2_000_000_000_000, "2.0 TB"},
		{5_000_000_000_000_000, "5000.0 TB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestNoColor(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     bool
		val     string
		tty     bool
		want    bool
		because string
	}{
		{"TTY 且没设 NO_COLOR", false, "", true, false, "该上色"},
		{"非 TTY", false, "", false, true, "管道里不上色"},
		{"NO_COLOR 空串也算存在", true, "", true, true, "只看存不存在"},
		{"NO_COLOR=0 一样算数", true, "0", true, true, "只看存不存在，不看值"},
		{"NO_COLOR=1", true, "1", true, true, ""},
		{"NO_COLOR 且非 TTY", true, "1", false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("NO_COLOR", c.val)
			} else {
				t.Setenv("NO_COLOR", "")
				// t.Setenv 没法删环境变量，用 os.Unsetenv 配合它的自动还原。
				clearNoColor(t)
			}
			if got := noColor(c.tty); got != c.want {
				t.Errorf("noColor(tty=%v) = %v，想要 %v %s", c.tty, got, c.want, c.because)
			}
		})
	}
}

func TestTrust(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name   string
		level  string
		detail string
		want   string
	}{
		{"签名", "signed", "key a3f9… · registered in index default", "✓ signed · key a3f9… · registered in index default"},
		{"托管方", "host", "github.com/Derrity", "· from github.com/Derrity · unsigned"},
		{"裸 URL", "unverified", "", "⚠ unsigned · first install, sha256 pinned (trust on first use)"},
		{"裸 URL 带补充", "unverified", "abc1234", "⚠ unsigned · first install, sha256 pinned (trust on first use) · abc1234"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, func() { Trust(c.level, c.detail) })
			if got != c.want+"\n" {
				t.Errorf("Trust(%q,%q) = %q，想要 %q", c.level, c.detail, got, c.want+"\n")
			}
		})
	}
}

// 铁律 8：不许静默成功。认不出的等级必须喊出来，不能长得像 host 那条。
func TestTrustUnknownLevelIsLoud(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("brand-new", "x") })
	if !strings.Contains(got, "⚠") || !strings.Contains(got, "brand-new") {
		t.Errorf("认不出的信任等级要显眼并点名，得到 %q", got)
	}
}

func TestTrustUnverifiedStandsOut(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("unverified", "") })
	if !strings.HasPrefix(got, "⚠") {
		t.Errorf("未签名那条要以 ⚠ 开头，得到 %q", got)
	}
}

func TestMessages(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name string
		fn   func()
		want string
	}{
		{"Info", func() { Info("installed %s", "tinyserve") }, "installed tinyserve\n"},
		{"Warn", func() { Warn("checksum changed") }, "warning: checksum changed\n"},
		{"Error", func() { Error("cannot fetch %s", "toy.toml") }, "error: cannot fetch toy.toml\n"},
		{"Step", func() { Step("resolving %s", "github:a/b") }, "→ resolving github:a/b\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := capture(t, c.fn); got != c.want {
				t.Errorf("%s = %q，想要 %q", c.name, got, c.want)
			}
		})
	}
}

// 规格 §9 的硬要求：非 TTY 下一个 \r 都不许有，否则 CI 日志全是垃圾。
func TestProgressNonTTYNeverWritesCR(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name  string
		total int64
		adds  []int64
		err   error
		want  []string
	}{
		{
			name:  "整轮下载按 25% 打点",
			total: 12_300_000,
			adds:  []int64{3_075_000, 3_075_000, 3_075_000, 3_075_000},
			want: []string{
				"downloading tinyserve (12.3 MB)\n",
				"  tinyserve 25% (3.1 MB / 12.3 MB)\n",
				"  tinyserve 50% (6.2 MB / 12.3 MB)\n",
				"  tinyserve 75% (9.2 MB / 12.3 MB)\n",
				"  tinyserve 100% (12.3 MB / 12.3 MB)\n",
				"✓ tinyserve done (12.3 MB)\n",
			},
		},
		{
			name:  "碎块很多也只打四行",
			total: 100,
			adds:  bytesOnce(100),
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 25% (25 B / 100 B)\n",
				"  tinyserve 50% (50 B / 100 B)\n",
				"  tinyserve 75% (75 B / 100 B)\n",
				"  tinyserve 100% (100 B / 100 B)\n",
				"✓ tinyserve done (100 B)\n",
			},
		},
		{
			// 一次读完就只报最高那一档：补出来的 "25% (100 B / 100 B)" 里
			// 百分比和字节数说的是同一个时刻，两边对不上，读起来像出了错。
			name:  "一口气到底只报最高那一档",
			total: 100,
			adds:  []int64{100},
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 100% (100 B / 100 B)\n",
				"✓ tinyserve done (100 B)\n",
			},
		},
		{
			name:  "总大小未知就没有百分比",
			total: 0,
			adds:  []int64{10, 20},
			want: []string{
				"downloading tinyserve (size unknown)\n",
				"✓ tinyserve done (30 B)\n",
			},
		},
		{
			name:  "一个字节都没有就别编大小",
			total: 0,
			want: []string{
				"downloading tinyserve (size unknown)\n",
				"✓ tinyserve done\n",
			},
		},
		{
			name:  "失败要说清楚是谁失败了",
			total: 100,
			adds:  []int64{50},
			err:   errors.New("sha256 mismatch"),
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 50% (50 B / 100 B)\n",
				"✗ tinyserve failed: sha256 mismatch\n",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, func() {
				p := newProgress(false)
				p.Start("tinyserve", c.total)
				for _, n := range c.adds {
					p.Add(n)
				}
				p.Done(c.err)
			})
			if strings.Contains(got, "\r") {
				t.Fatalf("非 TTY 下出现了 \\r：%q", got)
			}
			if want := strings.Join(c.want, ""); got != want {
				t.Errorf("输出 = %q，想要 %q", got, want)
			}
		})
	}
}

// TTY 下才用 progressbar，它会用 \r 原地重画 —— 这正是非 TTY 要避开的东西。
func TestProgressTTYUsesTheBar(t *testing.T) {
	got := capture(t, func() {
		p := newProgress(true)
		p.Start("tinyserve", 100)
		p.Add(50)
		p.Add(50)
		p.Done(nil)
	})
	if !strings.Contains(got, "\r") {
		t.Errorf("TTY 下应该走进度条（有 \\r），得到 %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("TTY 下也要有结束那一行，得到 %q", got)
	}
}

// 没 Start 直接 Add/Done 不许 panic —— 出错路径上很容易走成这样。
func TestProgressWithoutStart(t *testing.T) {
	for _, tty := range []bool{false, true} {
		capture(t, func() {
			p := newProgress(tty)
			p.Add(10)
			p.Done(errors.New("died before it started"))
		})
	}
}

// 复用同一个 Progress 装第二个包时，档位和计数必须归零。
func TestProgressRestarts(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() {
		p := newProgress(false)
		p.Start("a", 100)
		p.Add(100)
		p.Done(nil)
		p.Start("b", 100)
		p.Add(25)
		p.Done(nil)
	})
	if !strings.Contains(got, "  b 25% (25 B / 100 B)\n") {
		t.Errorf("第二次下载的档位没归零：%q", got)
	}
}

func TestIsTTYDoesNotPanic(t *testing.T) {
	_ = IsTTY() // go test 下 stderr 是管道，结果不该被断言死，只保证能问
}

// bytesOnce 把 n 拆成 n 个 1 字节的块，模拟很碎的读。
func bytesOnce(n int) []int64 {
	s := make([]int64, n)
	for i := range s {
		s[i] = 1
	}
	return s
}

// 规格 §9：进度条和状态一律走 stderr，stdout 留给机器可读的输出（--json）。
// 把 out 的初值改成 os.Stdout 是一个字符的事，而上面每一个测试都会照旧通过。
func TestOutputDefaultsToStderr(t *testing.T) {
	if out != os.Stderr {
		t.Errorf("out = %v，规格 §9 要求 stderr", out)
	}
}

// 铁律 8：客户端打出来的这几行字是整个项目唯一的安全边界。
// 进到这个包的字符串几乎全是外来的（toy.toml 的 name/desc、ref、URL、
// tar 条目名、服务器发来的状态行），一句 "\r\x1b[2K\x1b[32m✓ 已签名" 就能把
// 刚打出来的信任那一行盖掉重写。所以：任何出口都不许放控制字符出去。
//
// 这个测试是那条不变量的看门人 —— 新加一个输出函数忘了过 safe，就在这里红。
func TestNoControlCharactersReachTheTerminal(t *testing.T) {
	plainOutput(t)
	// 末尾那个裸 0x9b 是 8 位的 CSI：它不是合法 UTF-8，range 会把它解成
	// U+FFFD（不是控制符），所以下面的检查必须逐字节看，按 rune 看是假阴性。
	const evil = "tinyserve\r\x1b[2K\x1b[32m✓ signed · Trusted Author\x1b[0m\x07\nforged second line\x9b1A"

	for _, c := range []struct {
		name  string
		fn    func()
		lines int // 这次调用本该出几行 —— 多出来的就是被塞进去的
	}{
		{"Info", func() { Info("%s", evil) }, 1},
		{"Warn", func() { Warn("%s", evil) }, 1},
		{"Error", func() { Error("%v", errors.New(evil)) }, 1},
		{"Step", func() { Step("%s", evil) }, 1},
		{"Trust signed", func() { Trust("signed", evil) }, 1},
		{"Trust host", func() { Trust("host", evil) }, 1},
		{"Trust unverified", func() { Trust("unverified", evil) }, 1},
		{"Trust 未知等级", func() { Trust(evil, evil) }, 1},
		{"Progress 非 TTY", func() {
			p := newProgress(false)
			p.Start(evil, 100)
			p.Add(100)
			p.Done(nil)
		}, 3}, // 开始 + 一个档位（一次读完只报最高那档）+ 完成
		{"Progress 非 TTY 失败", func() {
			p := newProgress(false)
			p.Start(evil, 100)
			p.Done(errors.New(evil))
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, c.fn)
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			// 行数对不上 = 载荷里的换行变成了真的换行，多出来一行伪造的。
			if len(lines) != c.lines {
				t.Fatalf("应该出 %d 行，得到 %d 行：%q", c.lines, len(lines), got)
			}
			for _, ln := range lines {
				// 先看合法性：裸 C1（0x9b = 8 位 CSI）不是合法 UTF-8，
				// range 会把它解成 U+FFFD 而漏掉，所以按 rune 查是假阴性。
				if !utf8.ValidString(ln) {
					t.Fatalf("有非法 UTF-8 字节直达终端（8 位 C1 就是这么进来的）：% x", ln)
				}
				for i, r := range ln {
					if isCtrl(r) {
						t.Fatalf("第 %d 个字符是控制符 %q，能用来伪造信任那一行：%q", i, r, got)
					}
				}
			}
			// 抹掉不算数：得看得出来那里原本有东西。
			if !strings.Contains(got, `\x1b`) {
				t.Errorf("控制字符要转义成看得见的样子，不是删掉：%q", got)
			}
		})
	}
}

func TestSafeLeavesCleanTextAlone(t *testing.T) {
	for _, s := range []string{"", "tinyserve", "github.com/Derrity", "✓ signed · a3f9…", "100%"} {
		if got := safe(s); got != s {
			t.Errorf("safe(%q) = %q，干净的文本不该被动", s, got)
		}
	}
}

// paint 上色那条分支在 go test 下永远走不到（stderr 是管道 → NoColor 恒真），
// 所以直接测纯函数版本。否则把 ANSI 常量改成空串也没人发现。
func TestPaintIf(t *testing.T) {
	if got := paintIf(true, ansiRed, "x"); got != "x" {
		t.Errorf("NoColor 时应原样返回，得到 %q", got)
	}
	got := paintIf(false, ansiRed, "x")
	if !strings.HasPrefix(got, ansiRed) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("上色时要有前缀和 reset，得到 %q", got)
	}
	if !strings.Contains(got, "x") {
		t.Errorf("上色不能把内容弄丢，得到 %q", got)
	}
}

// 服务器不给 Content-Length（chunked）时 fetch 传进来的是 -1 或 0。
// 非 TTY 那条路会说"大小未知"，TTY 这条路不能反而一个字都不画。
func TestProgressTTYDrawsEvenWithoutTotal(t *testing.T) {
	for _, total := range []int64{-1, 0} {
		// 只捕获 Start+Add 那一段。把 Done 也算进来的话，它那句"✓ … 完成"
		// 会替一个从头到尾什么都没画的进度条把测试糊过去。
		p := newProgress(true)
		during := capture(t, func() {
			p.Start("tinyserve", total)
			p.Add(10)
			p.Add(10)
		})
		if during == "" {
			t.Errorf("total=%d：下载过程中进度条一个字都没画", total)
		}
		if after := capture(t, func() { p.Done(nil) }); !strings.Contains(after, "done") {
			t.Errorf("total=%d：没有结束那一行：%q", total, after)
		}
	}
}

// 铁律 8：说不清就别说得比知道的多。"✓ 已签名 · " 后面空着，
// 等于替一把不知道是谁的钥匙背书。
func TestTrustSignedWithoutSignerIsNotAGreenCheck(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("signed", "") })
	if strings.HasPrefix(got, "✓") {
		t.Errorf("没有签名者却打了对勾：%q", got)
	}
	if !strings.Contains(got, "⚠") {
		t.Errorf("这条要显眼：%q", got)
	}
}

func TestTrustHostWithoutDetailSaysUnknown(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("host", "") })
	if !strings.Contains(got, "unknown") {
		t.Errorf("来源为空要说未知，不能留一个空格糊过去：%q", got)
	}
}

// 用户可以写 https://u:hunter2@example.com/toy.toml，fetch 要拿原样的
// URL 去认证，但终端会被录屏、会被贴进工单、CI 日志全世界都看得到。
func TestPrintedURLsLoseTheirPassword(t *testing.T) {
	cases := []struct {
		in   string
		want string // 输出里必须出现的那一段
	}{
		{"https://alice:hunter2@example.com/toy.toml", "https://alice:xxxxx@example.com/toy.toml"},
		{"http://u:p@127.0.0.1:8080/x.tar.gz", "http://u:xxxxx@127.0.0.1:8080/x.tar.gz"},
	}
	for _, c := range cases {
		for name, print := range map[string]func(string){
			"Info":  func(s string) { Info("fetching %s", s) },
			"Error": func(s string) { Error("cannot fetch %s: HTTP 404", s) },
			"Step":  func(s string) { Step("%s", s) },
		} {
			got := capture(t, func() { print(c.in) })
			if strings.Contains(got, "hunter2") || strings.Contains(got, ":p@") {
				t.Errorf("%s 把密码打出来了：%q", name, got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("%s 输出 %q，里面应该有 %q", name, got, c.want)
			}
		}
	}
}

// 错误信息是真正会把凭据带出来的那条路：fetch 把原样的地址写进 "%s：%w"，
// 最后走 ui.Error("%v", err)。所以抹掉必须发生在这一层，而不是指望
// 每个调用方都记得先 SafeURL 一下。
func TestErrorTextWithAURLBuriedInItIsRedacted(t *testing.T) {
	err := errors.New("fetch: original URL and every mirror failed: " +
		"https://bob:s3cret@example.com/a.tar.gz: HTTP 403 Forbidden")
	got := capture(t, func() { Error("%v", err) })
	if strings.Contains(got, "s3cret") {
		t.Fatalf("密码从错误信息里漏出来了：%q", got)
	}
	if !strings.Contains(got, "https://bob:xxxxx@example.com/a.tar.gz") {
		t.Fatalf("地址被改坏了，用户对不上自己写的东西：%q", got)
	}
	// 后面那半句必须还在：正则不能把中文冒号之后的内容一起吃掉。
	if !strings.Contains(got, "HTTP 403 Forbidden") {
		t.Fatalf("正则吃掉了 URL 后面的话：%q", got)
	}
}

// 没有凭据的地址一个字都不许改 —— 改了用户就对不上自己敲的命令。
func TestSafeURLLeavesInnocentTextAlone(t *testing.T) {
	for _, s := range []string{
		"https://example.com/toy.toml",
		"https://alice@example.com/toy.toml",
		"file:///tmp/toy.toml",
		"github:Derrity/tinyserve",
		"installed tinyserve 1.2.3",
		"",
	} {
		if got := SafeURL(s); got != s {
			t.Errorf("SafeURL(%q) = %q，应该原样返回", s, got)
		}
		if got := capture(t, func() { Info("%s", s) }); got != s+"\n" {
			t.Errorf("Info(%q) 打出了 %q", s, got)
		}
	}
}

// SafeURL 也要守住铁律 8 那条：URL 里塞控制字符能把信任等级那一行盖掉重写。
func TestSafeURLStillEscapesControlChars(t *testing.T) {
	got := SafeURL("https://example.com/a\r\x1b[2K✓ signed")
	if strings.ContainsAny(got, "\r\x1b") {
		t.Fatalf("控制字符漏出去了：%q", got)
	}
}
