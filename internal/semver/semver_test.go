package semver

import (
	"math"
	"strings"
	"testing"
)

func TestParseOK(t *testing.T) {
	cases := []struct {
		in   string
		want V
	}{
		{"1.2.3", V{1, 2, 3, "", "1.2.3"}},
		{"v1.2.3", V{1, 2, 3, "", "v1.2.3"}},
		{"v0.0.0", V{0, 0, 0, "", "v0.0.0"}},
		{"v10.20.30", V{10, 20, 30, "", "v10.20.30"}},
		{"1.2.3-rc.1", V{1, 2, 3, "rc.1", "1.2.3-rc.1"}},
		{"v2.0.0-alpha", V{2, 0, 0, "alpha", "v2.0.0-alpha"}},
		{"v1.0.0-rc-1", V{1, 0, 0, "rc-1", "v1.0.0-rc-1"}},
		// 构建元数据不参与比较，解析时丢掉，但 Raw 保留原样
		{"v1.2.3+build.5", V{1, 2, 3, "", "v1.2.3+build.5"}},
		{"v1.2.3-rc.1+build.5", V{1, 2, 3, "rc.1", "v1.2.3-rc.1+build.5"}},
		// 前导零不合 semver 严格定义，但真实仓库里有，照收
		{"v1.02.3", V{1, 2, 3, "", "v1.02.3"}},
		{"  v1.2.3  ", V{1, 2, 3, "", "v1.2.3"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) 报错了：%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %+v，想要 %+v", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"release-2024", // 规格点名的那种非 semver tag
		"latest",
		"v",
		"1.2",
		"1",
		"1.2.3.4",
		"v1.2.x",
		"v1..3",
		"1.-2.3",
		"v1.2.-3",
		"1.0.0-",      // 预发布标签是空的
		"1.0.0-rc..1", // 中间有空段
		"1.0.0-rc.",   // 结尾的点
		"1.0.0-rc!",   // 非法字符
		"v 1.2.3",
		"99999999999999999999.0.0", // 溢出
		// 构建元数据同样是点分标识符，不能因为「不参与比较」就不校验 ——
		// 它照样进 Raw / Number()，也就照样进目录名和下载 URL。
		"v1.2.3+a/b",         // git 真的允许推这个 tag，实测过
		"1.2.3-rc.1+foo/bar", // 预发布 + 带斜杠的构建元数据
		"v1.2.3+",            // 空的构建元数据
		"v1.2.3+a..b",        // 中间有空段
		"v1.2.3+a b",
		"v1.2.3+++",
		strings.Repeat("1", 200) + ".0.0",    // 超长（也超 int）
		"v1.2.3-" + strings.Repeat("a", 200), // 超长但每段都合法
	}
	for _, s := range bad {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) 应该报错，却拿到了 %+v", s, v)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0}, // v 前缀不影响比较
		{"2.0.0", "1.9.9", 1},
		{"1.2.0", "1.10.0", -1},
		{"1.2.10", "1.2.9", 1},
		{"0.0.1", "0.0.0", 1},
		// 预发布版排在同号正式版之前
		{"1.0.0-rc", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc", 1},
		{"1.0.0-rc", "0.9.9", 1},
		{"1.0.0-rc.1", "1.0.0-rc.1", 0},
		// 纯数字段按数值比，不是按字符串比
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1}, // 数字段低于含字母的段
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},      // 段数多的更大
	}
	for _, c := range cases {
		a, err := Parse(c.a)
		if err != nil {
			t.Fatalf("Parse(%q)：%v", c.a, err)
		}
		b, err := Parse(c.b)
		if err != nil {
			t.Fatalf("Parse(%q)：%v", c.b, err)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，想要 %d", c.a, c.b, got, c.want)
		}
		if got := Compare(b, a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d，反过来应该是 %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestMax(t *testing.T) {
	cases := []struct {
		name     string
		tags     []string
		allowPre bool
		want     string // 期望的 Raw
	}{
		{"普通挑最高", []string{"v1.0.0", "v1.2.0", "v0.9.9"}, false, "v1.2.0"},
		{"最高的不在最后", []string{"v0.1.0", "v2.0.0", "v1.5.0"}, false, "v2.0.0"},
		{"跳过非 semver 的 tag", []string{"release-2024", "latest", "v1.0.0", "nightly"}, false, "v1.0.0"},
		{"默认忽略预发布", []string{"v1.0.0", "v2.0.0-rc.1"}, false, "v1.0.0"},
		{"allowPre 才拿 rc", []string{"v1.0.0", "v2.0.0-rc.1"}, true, "v2.0.0-rc.1"},
		{"同号正式版赢过 rc", []string{"v1.0.0-rc.1", "v1.0.0"}, true, "v1.0.0"},
		{"混着 v 前缀", []string{"1.0.0", "v1.1.0"}, false, "v1.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Max(c.tags, c.allowPre)
			if err != nil {
				t.Fatalf("Max 报错了：%v", err)
			}
			if got.Raw != c.want {
				t.Fatalf("Max = %q，想要 %q", got.Raw, c.want)
			}
		})
	}
}

func TestMaxErrors(t *testing.T) {
	bad := []struct {
		name     string
		tags     []string
		allowPre bool
	}{
		{"空列表", nil, false},
		{"全是非 semver", []string{"release-2024", "latest", "main"}, false},
		{"只有预发布且不允许", []string{"v1.0.0-rc.1", "v1.0.0-rc.2"}, false},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if v, err := Max(c.tags, c.allowPre); err == nil {
				t.Fatalf("应该报错，却拿到了 %+v", v)
			}
		})
	}
}

