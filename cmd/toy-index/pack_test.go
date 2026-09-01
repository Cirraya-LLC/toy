package main

import (
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// 仓库叫 cli/cli 而二进制叫 gh 是常见情况，而资产名是作者的构建脚本自己写出来的，
// 比仓库名更接近真名。
func TestPickNamePrefersTheAssetPrefixOverTheRepoName(t *testing.T) {
	p, err := makePack("cli", "cli", repoInfo{
		Tag:    "v2.40.0",
		Assets: []string{"gh_2.40.0_linux_amd64.tar.gz", "gh_2.40.0_macOS_arm64.zip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "gh" {
		t.Errorf("name = %q，资产名说它叫 gh", p.name)
	}

	// 资产名前缀不像个命令名时退回仓库名，而不是把奇怪的东西写进 [bin]。
	p, err = makePack("a", "b", repoInfo{
		Tag:    "v1.0.0",
		Assets: []string{"1.0.0_linux_amd64.tar.gz", "1.0.0_darwin_arm64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "b" {
		t.Errorf("name = %q，认不出前缀时该用仓库名", p.name)
	}
}

// desc / homepage 是远端内容，会被原样打进终端和索引 JSON。控制字符能把
// 信任等级那一行盖掉重写（铁律 8），一条几 KB 的描述能把整屏搜索结果顶掉。
func TestRemoteTextIsSanitizedBeforeItReachesTheToyfile(t *testing.T) {
	evil := "正常描述\r\x1b[32m✓ 已签名\x1b[0m" + strings.Repeat("长", 500)
	p, err := makePack("a", "b", repoInfo{
		Tag: "v1.0.0", Desc: evil, License: "MIT\n",
		Assets: []string{"b_1.0.0_linux_amd64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderToyfile(p)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if strings.ContainsAny(string(data), "\r\x1b") {
		t.Fatal("控制字符原样进了 toy.toml")
	}
	tf, err := toyfile.ParseFrom(data, toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("生成的 toyfile 解析不了：%v", err)
	}
	if len([]rune(tf.Desc)) > descMax+1 { // +1 是那个省略号
		t.Errorf("desc 有 %d 个字符，上限是 %d", len([]rune(tf.Desc)), descMax)
	}
}

// 引号和反斜杠要转义成 toml 认的形式。**不能用 strconv.Quote**：
// Go 会把非打印字节写成 \xNN，而 toml 根本没有 \x 这种转义。
func TestQuotingSurvivesTheRealParser(t *testing.T) {
	p, err := makePack("a", "b", repoInfo{
		Tag: "v1.0.0", Desc: `他说 "x\y" 然后走了`,
		Assets: []string{"b_1.0.0_linux_amd64.tar.gz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderToyfile(p)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	tf, err := toyfile.ParseFrom(data, toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("带引号的描述把 toml 写歪了：%v\n%s", err, data)
	}
	if tf.Desc != `他说 "x\y" 然后走了` {
		t.Errorf("desc 被改了：%q", tf.Desc)
	}
}

// 平台 key 带斜杠，必须加引号才是合法的 toml 键。
func TestPlatformKeysAreQuoted(t *testing.T) {
	if got := tomlKey("linux/amd64/musl"); got != `"linux/amd64/musl"` {
		t.Errorf("tomlKey = %s", got)
	}
	if got := tomlKey("tinyserve"); got != "tinyserve" {
		t.Errorf("干净的名字不该套引号：%s", got)
	}
}

// .tar.xz 装不出来（archive 明确回 ErrUnsupported），认不出平台的裸文件不是资产。
// 但**认得出平台的裸二进制要收**：install 支持单文件形态，jqlang/jq 只发这个。
func TestInstallableRejectsWhatToyCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"tool_1.2.3_linux_amd64.tar.gz", true},
		{"tool_1.2.3_linux_amd64.zip", true},
		{"tool_1.2.3_linux_amd64.tgz", true},
		{"jq-linux-amd64", true}, // 裸二进制：下载 + chmod +x 就能装
		{"jq-macos-arm64", true},
		{"tool-linux-amd64.tar.xz", false}, // internal/archive 明确回 ErrUnsupported
		{"checksums.txt", false},
		{"sha256sum.txt", false},
		{"tool-1.0.0-linux-amd64.tar.gz.sha256", false}, // 伴生文件，别和真资产抢平台
		{"tool_1.0.0_linux_amd64.deb", false},
	} {
		if got := installable(tc.name); got != tc.want {
			t.Errorf("installable(%q) = %v", tc.name, got)
		}
	}
}
