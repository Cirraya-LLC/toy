package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 原本的指控：Tags 只拒绝以 '-' 开头的 clone 地址（那道检查是防
// `--upload-pack=<命令>`）。但 git 还有一条 URL 形态的任意命令执行 ——
// `ext::<命令>`，git-remote-ext 会把它交给 shell 跑，而它不以 '-' 开头，
// `--` 分隔符和那道前缀检查都拦不住。
//
// 为什么实际上不成立，两条各自独立：
//
//  1. git 自己从 2.12 起把 ext 传输默认关掉了（protocol.ext.allow 默认 never），
//     所以真跑起来是 "fatal: transport 'ext' not allowed"。
//  2. 更要紧的是**这条路根本走不到**：cloneURL 的前缀来自 host 表（内置的四条，
//     或者用户自己写进 config.toml 的），owner/repo 由 internal/ref 限成
//     [A-Za-z0-9._-]（连冒号都不允许）。包作者能控制的是 toy.toml，而 toy.toml
//     里没有任何东西能碰到 clone 地址。
//
// 所以这里不加代码（用户自己的 gitconfig 是用户自己的事，那不是包的攻击面）。
// 留着这个测试当回归防线：哪天 clone 地址变成可以由别处拼进来的东西，
// 或者有人给 Tags 加上 `-c protocol.ext.allow=always` 之类的开关，它会红。
func TestExtTransportDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "PWNED")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// git-remote-ext 会 sh -c 这一串
	_, err := Tags(ctx, "ext::sh -c \": > "+marker+"\"")
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatalf("host.Tags 通过 ext:: 传输协议执行了任意命令（%s 出现了），err=%v", marker, err)
	}
	if err == nil {
		t.Fatal("想要报错：ext:: 不是一个能用来发现版本的 clone 地址")
	}
	t.Logf("被拒了（err=%v）", err)
}
