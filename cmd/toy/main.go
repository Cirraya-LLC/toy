// Command toy 装那些没人给你打包的小工具，装的时候一行脚本都不跑。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/Cirraya-LLC/toy/internal/ui"
)

// Version 由 -ldflags 注入。
var Version = "dev"

type command struct {
	name    string
	usage   string
	summary string
	run     func(ctx context.Context, args []string) error
}

// errUsage 让子命令能要求打印自己的用法而不必自己排版。
var errUsage = errors.New("usage")

func commands() []command {
	return []command{
		{"install", "<ref> [--force] [--version v]", "安装一个包", runInstall},
		{"uninstall", "<名字>", "卸载", runUninstall},
		{"update", "[名字]", "升级已装的包", runUpdate},
		{"list", "", "列出已装的包", runList},
		{"search", "<关键词>", "在索引里搜工具", runSearch},
		{"info", "<ref>", "看一个包的详情和信任状态", runInfo},
		{"index", "add|rm|list [参数]", "管理索引", runIndex},
		{"keygen", "", "生成 ed25519 密钥对", runKeygen},
		{"publish", "[目录]", "读 toy.toml，签名，输出索引条目", runPublish},
		{"serve", "[--addr :8080]", "把本机缓存变成一个镜像", runServe},
		{"doctor", "", "检查 PATH、目录权限、索引可达性", runDoctor},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	flag.Usage = usage
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	name := os.Args[1]
	switch name {
	case "-h", "--help", "help":
		usage()
		return
	case "-v", "--version", "version":
		fmt.Println("toy", Version)
		return
	}

	for _, c := range commands() {
		if c.name != name {
			continue
		}
		err := c.run(ctx, os.Args[2:])
		switch {
		case err == nil:
			return
		case errors.Is(err, errUsage):
			fmt.Fprintf(os.Stderr, "用法: toy %s %s\n", c.name, c.usage)
			os.Exit(2)
		case errors.Is(err, context.Canceled):
			ui.Warn("已取消")
			os.Exit(130)
		default:
			ui.Error("%v", err)
			os.Exit(1)
		}
	}

	ui.Error("不认识的命令 %q，跑 toy help 看看有哪些", name)
	os.Exit(2)
}

func usage() {
	fmt.Fprintf(os.Stderr, "toy %s —— 装那些没人给你打包的小工具，装的时候一行脚本都不跑\n\n用法:\n  toy <命令> [参数]\n\n命令:\n", Version)
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %s %s\t%s\n", c.name, c.usage, c.summary)
	}
	w.Flush()
	fmt.Fprintf(os.Stderr, "\n包引用可以写成:\n  Derrity/tinyserve          查索引\n  github:Derrity/tinyserve   直接读那个托管方\n  https://…/toy.toml         裸地址\n")
}
