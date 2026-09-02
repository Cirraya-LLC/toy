// Command toy 装那些没人给你打包的小工具，装的时候一行脚本都不跑。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
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

// 用法串里选项一律写在位置参数【前面】：flag 包在第一个非 flag 参数处就停止解析，
// 所以 `toy install foo --force` 里的 --force 会被当成第二个位置参数（然后 usage）。
// 这几行是用户唯一会看到的语法说明，写成 `<ref> [--force]` 就是在教一个跑不通的顺序。
func commands() []command {
	return []command{
		{"init", "[--dist ./dist] [--name x] [--version v] [--force] [dir]", "write a toy.toml from the asset names in dist", runInit},
		{"install", "[--reinstall] [--force] [--version v] <ref>", "install a package", runInstall},
		{"uninstall", "<name>", "remove an installed package", runUninstall},
		{"update", "[name]", "upgrade installed packages", runUpdate},
		{"list", "", "list what is installed", runList},
		{"search", "[--limit n] [--json] <query>", "search the indexes for a tool", runSearch},
		{"info", "<ref>", "show a package's details and trust status", runInfo},
		{"index", "add <name> <url> | rm <name> | list", "manage indexes", runIndex},
		{"keygen", "", "generate an ed25519 key pair", runKeygen},
		{"publish", "[--inline] [--source github:owner/repo] [dir]", "read toy.toml, sign it, print an index entry", runPublish},
		{"serve", "[--addr host:port]", "serve the local cache as a mirror", runServe},
		{"doctor", "", "check PATH, directory permissions and index reachability", runDoctor},
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
			// 不带参数的子命令（list / doctor / keygen）usage 是空串，
			// 直接拼会留一个尾随空格。
			fmt.Fprintln(os.Stderr, strings.TrimSpace("usage: toy "+c.name+" "+c.usage))
			os.Exit(2)
		case errors.Is(err, context.Canceled):
			ui.Warn("cancelled")
			os.Exit(130)
		default:
			ui.Error("%v", err)
			os.Exit(1)
		}
	}

	ui.Error("no such command %q — run `toy help` for the list", name)
	os.Exit(2)
}

func usage() {
	fmt.Fprintf(os.Stderr, "toy %s — install the small tools nobody packaged for you. Nothing in a package is ever executed.\n\nusage:\n  toy <command> [args]\n\ncommands:\n", Version)
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %s %s\t%s\n", c.name, c.usage, c.summary)
	}
	w.Flush()
	fmt.Fprintf(os.Stderr, "\na package reference can be written as:\n  Derrity/tinyserve          look it up in the indexes\n  github:Derrity/tinyserve   go straight to that host\n  https://…/toy.toml         a plain URL, nothing vouched for\n")
}
