// Package platform 决定「当前这台机器该拿哪个构建」。
//
// 规格 §3 说 [artifact] 的 key 是 GOOS/GOARCH，但那两维不足以描述真实世界：
//
//   - linux/amd64 一个字都没说 libc。glibc 构建丢进 Alpine，报的是
//     `sh: ./tool: not found` —— 动态加载器找不到 ld-linux 时的报错，
//     而文件明明就在那儿，是史上最误导人的错误之一。
//   - Apple Silicon 能靠 Rosetta 2 跑 darwin/amd64，主动拒绝它等于
//     拒绝一个装得上的包。
//   - GOARCH=arm 分不出 armv6（Pi Zero / Pi 1）和 armv7（Pi 2/3），
//     v7 的二进制丢到 Zero 上是非法指令。
//
// 所以匹配从「一行精确比较」改成「按优先级试一串候选」，最具体的在前。
// 探测逻辑全在 candidatesFor 这个纯函数里，Candidates 只负责读文件系统。
package platform

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Candidates 返回按优先级排的平台 key 候选，最具体的在前。会读文件系统做探测。
func Candidates() []string {
	return candidatesFor(runtime.GOOS, runtime.GOARCH,
		detectMusl(runtime.GOOS, runtime.GOARCH), detectARM(runtime.GOARCH))
}

// Current 返回给人看的当前平台描述，doctor 用。
func Current() string { return describe(Candidates()) }

// Base 返回这台机器两段的 GOOS/GOARCH —— 也就是作者会写进 [artifact] 的那个 key。
// 报「当前平台是什么」「该请作者补哪个 key」时用它，不要用 Candidates()[0]：
// 后者在 linux 上是我们自己合成的 linux/amd64/gnu，没有人会去发布一个叫 gnu 的资产。
func Base() string { return runtime.GOOS + "/" + runtime.GOARCH }

// candidatesFor 是整个包的规格，纯函数，测试全打这里。
//
//	Alpine x86-64      linux/amd64/musl → linux/amd64
//	glibc x86-64       linux/amd64/gnu  → linux/amd64 → linux/amd64/musl
//	Apple Silicon      darwin/arm64     → darwin/amd64   （Rosetta 2，静态规则不探测）
//	Intel Mac          darwin/amd64
//	树莓派 3（armv7）   linux/armv7      → linux/arm
//	树莓派 Zero（armv6）linux/armv6      → linux/arm
//
// arm 为 0 表示没探到代次 —— 那就只给 linux/arm，不要猜：猜错的代价是
// 用户拿到一个跑起来就非法指令的二进制。
func candidatesFor(goos, goarch string, musl bool, arm int) []string {
	base := goos + "/" + goarch
	switch {
	case goos == "linux" && goarch == "arm":
		// arm >= 7 一律当 armv7：armv8 的 32 位用户态跑 armv7 二进制没问题，
		// 而没有人会去发布一个叫 linux/armv8 的资产。
		if arm >= 7 {
			return []string{"linux/armv7", base}
		}
		if arm == 6 {
			return []string{"linux/armv6", base}
		}
		return []string{base}
	case goos == "linux":
		if musl {
			// 反方向【不加】：musl 机器兜底到 glibc 构建是真的跑不起来，
			// 而报错是 `not found`（找不到动态加载器），文件明明就在那儿。
			return []string{base + "/musl", base}
		}
		// glibc 机器最后再兜底到 musl 那一档：静态链接的 musl 二进制在 glibc 机器上
		// 跑得好好的（这正是大家发 musl 版的原因），而 ripgrep / zoxide 这类项目的
		// x86-64 就只发 musl 一份 —— 不兜底等于把它们整块挡在门外。
		return []string{base + "/gnu", base, base + "/musl"}
	case goos == "darwin" && goarch == "arm64":
		return []string{base, "darwin/amd64"}
	}
	return []string{base}
}

// describe 把候选串成一句人话。它只读候选，所以不存在「描述和真正会去试的
// 东西对不上」这种漂移 —— 那是两条独立探测路径必然会长出来的 bug。
func describe(cands []string) string {
	switch len(cands) {
	case 0:
		return "未知平台"
	case 1:
		return cands[0]
	default:
		return cands[0] + "（没有的话依次退回 " + strings.Join(cands[1:], "、") + "）"
	}
}

// muslLoader 是 musl 动态加载器的文件名里那段机器名 —— 和 GOARCH 不是一回事
// （amd64 在文件名里叫 x86_64）。查不到的架构一律当 gnu：探测不出来的时候
// 退回两段 key 仍然装得上，而猜错 musl 会让用户下到一个根本起不来的二进制。
var muslLoader = map[string]string{
	"amd64":   "x86_64",
	"arm64":   "aarch64",
	"386":     "i386",
	"arm":     "armhf",
	"riscv64": "riscv64",
	"s390x":   "s390x",
	"ppc64le": "powerpc64le",
}

func detectMusl(goos, goarch string) bool {
	if goos != "linux" {
		return false
	}
	machine, ok := muslLoader[goarch]
	if !ok {
		return false
	}
	_, err := os.Stat("/lib/ld-musl-" + machine + ".so.1")
	return err == nil
}

func detectARM(goarch string) int {
	if goarch != "arm" {
		return 0
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return armFromCPUInfo(string(b))
}

// armFromCPUInfo 找 "CPU architecture: 7" 那一行。读不到返回 0（= 不知道）。
func armFromCPUInfo(cpuinfo string) int {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "CPU architecture" {
			continue
		}
		// 老内核会写 "AArch64" 这种非数字的值，那就是不知道。
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return n
		}
	}
	return 0
}

// CPULevelHint 粗略猜一下 x86-64 微架构等级，**只给 doctor 显示，绝不参与匹配**。
//
// 真正正确的判定要解析 CPUID，标准库不给，而引一个 CPUID 库违反依赖策略（规格 §10）——
// 所以这里明确不做微架构匹配。但撞上 `Fatal glibc error: CPU does not support
// x86-64-v2` 的人至少该有条线索：那句报错里没有任何东西指向「你的 CPU 太老」。
// 非 Linux（拿不到 /proc/cpuinfo）返回空串。
func CPULevelHint() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	return cpuLevelFromFlags(string(b))
}

// cpuLevelFromFlags 只认 x86 的 "flags" 行 —— arm 的 /proc/cpuinfo 写的是
// "Features"，所以这个函数在非 x86 上自然返回空串，不用额外判断架构。
func cpuLevelFromFlags(cpuinfo string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		has := map[string]bool{}
		for _, f := range strings.Fields(val) {
			has[f] = true
		}
		switch {
		case has["avx512f"]:
			return "x86-64-v4"
		case has["avx2"]:
			return "x86-64-v3"
		case has["sse4_2"] && has["popcnt"]:
			return "x86-64-v2"
		default:
			return "x86-64-v1"
		}
	}
	return ""
}
