package platform

import (
	"runtime"
	"strings"
	"testing"
)

// candidatesFor 就是这次改动的规格（包注释里那张表），所以逐行钉住。
func TestCandidatesFor(t *testing.T) {
	cases := []struct {
		name         string
		goos, goarch string
		musl         bool
		arm          int
		want         []string
	}{
		// 反方向不许有：musl 机器兜底到 glibc 构建是真的跑不起来（报 `not found`）。
		{"Alpine x86-64", "linux", "amd64", true, 0, []string{"linux/amd64/musl", "linux/amd64"}},
		// glibc 机器末位兜底到 musl：静态链接的 musl 二进制在这里照跑，而
		// ripgrep / zoxide 的 x86-64 只发 musl 一份，不兜底就整块装不上。
		{"glibc x86-64", "linux", "amd64", false, 0, []string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}},
		{"Apple Silicon 能靠 Rosetta 跑 amd64", "darwin", "arm64", false, 0, []string{"darwin/arm64", "darwin/amd64"}},
		{"Intel Mac 不回退到 arm64", "darwin", "amd64", false, 0, []string{"darwin/amd64"}},
		{"树莓派 3 是 armv7", "linux", "arm", false, 7, []string{"linux/armv7", "linux/arm"}},
		{"树莓派 Zero 是 armv6", "linux", "arm", false, 6, []string{"linux/armv6", "linux/arm"}},
		{"linux/arm64 musl", "linux", "arm64", true, 0, []string{"linux/arm64/musl", "linux/arm64"}},
		{"linux/arm64 gnu", "linux", "arm64", false, 0, []string{"linux/arm64/gnu", "linux/arm64", "linux/arm64/musl"}},
		// 探不到代次就别猜：armv7 的二进制丢到 Zero 上是非法指令。
		{"arm 代次未知只给裸 key", "linux", "arm", false, 0, []string{"linux/arm"}},
		// armv8 的 32 位用户态跑 armv7 二进制没问题，而没人发布 linux/armv8。
		{"arm >= 7 一律当 armv7", "linux", "arm", false, 8, []string{"linux/armv7", "linux/arm"}},
		// 认不出来的系统就老老实实两段，不要凭空造回退。
		{"freebsd 没有回退", "freebsd", "amd64", false, 0, []string{"freebsd/amd64"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := candidatesFor(c.goos, c.goarch, c.musl, c.arm)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("candidatesFor(%q,%q,%v,%d) = %v，想要 %v", c.goos, c.goarch, c.musl, c.arm, got, c.want)
			}
			// 最具体的必须在最前面：回退的意义就在这个顺序上。判据是「首选不是
			// 那个两段的底座」，不能拿字符串长度当"具体程度"的代理 —— glibc 机器
			// 的末位候选 linux/amd64/musl 比首选还长一个字符。
			if len(got) > 1 && c.goos == "linux" && got[0] == c.goos+"/"+c.goarch {
				t.Errorf("两段的底座不该排在最前面：%v", got)
			}
		})
	}
}

// 精确的 GOOS/GOARCH 永远得在候选里 —— 现存的每一份 toy.toml 都只写了这一种 key，
// 回退这一层不许把它们弄丢。
func TestBareKeyIsAlwaysACandidate(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		musl         bool
		arm          int
	}{
		{"linux", "amd64", true, 0}, {"linux", "amd64", false, 0},
		{"linux", "arm", false, 7}, {"linux", "arm", false, 6},
		{"darwin", "arm64", false, 0}, {"darwin", "amd64", false, 0},
	} {
		base := c.goos + "/" + c.goarch
		found := false
		for _, p := range candidatesFor(c.goos, c.goarch, c.musl, c.arm) {
			if p == base {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 不在候选里：%v", base, candidatesFor(c.goos, c.goarch, c.musl, c.arm))
		}
	}
}

