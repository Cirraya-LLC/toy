// Package ui 是终端输出。规格 §9：进度条走 stderr，非 TTY 必须自动降级。
package ui

// IsTTY 报告 stderr 是不是终端。非 TTY 时进度条降级成一行行的日志，
// 否则 CI 日志里全是 \r 垃圾。
func IsTTY() bool { panic("todo") }

// Progress 实现 fetch.Sink。TTY 下是进度条，非 TTY 下是若干行普通日志
// （开始一行、结束一行，中间按百分比节流，绝不输出 \r）。
type Progress struct{ tty bool }

func NewProgress() *Progress { panic("todo") }

func (p *Progress) Start(name string, total int64) { panic("todo") }
func (p *Progress) Add(n int64)                    { panic("todo") }
func (p *Progress) Done(err error)                 { panic("todo") }

// 下面这些都往 stderr 写。机器可读的输出留给 stdout。
func Info(format string, a ...any)  { panic("todo") }
func Warn(format string, a ...any)  { panic("todo") }
func Error(format string, a ...any) { panic("todo") }
func Step(format string, a ...any)  { panic("todo") }

// Trust 打印安装完成那一行的信任等级（铁律 8、规格 §4）：
//
//	signed      ✓ 已签名 · Derrity (key a3f9…)
//	host        · 来源 github.com/Derrity · 未签名
//	unverified  ⚠ 未签名 · 首次安装，已钉住 sha256
//
// level 取 store.TrustSigned / TrustHost / TrustUnverified。
func Trust(level, detail string) { panic("todo") }

// NoColor 在 NO_COLOR 环境变量存在或非 TTY 时为 true。
func NoColor() bool { panic("todo") }
