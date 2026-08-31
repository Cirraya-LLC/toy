// Package semver 是一个够用就好的语义化版本比较器。
// 规格 §10：不引第三方库 —— 就是三个整数加可选预发布标签的比较。
package semver

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// maxLen 和 internal/ref 的 maxSegment 一致：版本号会变成 ~/.toy/pkg 下的
// 一级目录名，而单个路径分量在多数文件系统上只有 255 字节。tag 是从
// git ls-remote 来的、上游想写多长写多长，所以这道闸必须在这里。
const maxLen = 100

// V 是一个解析过的版本。Pre 为空表示正式发布。
type V struct {
	Major, Minor, Patch int
	Pre                 string // 不含前导 '-'
	Raw                 string // 原始 tag，可能带 'v' 前缀
}

// Parse 接受 "v1.2.3" / "1.2.3" / "1.2.3-rc.1"。
// 不合法的 tag 返回 error —— 调用方应当跳过它而不是当成 0.0.0。
func Parse(s string) (V, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return V{}, fmt.Errorf("版本号是空的")
	}
	if len(raw) > maxLen {
		return V{}, fmt.Errorf("版本号太长了（%d 个字符，最多 %d）", len(raw), maxLen)
	}
	body := strings.TrimPrefix(raw, "v")
	// 构建元数据（'+' 之后的部分）按 semver 不参与比较，但**必须校验字符**：
	// Raw 原样进 ~/.toy/pkg 的目录名（规格 §6）和 Number() 拼出的下载 URL（§2.4），
	// 而 "v1.2.3+a/b" 是一个 git 真的允许推上去的 tag —— 不校验就等于让上游
	// 往我们的路径和 URL 里塞斜杠。校验完再丢掉。
	if i := strings.IndexByte(body, '+'); i >= 0 {
		if err := checkIdents(body[i+1:]); err != nil {
			return V{}, fmt.Errorf("%q 的构建元数据不合法：%w", raw, err)
		}
		body = body[:i]
	}
	var pre string
	if i := strings.IndexByte(body, '-'); i >= 0 {
		// 有连字符就必须有预发布标签，所以校验放在这个分支里：
		// 写成 "1.0.0-" 时 pre 是空的，绝不能因为「空标签 = 正式版」而被放行。
		pre, body = body[i+1:], body[:i]
		if err := checkIdents(pre); err != nil {
			return V{}, fmt.Errorf("%q 的预发布标签不合法：%w", raw, err)
		}
	}

	nums := strings.Split(body, ".")
	if len(nums) != 3 {
		return V{}, fmt.Errorf("%q 不是 semver：版本号要写成 主.次.修订 三段数字，例如 v1.2.3", raw)
	}
	v := V{Pre: pre, Raw: raw}
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := parseNum(nums[i])
		if err != nil {
			return V{}, fmt.Errorf("%q 不是 semver：%w", raw, err)
		}
		*dst = n
	}
	return v, nil
}

// parseNum 只接受纯数字，所以 "-2"、"x"、"" 这些都会被挡下来。
func parseNum(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("有一段版本号是空的")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q 不是数字", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q 这个数字太大了", s)
	}
	return n, nil
}

// checkIdents 校验点分标识符：每段非空且只含字母、数字和连字符。
// 预发布标签和构建元数据在 semver 里是同一套文法，所以共用这一个校验。
func checkIdents(s string) error {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return fmt.Errorf("有一段是空的（别写连着的点或者结尾的点）")
		}
		for _, c := range id {
			ok := c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return fmt.Errorf("%q 里有非法字符 %q，只能用字母、数字和连字符", id, c)
			}
		}
	}
	return nil
}

// Compare 返回 -1 / 0 / 1。预发布版本排在同号正式版之前（1.0.0-rc < 1.0.0）。
func Compare(a, b V) int {
	// 用 cmp.Compare 而不是相减：V 的字段是导出的，手工构造出负数时
	// 相减会溢出并把大小关系判反。
	if c := cmp.Compare(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Patch, b.Patch); c != 0 {
		return c
	}
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1 // 正式版高于同号预发布版
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre 逐段比预发布标签：纯数字段按数值比（rc.2 < rc.10），
// 数字段低于含字母的段，段数多的更大（rc.1 < rc.1.1）。
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

func compareIdent(a, b string) int {
	an, bn := allDigits(a), allDigits(b)
	switch {
	case an && bn:
		return compareNumeric(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// compareNumeric 按数值比两串数字，但不做 strconv —— 再长的数字也不会溢出。
func compareNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Max 从一堆 tag 里挑出最高的正式版本。全部解析失败时返回 error。
// 除非 allowPre，否则忽略预发布版本 —— 用户装东西默认不该拿到 rc。
func Max(tags []string, allowPre bool) (V, error) {
	var (
		best       V
		found      bool
		skippedPre int
	)
	for _, t := range tags {
		v, err := Parse(t)
		if err != nil {
			continue // 仓库里常有 "release-2024" 这种非 semver 的 tag，跳过而不是当成 0.0.0
		}
		if v.Pre != "" && !allowPre {
			skippedPre++
			continue
		}
		if !found || Compare(v, best) > 0 {
			best, found = v, true
		}
	}
	if found {
		return best, nil
	}
	if skippedPre > 0 {
		return V{}, fmt.Errorf("只找到 %d 个预发布版本，没有正式版；用 --version 指定具体的 tag 才会装预发布版", skippedPre)
	}
	return V{}, fmt.Errorf("这 %d 个 tag 里没有 semver 版本（要形如 v1.2.3）；这个仓库可能还没打过版本 tag", len(tags))
}

// Number 返回不带 'v' 前缀的版本号，用于 toy.toml 里的 {version} 占位符。
func (v V) Number() string {
	// 优先照原始 tag 去掉 'v'：作者的 release 资产名就是照着 tag 起的，
	// 重新拼一遍反而可能对不上（比如带构建元数据的 tag）。
	if v.Raw != "" {
		return strings.TrimPrefix(v.Raw, "v")
	}
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