func TestArmFromCPUInfo(t *testing.T) {
	pi3 := "processor\t: 0\nmodel name\t: ARMv7 Processor rev 4 (v7l)\nCPU architecture: 7\n"
	if got := armFromCPUInfo(pi3); got != 7 {
		t.Errorf("armv7 = %d", got)
	}
	zero := "processor\t: 0\nCPU architecture: 6\nFeatures\t: half thumb\n"
	if got := armFromCPUInfo(zero); got != 6 {
		t.Errorf("armv6 = %d", got)
	}
	// 读不到 / 不是数字 = 不知道，返回 0，绝不猜一个代次出来。
	for _, s := range []string{"", "flags: avx2\n", "CPU architecture: AArch64\n", "CPU architecture\n"} {
		if got := armFromCPUInfo(s); got != 0 {
			t.Errorf("%q 应当是 0，拿到 %d", s, got)
		}
	}
}

func TestCPULevelFromFlags(t *testing.T) {
	line := func(flags string) string { return "processor\t: 0\nflags\t\t: " + flags + "\n" }
	cases := map[string]string{
		line("fpu vme sse4_2 popcnt avx avx2 avx512f"): "x86-64-v4",
		line("fpu sse4_2 popcnt avx avx2"):             "x86-64-v3",
		line("fpu sse4_2 popcnt"):                      "x86-64-v2",
		line("fpu vme de pse tsc"):                     "x86-64-v1",
		// popcnt 缺一个就不是 v2 —— 这正是 x86-64-v2 那条 glibc 报错的成因。
		line("fpu sse4_2"): "x86-64-v1",
		// arm 的 /proc/cpuinfo 写的是 Features，不是 flags：不该硬猜一个等级出来。
		"processor\t: 0\nFeatures\t: half thumb neon\n": "",
		"": "",
	}
	for in, want := range cases {
		if got := cpuLevelFromFlags(in); got != want {
			t.Errorf("cpuLevelFromFlags(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 提示只给 doctor 看，绝不参与匹配：它不该长得像一个平台 key。
func TestCPULevelHintIsNeverAPlatformKey(t *testing.T) {
	for _, s := range []string{"x86-64-v1", "x86-64-v2", "x86-64-v3", "x86-64-v4", CPULevelHint()} {
		if strings.Contains(s, "/") {
			t.Errorf("CPU 等级提示 %q 含 /，会被误当成平台 key", s)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := describe([]string{"darwin/amd64"}); got != "darwin/amd64" {
		t.Errorf("单候选应当原样返回：%q", got)
	}
	got := describe([]string{"linux/amd64/musl", "linux/amd64"})
	for _, want := range []string{"linux/amd64/musl", "linux/amd64"} {
		if !strings.Contains(got, want) {
			t.Errorf("描述里没有 %q：%q", want, got)
		}
	}
	if describe(nil) == "" {
		t.Error("空候选也要给句人话")
	}
}

// Candidates/Current 会真的读文件系统，只断言不变量：非空、首项是最具体的、
// 而且当前 GOOS/GOARCH 一定在里面。
func TestCandidatesOnThisMachine(t *testing.T) {
	got := Candidates()
	if len(got) == 0 {
		t.Fatal("Candidates() 不能是空的")
	}
	base := runtime.GOOS + "/" + runtime.GOARCH
	found := false
	for _, p := range got {
		if p == base {
			found = true
		}
	}
	if !found && runtime.GOARCH != "arm" {
		t.Errorf("%s 不在 %v 里", base, got)
	}
	if !strings.Contains(Current(), got[0]) {
		t.Errorf("Current() = %q 里没有首选候选 %q", Current(), got[0])
	}
}

// musl 的兜底是单向的：glibc 机器可以退到 musl 构建（静态链接，照跑），
// musl 机器【绝不能】退到 glibc 构建 —— 那是真的起不来，而报错长得像文件不存在。
// 这一条翻转过来不会有任何别的测试变红，所以单独钉住。
func TestMuslFallbackIsOneWay(t *testing.T) {
	for _, goarch := range []string{"amd64", "arm64"} {
		base := "linux/" + goarch
		glibc := candidatesFor("linux", goarch, false, 0)
		if last := glibc[len(glibc)-1]; last != base+"/musl" {
			t.Errorf("glibc %s 的末位候选 = %q，想要 %q", goarch, last, base+"/musl")
		}
		for _, c := range candidatesFor("linux", goarch, true, 0) {
			if c == base+"/gnu" {
				t.Errorf("musl 机器不该有 %s 这个候选：%v", c, candidatesFor("linux", goarch, true, 0))
			}
		}
	}
}
