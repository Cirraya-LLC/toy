// Package ui 是终端输出。规格 §9：进度条走 stderr，非 TTY 必须自动降级。
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"
)

// out 是本包一切输出的去处。规格 §9：进度条和状态走 stderr，
// 机器可读的输出（--json）留给 stdout。做成变量只为了测试能断言。
var out io.Writer = os.Stderr

// 最朴素的 ANSI，不引 color 库。
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// IsTTY 报告 stderr 是不是终端。非 TTY 时进度条降级成一行行的日志，
// 否则 CI 日志里全是 \r 垃圾。
func IsTTY() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

// NoColor 在 NO_COLOR 环境变量存在或非 TTY 时为 true。
func NoColor() bool { return noColor(IsTTY()) }

// noColor 是 NoColor 的纯函数版本，好测。NO_COLOR 只看存不存在，
// 不看值 —— 约定就是这样，NO_COLOR=0 一样表示别上色。
func noColor(tty bool) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return !tty
}

// paint 给 s 套上 ANSI，NoColor 时原样返回。
func paint(code, s string) string { return paintIf(NoColor(), code, s) }

// paintIf 是 paint 的纯函数版本，好测。必须有：go test 下 stderr 是管道，
// NoColor() 恒为 true，上色那条分支光靠 paint 一次都走不到。
func paintIf(noColor bool, code, s string) string {
	if noColor {
		return s
	}
	return code + s + ansiReset
}

// isCtrl 覆盖 C0、DEL 和 C1（0x80-0x9f）。C1 在 UTF-8 终端里一般不当控制符，
// 但它也不会出现在任何正经的包名、URL 或错误信息里。
func isCtrl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

