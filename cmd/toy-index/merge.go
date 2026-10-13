package main

// merge: move the entries that really installed into the index, and write the pull
// request text a person reviews them from.
//
// The discovery workflow generates a batch into a scratch index, runs index-check on it
// on every platform it has, and calls this with each run's --results file. An entry is
// kept when it passed somewhere and failed nowhere; "skip" (no build for that platform)
// is neither. Entries already in the index are never touched from here.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// commonCommands are names that come with an ordinary system or toolchain. A package
// claiming one is refused on machines that have it (the PATH shadow check) and takes it
// over on machines that do not, so the pull request puts it in front of a person. This
// is a review aid, not enforcement: the hard block is store.reservedLinkNames.
var commonCommands = map[string]bool{
	"awk": true, "bash": true, "cat": true, "cc": true, "cp": true, "curl": true, "cut": true,
	"date": true, "diff": true, "dig": true, "docker": true, "env": true, "find": true,
	"gcc": true, "git": true, "go": true, "grep": true, "gzip": true, "head": true,
	"java": true, "kill": true, "kubectl": true, "less": true, "ln": true, "ls": true,
	"make": true, "man": true, "mkdir": true, "more": true, "mv": true, "nano": true,
	"nc": true, "node": true, "npm": true, "openssl": true, "patch": true, "ping": true,
	"ps": true, "python": true, "python3": true, "rm": true, "scp": true, "sed": true,
	"sh": true, "sort": true, "ssh": true, "su": true, "sudo": true, "tail": true,
	"tar": true, "tee": true, "test": true, "time": true, "top": true, "touch": true,
	"tr": true, "uniq": true, "unzip": true, "vi": true, "vim": true, "wget": true,
	"which": true, "xargs": true, "zip": true, "zsh": true,
}

// genericNames say nothing about what the tool is; on PATH they collide with whatever
// else picked the same word.
var genericNames = map[string]bool{
	"app": true, "bin": true, "cli": true, "main": true, "run": true, "server": true,
	"tool": true, "util": true, "client": true, "agent": true, "example": true,
}

type verdict struct {
	status string // pass, fail, skip
	detail string
}

func runMerge(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "")
	into := fs.String("into", filepath.Join("index", "toy-index.json"), "")
	results := fs.String("results", "", "")
	report := fs.String("report", "", "")
	review := fs.String("review", "", "") // discover --review: repos whose licence a person has to read
	runURL := fs.String("run-url", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *from == "" || *results == "" {
		return errUsage
	}
	var toRead []string
	if *review != "" {
		var err error
		if toRead, err = readRepoList(*review); err != nil {
			return err
		}
	}
	return merge(*from, *into, strings.Split(*results, ","), *report, prExtras{toRead: toRead, runURL: *runURL})
}

// prExtras is what the pull request says beyond the entries themselves.
type prExtras struct {
	toRead []string // licences a person has to read before these can be listed
	runURL string   // the workflow run whose index-check logs back every entry
}

func merge(fromPath, intoPath string, resultPaths []string, reportPath string, extras prExtras) error {
	from, err := loadIndex(fromPath)
	if err != nil {
		return err
	}
	into, err := loadIndex(intoPath)
	if err != nil {
		return err
	}
	runs := make([]map[string]verdict, 0, len(resultPaths))
	for _, p := range resultPaths {
		v, err := readResults(strings.TrimSpace(p))
		if err != nil {
			return err
		}
		runs = append(runs, v)
	}

	taken := commandOwners(into)
	var added []index.Entry
	dropped := map[string]string{}
	for _, e := range from.Packages {
		if _, ok := into.Find(e.ID); ok {
			dropped[e.ID] = "already in the index"
			continue
		}
		passed, why := 0, ""
		for i, run := range runs {
			switch v, ok := run[foldRepo(e.ID)]; {
			case !ok:
				why = fmt.Sprintf("not checked in run %d", i+1)
			case v.status == "fail":
				why = v.detail
			case v.status == "pass":
				passed++
			}
			if why != "" {
				break
			}
		}
		if why == "" && passed == 0 {
			why = "no platform could install it"
		}
		if why != "" {
			dropped[e.ID] = why
			continue
		}
		into.Upsert(e)
		added = append(added, e)
	}
	if len(added) > 0 {
		if err := writeIndex(intoPath, into); err != nil {
			return err
		}
	}
	ui.Info("merged %d entries into %s; dropped %d", len(added), intoPath, len(dropped))
	if reportPath != "" {
		return os.WriteFile(reportPath, []byte(prReport(added, dropped, runs, taken, extras)), 0o644)
	}
	return nil
}

// readResults reads one index-check --results file: id<TAB>status<TAB>detail.
func readResults(path string) (map[string]verdict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read index-check results %s: %w", path, err)
	}
	defer f.Close()
	out := map[string]verdict{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) < 2 {
			continue
		}
		v := verdict{status: parts[1]}
		if len(parts) == 3 {
			v.detail = parts[2]
		}
		switch v.status {
		case "pass", "fail", "skip":
		default:
			return nil, fmt.Errorf("%s: unknown status %q", path, v.status)
		}
		// A later line for the same entry wins: that is the order index-check ran in.
		out[foldRepo(parts[0])] = v
	}
	return out, sc.Err()
}

