// Command toy-index is the index maintainer's bulk entry generator: feed it a list of
// owner/repo lines and it produces entries you can drop straight into the index JSON,
// turning "hand-write 50 records" into "run one command, then read it over once".
//
// # Why a separate binary instead of a toy subcommand
//
// The `toy` client NEVER touches a host's REST API (spec §2): unauthenticated 60/hour
// rate limits would wall users off on day one, and GitHub / GitLab / Gitea would each
// need their own adapter. It uses `git ls-remote` + raw URL templates instead -- one
// command that behaves the same on every git host.
//
// Index generation is a different animal: it is an offline batch job WE run. Whoever
// runs it is the index maintainer, hitting the API with a token is fine, and the rate
// limit only lands on us. Making it `toy index gen` would soften that rule into "toy
// does not use the API, except for that one subcommand" -- a sentence the next person
// will treat as precedent. Split into two binaries, the rule still has a shape you can
// check mechanically: nothing under cmd/toy should ever call api.github.com, and
// `grep api.github.com cmd/toy` should always come back empty.
//
// # What it emits are inline entries (spec §5.1), which imposes two hard constraints
//
//   - NEVER write [sha256]. Inline entries follow the upstream's newest tag, while
//     [sha256] pins one version's bytes -- leave it in and the author's next release
//     hands everyone a checksum mismatch that reads exactly like a poisoning warning.
//     First install is still pinned by trust on first use.
//   - The command names on the left of [bin] need a human to read them (§14.9 rule 2):
//     reservedLinkNames blocks `git` and the shadow check blocks commands that already
//     exist, but a name that is not yet on PATH can be claimed. So the report lists
//     them all at the end.
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

// errUsage lets a subcommand ask for the usage text without formatting it itself.
var errUsage = errors.New("usage")

var usageLines = []string{
	"Usage: toy-index gen --repos <file> [--out index/toy-index.json] [--update] [--probe] [--token <token>]",
	"       toy-index discover --out <file> [--review <file>] [--topics a,b] [--min-stars 1000]",
	"                          [--pushed-within 730] [--max 50] [--index index/toy-index.json]",
	"       toy-index merge --from <generated.json> --results <tsv>[,<tsv>...] [--into index/toy-index.json]",
	"                       [--report <pr-body.md>]",
	"",
	"  --repos   Repo list, one owner/repo per line. # starts a comment (whole line or trailing); blank lines ignored",
	"  --out     Index JSON to merge into (a new empty one is created if it does not exist)",
	"  --update  Overwrite entries already in the index. Off by default: existing entries may have been",
	"            hand-corrected (especially the in-package path on the right of [bin]), and a blind",
	"            overwrite throws those corrections away",
	"  --probe   Download one linux/amd64 asset and unpack it, turning [bin] from a guess into a fact",
	"  --token   Host API token; falls back to $GITHUB_TOKEN. Without one you hit the rate limit fast",
	"",
	"  gen and discover both refuse a repo whose licence does not allow redistribution (cmd/toy-index/license.go),",
	"  and anything in index/delisted.txt. index/licenses.txt records licences a person read by hand.",
	"",
	"  discover searches GitHub by topic (most stars first) and writes the repos gen would accept to --out,",
	"  up to --max of them; --review gets the ones whose licence file GitHub cannot identify.",
	"",
	"  merge moves into --into the generated entries that index-check (--results) passed somewhere and",
	"  failed nowhere, and writes the pull request text, flagging command names a person should check.",
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
	case "discover":
		return runDiscover(ctx, args[1:])
	case "merge":
		return runMerge(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q; the subcommands are gen, discover and merge", args[0])
	}
}

func printUsage() {
	for _, l := range usageLines {
		ui.Info("%s", l)
	}
}

// readRepoList reads the repo list. One owner/repo per line, # starts a comment (whole
// line or trailing), blank lines ignored. Duplicates keep only the first occurrence --
// running the same repo twice just burns API quota for nothing.
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