// safe 把外来文本里的控制字符换成看得见的 \xNN。
//
// 进到这个包的字符串几乎全是外来的：toy.toml 里的 name/desc、ref、URL、
// tar 条目名、以及 ui.Error("%v", err) 里那个 err —— fetch 的 "HTTP %s" 直接
// 抄的是服务器发来的状态行。一句 "\r\x1b[2K\x1b[32m✓ 已签名 · Trusted" 就能
// 把刚打出来的信任那一行盖掉重写，而铁律 8 说信任等级必须如实打印、客户端
// 是唯一的安全边界 —— 能被覆写的那一行就不叫边界。
//
// 换成可见的转义而不是删掉：删掉等于替攻击者把痕迹擦干净了。
// 顺带把规格 §9 那条「非 TTY 一个 \r 都不许有」真正守住 —— 原来只有我们
// 自己不写 \r，别人塞进来的照样能出去。
func safe(s string) string {
	if strings.IndexFunc(s, isCtrl) < 0 {
		return s // 绝大多数时候一次扫描就走人，不分配
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if isCtrl(r) {
			fmt.Fprintf(&b, `\x%02x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sprintf 是本包唯一的格式化入口。格式串是我们自己写的字面量，参数却常常
// 一路来自网络，所以结果一律过 safe。新加输出函数时用它，别直接 fmt.Sprintf。
func sprintf(format string, a ...any) string { return safe(fmt.Sprintf(format, a...)) }

// line 往 stderr 写一行，永远只有一个结尾换行。
func line(s string) {
	fmt.Fprintln(out, strings.TrimRight(s, "\n"))
}

// 下面这些都往 stderr 写。机器可读的输出留给 stdout。
func Info(format string, a ...any) { line(sprintf(format, a...)) }

func Warn(format string, a ...any) {
	line(paint(ansiYellow, "警告: "+sprintf(format, a...)))
}

func Error(format string, a ...any) {
	line(paint(ansiRed, "错误: "+sprintf(format, a...)))
}

func Step(format string, a ...any) {
	line(paint(ansiDim, "→ ") + sprintf(format, a...))
}

// Trust 打印安装完成那一行的信任等级（铁律 8、规格 §4）：
//
//	signed      ✓ 已签名 · Derrity (key a3f9…)
//	host        · 来源 github.com/Derrity · 未签名
//	unverified  ⚠ 未签名 · 首次安装，已钉住 sha256
//
// level 取 store.TrustSigned / TrustHost / TrustUnverified。
func Trust(level, detail string) {
	// detail 是签名者名字 / 托管方域名，都来自索引或 ref —— 外来的。
	detail = safe(detail)
	switch level {
	case "signed":
		if detail == "" {
			// 说"已签名"却说不出是谁签的，等于替一把不知道是谁的钥匙背书。
			// 铁律 8：不许静默成功，说不清就别说得比知道的多。
			line(paint(ansiBold+ansiRed, "⚠ 声称已签名但没给出签名者 · 当作未验证处理"))
			return
		}
		line(paint(ansiGreen, "✓ 已签名") + " · " + detail)
	case "host":
		if detail == "" {
			detail = "未知"
		}
		line(paint(ansiDim, "· 来源 "+detail+" · 未签名"))
	case "unverified":
		s := "⚠ 未签名 · 首次安装，已钉住 sha256"
		if detail != "" {
			s += " · " + detail
		}
		line(paint(ansiBold+ansiYellow, s))
	default:
		// 铁律 8：不许静默成功。认不出的等级要说出来，不能当成安全的那一档糊过去。
		line(paint(ansiBold+ansiRed, fmt.Sprintf("⚠ 未知的信任等级 %q，当作未验证处理 · %s", level, detail)))
	}
}

// Progress 实现 fetch.Sink。TTY 下是进度条，非 TTY 下是若干行普通日志
// （开始一行、结束一行，中间按百分比节流，绝不输出 \r）。
type Progress struct {
	tty    bool
	bar    *progressbar.ProgressBar
	name   string
	total  int64
	cur    int64
	bucket int64 // 非 TTY 下已经打印过的最高 25% 档位
}

func NewProgress() *Progress { return newProgress(IsTTY()) }

// newProgress 让测试能直接要一个 TTY 形态的实例，不用真的有终端。
func newProgress(tty bool) *Progress { return &Progress{tty: tty} }

func (p *Progress) Start(name string, total int64) {
	// name 来自 toy.toml / ref，外来的；这里消一次毒，Add 和 Done 就都干净了。
	p.name, p.total, p.cur, p.bucket = safe(name), total, 0, 0
	if p.tty {
		// total<=0 表示服务器没给 Content-Length（chunked 响应就是这样，
		// fetch.open 直接把 resp.ContentLength 传过来）。progressbar 用 -1 表示
		// "总量未知，画成转圈的"，传 0 进去它一个字都不画 —— 整次下载在终端上
		// 完全静默，而非 TTY 那条路是会老老实实说"大小未知"的。
		if total <= 0 {
			total = -1
		}
		p.bar = progressbar.NewOptions64(total,
			progressbar.OptionSetWriter(out),
			progressbar.OptionSetDescription(p.name),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionSetPredictTime(true),
			progressbar.OptionClearOnFinish(),
		)
		return
	}
	line("下载 " + p.name + " (" + humanBytes(total) + ")")
}

func (p *Progress) Add(n int64) {
	p.cur += n
	if p.tty {
		if p.bar != nil {
			_ = p.bar.Add64(n)
		}
		return
	}
	// 非 TTY：只在跨过 25/50/75/100 这几个档位时打一行，绝不输出 \r。
	// 总大小未知就没有百分比可言，那时什么都不打，结果留给 Done。
	if p.total <= 0 {
		return
	}
	for b := p.bucket + 25; b <= 100 && p.cur*100 >= p.total*b; b += 25 {
		p.bucket = b
		line(sprintf("  %s %d%% (%s / %s)", p.name, b, humanBytes(p.cur), humanBytes(p.total)))
	}
}

func (p *Progress) Done(err error) {
	if p.tty && p.bar != nil {
		_ = p.bar.Finish()
		p.bar = nil
	}
	if err != nil {
		// err 一路来自网络：fetch 的 "HTTP %s" 抄的就是服务器发来的状态行。
		line(paint(ansiRed, sprintf("✗ %s 失败: %v", p.name, err)))
		return
	}
	if p.cur <= 0 {
		// 一个字节都没数过，就别编一个大小出来。
		line(paint(ansiGreen, "✓ "+p.name+" 完成"))
		return
	}
	line(paint(ansiGreen, sprintf("✓ %s 完成 (%s)", p.name, humanBytes(p.cur))))
}

// humanBytes 把字节数写成人看的样子，例如 12.3 MB。
// 十进制单位（1000），跟 release 页面上标的大小对得上。
func humanBytes(n int64) string {
	if n <= 0 {
		return "大小未知"
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v, units := float64(n), []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		v /= 1000
		if v < 1000 || u == "TB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%d B", n) // 到不了
}