// 非 semver 的 tag 必须是「跳过」，绝不能被当成 0.0.0 —— 否则它会赢过真的低版本。
func TestMaxNeverTreatsJunkAsZero(t *testing.T) {
	got, err := Max([]string{"release-2024", "v0.0.1"}, false)
	if err != nil {
		t.Fatalf("Max 报错了：%v", err)
	}
	if got.Raw != "v0.0.1" {
		t.Fatalf("Max = %q，想要 v0.0.1", got.Raw)
	}
}

func TestNumber(t *testing.T) {
	cases := []struct {
		in   V
		want string
	}{
		{mustParse(t, "v1.2.3"), "1.2.3"},
		{mustParse(t, "1.2.3"), "1.2.3"},
		{mustParse(t, "v1.2.3-rc.1"), "1.2.3-rc.1"},
		{mustParse(t, "v1.2.3+build.5"), "1.2.3+build.5"},
		{V{Major: 1, Minor: 2, Patch: 3}, "1.2.3"},                   // 手工构造、没有 Raw
		{V{Major: 1, Minor: 0, Patch: 0, Pre: "rc.1"}, "1.0.0-rc.1"}, // 同上，带预发布
	}
	for _, c := range cases {
		if got := c.in.Number(); got != c.want {
			t.Errorf("%+v.Number() = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func mustParse(t *testing.T, s string) V {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q)：%v", s, err)
	}
	return v
}

// Parse 放行的东西会原样变成 ~/.toy/pkg 下的目录名（规格 §6）和 release 下载
// URL 的一部分（§2.4，经 Number() → toyfile.Expand）。所以「解析通过」必须蕴含
// 「拿去当路径分量和 URL 分量是安全的」。
//
// 写成 fuzz 而不是一张表：表只能挡住我想得到的坏字符，而这条不变量要挡的是
// 以后有人放宽语法时想不到的那些。不加 -fuzz 时它就跑下面这些种子。
func FuzzParseNeverYieldsUnsafeVersion(f *testing.F) {
	for _, seed := range []string{
		"v1.2.3", "1.2.3-rc.1", "v1.2.3+build.5", "v1.2.3-rc.1+build.5",
		"v1.2.3+a/b", "v1.2.3+../x", "v1.2.3+\n", "release-2024", "v1.2.3+",
		"", "v", "1.2.3.4", "v1.02.3",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := Parse(s)
		if err != nil {
			return // 拒了就没有不变量要守
		}
		for what, got := range map[string]string{"Raw": v.Raw, "Number()": v.Number()} {
			if got == "" {
				t.Fatalf("Parse(%q) 通过了，但 %s 是空的 —— 空字符串当不了目录名", s, what)
			}
			if len(got) > maxLen {
				t.Errorf("Parse(%q) 通过了，但 %s 有 %d 个字符，超过 %d", s, what, len(got), maxLen)
			}
			if strings.Contains(got, "..") {
				t.Errorf("Parse(%q) 通过了，但 %s = %q 里有 \"..\"（路径穿越）", s, what, got)
			}
			for _, c := range got {
				ok := c == '.' || c == '-' || c == '+' ||
					(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
				if !ok {
					t.Errorf("Parse(%q) 通过了，但 %s = %q 里有 %q —— 它要当目录名和 URL 分量用", s, what, got, c)
				}
			}
		}
	})
}

// V 的字段是导出的，所以「都非负」不是任何东西保证的事。相减实现的 Compare
// 在这里会溢出并把大小关系判反。
func TestCompareDoesNotOverflowOnHandBuiltV(t *testing.T) {
	lo, hi := V{Major: math.MinInt}, V{Major: math.MaxInt}
	if got := Compare(lo, hi); got != -1 {
		t.Errorf("Compare(极小, 极大) = %d，想要 -1", got)
	}
	if got := Compare(hi, lo); got != 1 {
		t.Errorf("Compare(极大, 极小) = %d，想要 1", got)
	}
}

// 「一个 semver tag 都没有」和「只有预发布版」是用户要做的两件不同的事：
// 前者只能去催上游打 tag，后者自己加个 --version 就装上了。合并成一句
// 笼统的报错会让第二种人无路可走（规格 §3「报错并明确告诉作者该怎么办」）。
func TestMaxErrorsTellYouWhichSituationYouAreIn(t *testing.T) {
	_, preOnly := Max([]string{"v1.0.0-rc.1", "v1.0.0-rc.2"}, false)
	if preOnly == nil {
		t.Fatal("只有预发布版时应该报错")
	}
	if !strings.Contains(preOnly.Error(), "--version") {
		t.Errorf("只有预发布版时应该告诉用户用 --version，实际是：%v", preOnly)
	}

	_, noSemver := Max([]string{"release-2024", "latest"}, false)
	if noSemver == nil {
		t.Fatal("全是非 semver 时应该报错")
	}
	if strings.Contains(noSemver.Error(), "--version") {
		t.Errorf("一个 semver tag 都没有时不该建议 --version（指定哪个都装不上）：%v", noSemver)
	}
}
