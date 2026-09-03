// Command toy installs the small tools nobody packaged for you, and never runs a
// line of their code while doing it.
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

// issueURL is where a user reports an index entry that has gone stale. It lives
// here rather than in a doc because the one moment it is useful is the moment an
// install fails, and by then nobody is reading docs.
const issueURL = "https://github.com/Cirraya-LLC/toy/issues"

// Version is injected with -ldflags.
var Version = "dev"

type command struct {
	name    string
	usage   string
	summary string
	run     func(ctx context.Context, args []string) error
}

// errUsage lets a subcommand ask for its own usage line without formatting it itself.
var errUsage = errors.New("usage")

// In these usage strings the options ALWAYS come BEFORE the positional arguments:
// Go's flag package stops parsing at the first non-flag argument, so the --force in
// `toy install foo --force` becomes a second positional argument (and then a usage
// error). These lines are the only syntax reference a user ever sees — writing
// `<ref> [--force]` here would be teaching an order that does not work.
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
			// Subcommands that take no arguments (list / doctor / keygen) have
			// an empty usage string, and concatenating it leaves a trailing space.
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
