package host

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// selfHostedHint 的输出最终经 ui.Error 打到终端，而 ui.safe 会把每一个控制字符
// 转义成看得见的 \xNN（铁律 8：信任那一行不能被别人塞进来的 \r / ESC 盖掉重写，
// 那道闸不能为了排版放开）。所以这段模板里但凡有 \n 或 \t，用户看到的就是一串
// 字面量 "\x0a\x09" —— 比不给提示还糟。这个测试就钉这一条。
func TestSelfHostedHintHasNoControlCharsToEscape(t *testing.T) {
	hint := selfHostedHint("mygitea")
	for i, r := range hint {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("selfHostedHint 第 %d 个字符是控制字符 %q —— "+
				"ui.safe 会把它转义成 \\x%02x 打给用户看。整段是：%q", i, r, r, hint)
		}
	}
}

// 光是没有控制字符还不够：那一行是让人整行复制进 config.toml 的，所以它必须
// 真的能被解析回一个四个模板都齐的 Host。
func TestSelfHostedHintPastesBackIntoAWorkingHost(t *testing.T) {
	hint := selfHostedHint("mygitea")
	line := hint[strings.LastIndex(hint, "：")+len("："):]

	var cfg struct {
		Hosts map[string]Host `toml:"hosts"`
	}
	if _, err := toml.Decode(line, &cfg); err != nil {
		t.Fatalf("提示给出的那一行不是合法 TOML：%v\n那一行是：%s", err, line)
	}
	h, ok := cfg.Hosts["mygitea"]
	if !ok {
		t.Fatalf("解析出来没有 hosts.mygitea，解出来的是 %#v", cfg.Hosts)
	}
	if h.isEmpty() {
		t.Fatalf("解析出来的 Host 是空的 —— 抄进 config.toml 会撞上「一个模板都没配」那条错误：%#v", h)
	}
	for name, tmpl := range map[string]string{
		"raw": h.Raw, "clone": h.Clone, "release": h.Release, "web": h.Web,
	} {
		if strings.TrimSpace(tmpl) == "" {
			t.Errorf("模板 %s 是空的 —— 提示里少了这个键", name)
		}
	}
}
