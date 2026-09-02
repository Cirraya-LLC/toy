// Command toy-index 是索引维护者用的批量条目生成器：喂一个 owner/repo 列表，
// 产出可以直接进索引 JSON 的条目，把"手写 50 条"变成"跑一条命令 + 人工过一眼"。
//
// # 为什么是单独一个二进制，而不是 toy 的一个子命令
//
// `toy` 客户端**永远不用托管方的 REST API**（规格 §2）：未认证 60 次/小时的限流
// 会让用户第一天就撞墙，而且 GitHub / GitLab / Gitea 要写三套适配器。它只用
// `git ls-remote` + raw URL 模板，一条命令对所有 git 托管方都成立。
//
// 而索引生成是**我们自己跑的离线批处理**：跑它的人是索引维护者，带 token 调 API
// 完全没问题，限流也只砸到自己头上。把它做成 `toy index gen` 会让上面那条规矩
// 变成「toy 不用 API，除了那个子命令」—— 一句迟早被下一个人当成先例的话。
// 分成两个二进制，规矩就还是清楚的：`cmd/toy` 里一个打 api.github.com 的调用都
// 不该有，`grep api.github.com cmd/toy` 应该永远是空的。
//
// # 产出的是内联条目（规格 §5.1），所以有两条硬约束
//
//   - **绝不写 [sha256]**。内联条目跟着上游最新的 tag 走，而 [sha256] 钉的是
//     某一个版本的字节 —— 留着它，上游一发新版所有人都会拿到「哈希对不上，
//     拒绝安装」，读起来完全像投毒警告。首次安装仍有 TOFU 钉住。
//   - **[bin] 左边那些命令名要人过一眼**（§14.9 第 2 条）：`reservedLinkNames`
//     挡住了 `git`、PATH 遮蔽检查挡住了已存在的命令，但一个 PATH 上还不存在的
//     名字是能被占的。所以报告最后会把它们单独列一遍。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Cirraya-LLC/toy/internal/ui"
)

// errUsage 让子命令能要求打印用法而不必自己排版。
var errUsage = errors.New("usage")

var usageLines = []string{
	"Usage: toy-index gen --repos <file> [--out index/toy-index.json] [--update] [--probe] [--token <token>]",
	"",
	"  --repos   Repo list, one owner/repo per line. # starts a comment (whole line or trailing); blank lines ignored",
	"  --out     Index JSON to merge into (a new empty one is created if it does not exist)",
	"  --update  Overwrite entries already in the index. Off by default: existing entries may have been",
	"            hand-corrected (especially the in-package path on the right of [bin]), and a blind",
	"            overwrite throws those corrections away",
	"  --probe   Download one linux/amd64 asset and unpack it, turning [bin] from a guess into a fact",
	"  --token   Host API token; falls back to $GITHUB_TOKEN. Without one you hit the rate limit fast",
	"",
	"Flags go before positional arguments: Go's flag package stops parsing at the first non-flag argument.",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:])
	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		printUsage()
		os.Exit(2)
	default:
		ui.Error("%v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "gen":
		return runGen(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q; the only one is gen", args[0])
	}
}

func printUsage() {
	for _, l := range usageLines {
		ui.Info("%s", l)
	}
}

// readRepoList 读仓库列表。一行一个 owner/repo，# 是注释（整行或行内都算），
// 空行忽略。重复的行只留第一条 —— 同一个仓库跑两遍只是白烧两次 API 配额。
func readRepoList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read repo list %s: %w\n"+
			"The format is one owner/repo per line; # starts a comment", path, err)
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || seen[strings.ToLower(line)] {
			continue
		}
		seen[strings.ToLower(line)] = true
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no repos at all (only blank lines and comments)", path)
	}
	return out, nil
}
