// Package semver 是一个够用就好的语义化版本比较器。
// 规格 §10：不引第三方库 —— 就是三个整数加可选预发布标签的比较。
package semver

// V 是一个解析过的版本。Pre 为空表示正式发布。
type V struct {
	Major, Minor, Patch int
	Pre                 string // 不含前导 '-'
	Raw                 string // 原始 tag，可能带 'v' 前缀
}

// Parse 接受 "v1.2.3" / "1.2.3" / "1.2.3-rc.1"。
// 不合法的 tag 返回 error —— 调用方应当跳过它而不是当成 0.0.0。
func Parse(s string) (V, error) { panic("todo") }

// Compare 返回 -1 / 0 / 1。预发布版本排在同号正式版之前（1.0.0-rc < 1.0.0）。
func Compare(a, b V) int { panic("todo") }

// Max 从一堆 tag 里挑出最高的正式版本。全部解析失败时返回 error。
// 除非 allowPre，否则忽略预发布版本 —— 用户装东西默认不该拿到 rc。
func Max(tags []string, allowPre bool) (V, error) { panic("todo") }

// Number 返回不带 'v' 前缀的版本号，用于 toy.toml 里的 {version} 占位符。
func (v V) Number() string { panic("todo") }