// commandOwners maps each command name the index already puts on PATH to its entry.
// Only inline entries can be read here; a pointer entry's names live upstream.
func commandOwners(idx *index.Index) map[string]string {
	owners := map[string]string{}
	for _, e := range idx.Packages {
		for _, n := range entryCommands(e) {
			owners[n] = e.ID
		}
	}
	return owners
}

func entryCommands(e index.Entry) []string {
	if e.Toyfile == "" {
		return nil
	}
	tf, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex)
	if err != nil {
		return nil
	}
	var names []string
	for n := range tf.Bin {
		names = append(names, n)
	}
	for n := range tf.Script {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func entryLicence(e index.Entry) string {
	if e.Toyfile == "" {
		return "its own toy.toml"
	}
	if tf, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex); err == nil && tf.License != "" {
		return tf.License
	}
	return "?"
}

// prReport is the pull request body. Everything in it that came from upstream (desc,
// failure text) goes through mdCell, so a description cannot break the table or smuggle
// markup into the one page a reviewer reads.
func prReport(added []index.Entry, dropped map[string]string, runs []map[string]verdict, taken map[string]string, extras prExtras) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generated by the weekly index discovery: %d new entries, each installed, run and "+
		"uninstalled by index-check on every platform this workflow has, with a licence that allows "+
		"redistribution.\n\n", len(added))
	if extras.runURL != "" {
		// The checks on the pull request itself wait for a maintainer's approval (it is
		// opened by github-actions[bot]); the ones that already back it are in this run.
		fmt.Fprintf(&b, "The index-check logs for every entry are in [the run that opened this](%s).\n\n", extras.runURL)
	}

	type flag struct{ name, id, why string }
	var flags []flag
	fmt.Fprintf(&b, "| Repository | Stars | Licence | Kind | Command | Description |\n|---|---:|---|---|---|---|\n")
	for _, e := range added {
		cmds := entryCommands(e)
		kind := "inline"
		if e.Toyfile == "" {
			kind = "pointer"
			// A pointer entry's names come from what index-check actually linked.
			for _, run := range runs {
				if v := run[foldRepo(e.ID)]; v.status == "pass" && v.detail != "" {
					cmds = strings.Fields(v.detail)
					break
				}
			}
		}
		for _, c := range cmds {
			switch {
			case taken[c] != "":
				flags = append(flags, flag{c, e.ID, "already the command of " + taken[c]})
			case commonCommands[c]:
				flags = append(flags, flag{c, e.ID, "a command most systems already have"})
			case genericNames[c]:
				flags = append(flags, flag{c, e.ID, "a generic word, likely to collide"})
			case len([]rune(c)) <= 2:
				flags = append(flags, flag{c, e.ID, "two characters or less"})
			}
		}
		fmt.Fprintf(&b, "| [%s](https://github.com/%s) | %d | %s | %s | `%s` | %s |\n",
			mdCell(e.ID), e.ID, e.Stars, mdCell(entryLicence(e)), kind, mdCell(strings.Join(cmds, " ")), mdCell(e.Desc))
	}

	b.WriteString("\n### Command names to look at\n\n")
	if len(flags) == 0 {
		b.WriteString("None flagged. Still read the Command column above: a name that is not on " +
			"a machine yet can be claimed by whichever package gets there first.\n")
	} else {
		b.WriteString("Drop the entry from this branch (and add it to `index/declined.txt`) if the name is wrong:\n\n")
		for _, f := range flags {
			fmt.Fprintf(&b, "- `%s` from %s: %s\n", mdCell(f.name), mdCell(f.id), f.why)
		}
	}

	if len(dropped) > 0 {
		ids := make([]string, 0, len(dropped))
		for id := range dropped {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(&b, "\n<details><summary>%d generated but left out</summary>\n\n", len(ids))
		for _, id := range ids {
			fmt.Fprintf(&b, "- %s: %s\n", mdCell(id), mdCell(dropped[id]))
		}
		b.WriteString("\n</details>\n")
	}
	if len(extras.toRead) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>%d popular repositories whose licence GitHub cannot identify</summary>\n\n"+
			"Read each one's licence file; if it allows redistribution, add `owner/repo SPDX-id` to "+
			"`index/licenses.txt` and the next run picks it up.\n\n", len(extras.toRead))
		for _, id := range extras.toRead {
			if _, err := listID(id); err == nil {
				fmt.Fprintf(&b, "- [%s](https://github.com/%s)\n", mdCell(id), id)
			}
		}
		b.WriteString("\n</details>\n")
	}
	b.WriteString("\nTo keep a repository out for good, add it to `index/declined.txt` (not a fit) or " +
		"`index/delisted.txt` (its maintainer asked).\n")
	return b.String()
}

// mdCell makes remote text safe inside a markdown table cell.
func mdCell(s string) string {
	s = strings.Join(strings.Fields(tomlSafe(s, 160)), " ")
	r := strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;", "`", "'", "[", "\\[", "]", "\\]")
	return r.Replace(s)
}
