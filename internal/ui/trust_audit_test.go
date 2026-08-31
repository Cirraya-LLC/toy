package ui

import (
	"fmt"
	"strings"
	"testing"
)

// safe 的注释说 isCtrl「覆盖 C0、DEL 和 C1（0x80-0x9f）」，而铁律 8 说信任
// 等级那一行不能被覆写。实际上 C1 只有以合法 UTF-8（0xC2 0x9B）出现时才被
// 转义；裸字节 0x9b 原样穿过去，因为快路径的 strings.IndexFunc 把非法 UTF-8
// 解成 U+FFFD（不是控制字符），于是整个字符串被当成干净的直接返回。
//
// 0x9b 就是 8 位的 CSI。在按 8 位 C1 解释的终端上（Linux 虚拟控制台、开了
// 8-bit controls 的 xterm、部分 screen/tmux 配置），CSI 1A + CSI 2K =
// 光标上移一行 + 擦掉整行，正好够把上一行的信任等级重写掉。
const auditFakeTrust = "✓ 已签名 · Derrity (key a3f9c1d2)"

// rawCSI 是裸字节形式；utf8CSI 是同一个码位的合法 UTF-8 形式。
const rawCSI = "\x9b"

var utf8CSI = string(rune(0x9b))

func auditPayload(csi string) string {
	return "404 " + csi + "1A" + csi + "2K" + csi + "32m" + auditFakeTrust
}

func TestSafeLetsRawC1BytesThroughUntouched(t *testing.T) {
	payload := auditPayload(rawCSI) // 只有裸 C1，没有别的控制字符，走快路径
	if got := safe(payload); got == payload {
		t.Errorf("safe 原样放行了裸 C1：% x", got)
	}
	// 对照：同一个码位写成合法 UTF-8 就被正确转义了。
	if got := safe(auditPayload(utf8CSI)); !strings.Contains(got, `\x9b`) {
		t.Errorf("U+009B 这条路应该转义成 \\x9b，得到 % x", got)
	}
}

// 同一个字节，结果取决于字符串里别的地方有没有控制字符：有就走慢路径
// （0x9b 被 WriteRune 换成 U+FFFD），没有就走快路径原样放行。
// 一个看运气的消毒函数不叫消毒函数。
func TestSafeIsInconsistentAboutTheSameByte(t *testing.T) {
	// 用字节比较，不要用 ContainsRune：strings.IndexRune 把 utf8.RuneError
	// 当成"匹配任何非法字节"，拿它判会得到假阴性。
	fast := safe("x" + rawCSI + "y")
	slow := safe("x" + rawCSI + "y\r")
	if strings.Contains(fast, rawCSI) == strings.Contains(slow, rawCSI) {
		t.Skip("行为已经一致了")
	}
	t.Errorf("同一个 0x9b 两种下场：fast=% x slow=% x", fast, slow)
}

// 端到端：fetch 的 "HTTP %s" 抄的就是服务器发来的状态行原文（reason phrase
// 是任意字节，Go 只保证不含 \n）。默认配置没有镜像，errors.Join 只有一条子
// 错误也就不插 \n —— 于是整条错误信息里一个合法的控制字符都没有，快路径放行，
// 裸 CSI 直达终端。`toy update` 的循环会在包 A 的信任等级那一行之后再打包 B
// 的 ui.Error，所以这是一条用 B 的失败改写 A 的信任等级的路。
func TestServerStatusLineReachesTheTerminalUnescaped(t *testing.T) {
	err := fmt.Errorf("fetch: 原始地址和所有镜像都拿不到：%w",
		fmt.Errorf("https://example.com/x.tar.gz：%w",
			fmt.Errorf("HTTP %s", auditPayload(rawCSI))))

	got := capture(t, func() { Error("%v", err) })
	if strings.Contains(got, rawCSI) {
		t.Errorf("裸 CSI 原样进了终端：% x", got)
	}
}
