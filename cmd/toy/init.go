package main

// toy init: work a toy.toml backwards out of the file names in ./dist.
//
// The first step on the author path is "hand-write the [artifact] mapping", and
// asset naming conventions are all over the place (x86_64 vs amd64, darwin vs macos
// vs apple-darwin, .tar.gz vs .zip). Get one word wrong and what somebody else sees
// on install is "no build matches this platform". This command turns that into "run
// it, then fix a word or two".
//
// Two things it deliberately does not do:
//
//   - It goes nowhere online and touches no host's API (spec §2). The dist directory
//     is goreleaser's output layout, and everything needed is already in the names.
//   - It does not run `git describe`. The version is recognized from the file names —
//     and if the names carry no version, there is nothing to replace with {version}
//     and git could not help anyway. Meanwhile "exec git by name" would drag another
//     copy of the §14.1 "skip ~/.toy/bin" defense into cmd/toy, and a second copy is
//     a second place that can silently stop working, bought for nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// iniMaxListed caps how many of the file names it saw an error message lists — a
// dist with two hundred files in it should not flood the terminal.
const iniMaxListed = 12

// iniAsset is one asset recognized in dist.
type iniAsset struct {
	file string // the file name as it is
	key  string // the [artifact] key in toy.toml
	tmpl string // the asset name with the version replaced by {version}
	sum  string // sha256
}

// ctx goes unused: this command goes nowhere online and execs nothing (see the top
// of the file).
func runInit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dist := fs.String("dist", "", "directory holding the release assets (defaults to dist/ under the project directory)")
	name := fs.String("name", "", "tool name; inferred from the directory or asset names if omitted")
	version := fs.String("version", "", "version of this batch of assets; read from the file names if omitted")
	force := fs.Bool("force", false, "overwrite an existing toy.toml")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errUsage
	}

	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	distDir := *dist
	if distDir == "" {
		distDir = filepath.Join(dir, "dist")
	}

	files, err := iniListFiles(distDir)
	if err != nil {
		return err
	}

	// The inference itself lives in internal/assetname — the bulk index generator
	// (cmd/toy-index) calls the same function. Two implementations would drift, and
	// the symptom of that drift is that an entry generated for the index differs
	// from what the author gets running toy init, with nothing anywhere saying so.
	res := assetname.Infer(files, *version)
	skipped := iniSkipLines(res.Skipped)
	if len(res.Assets) == 0 {
		return iniNothingRecognized(distDir, files, skipped)
	}
	assets := make([]iniAsset, len(res.Assets))
	for i, a := range res.Assets {
		assets[i] = iniAsset{file: a.File, key: a.Platform, tmpl: a.Template}
	}

	// The files are right here locally, so hashing them costs next to nothing, and it
	// lifts the package from "pinned by TOFU on first install" to "checksum enforced"
	// (iron law 4). Skipping this step makes a guinea pig out of everyone who
	// installs it.
	for i := range assets {
		sum, err := iniSum(filepath.Join(distDir, assets[i].file))
		if err != nil {
			return fmt.Errorf("cannot compute the sha256 of %s: %w", assets[i].file, err)
		}
		assets[i].sum = sum
	}

	pkg, guessedName := iniName(*name, dir, res.Name)
	data, err := iniRender(pkg, assets)
	if err != nil {
		return err
	}
	// Run our own output through the real parser too: odd characters in a file name
	// (quotes, square brackets, non-ASCII) can all bend the toml out of shape, and a
	// bent toy.toml is the author's next hour of puzzling.
	if _, err := toyfile.Parse(data); err != nil {
		return fmt.Errorf("the toy.toml generated from the file names in %s does not parse: %w. "+
			"Most likely a name contains characters toml cannot take — pass --name with a clean one and try again", distDir, err)
	}

	out := filepath.Join(dir, "toy.toml")
	if _, err := os.Stat(out); err == nil && !*force {
		return fmt.Errorf("%s already exists and will not be overwritten. Add --force to replace it — but check first that nothing in it was edited by hand", out)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", out, err)
	}

	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		ui.Info("%s", line)
	}
	ui.Info("")
	ui.Info("wrote %s", out)
	iniReport(pkg, assets, skipped, guessedName, res.Name)
	return nil
}

// iniListFiles lists the regular files at the top level of dist. Subdirectories are
// left out: goreleaser puts the packaged archives at the top level, while the
// subdirectories hold the unpackaged raw build output, which would duplicate them.
func iniListFiles(distDir string) ([]string, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w. toy init works backwards from the FILE NAMES of release "+
			"assets that already exist, so build them first (goreleaser writes to dist/ by default), "+
			"or point --dist somewhere else", distDir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// iniSkipLines turns the skip reasons into one human-readable line each.
func iniSkipLines(skipped []assetname.Asset) []string {
	out := make([]string, len(skipped))
	for i, a := range skipped {
		out[i] = a.File + ": " + a.Skip
	}
	return out
}

// iniName settles the tool name: --name, then the project directory name, then the
// common prefix of the asset names. The second return value means "this name was
// guessed" and the author has to check it.
func iniName(flagName, dir, prefix string) (name string, guessed bool) {
	if n := strings.TrimSpace(flagName); n != "" {
		return n, false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if base := filepath.Base(abs); base != "." && base != string(filepath.Separator) && base != "" {
			return base, true
		}
	}
	return prefix, true
}

// iniRender builds the toy.toml. Written by hand rather than through a toml encoder:
// this file exists to be edited by a person, its comments and field order are most of
// what it contains, and an encoder cannot write a single comment.
func iniRender(name string, assets []iniAsset) ([]byte, error) {
	var b strings.Builder
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
	// A bare toml key can only be [A-Za-z0-9_-]; everything else gets quoted. Platform
	// keys contain slashes and always need it. Tool names usually do not — quoting a
	// name that was already clean just leaves people thinking the quotes are required.
	key := func(s string) string {
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r == '-' || r == '_' || (r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) < 0 {
			return s
		}
		return q(s)
	}

	fmt.Fprintf(&b, "# Written by toy init from the release asset file names. Installing runs no scripts at all,\n")
	fmt.Fprintf(&b, "# which is why there is no install/build key here.\n")
	fmt.Fprintf(&b, "# Pushing a new tag is releasing a new version: {version} becomes that tag (without the v),\n")
	fmt.Fprintf(&b, "# so this file never has to change again.\n")
	fmt.Fprintf(&b, "name    = %s\n", q(name))
	fmt.Fprintf(&b, "desc    = \"\"  # <- one sentence saying what it does; this is what search results show\n")
	fmt.Fprintf(&b, "license = \"\"  # <- e.g. \"MIT\"\n")
	b.WriteString("\n[bin]\n")
	b.WriteString("# Left = the command name on PATH after installing. Right = the path inside the package\n")
	b.WriteString("# AFTER unpacking. When the whole tree sits in one wrapper directory (the cargo-dist style\n")
	b.WriteString("# " + name + "-1.2.3-x86_64-.../), that layer is stripped automatically, so leave it off the\n")
	b.WriteString("# right hand side. Write a subdirectory only if the package really has one, e.g. \"bin/" + name + "\".\n")
	b.WriteString("# Run `tar tzf dist/<one of the archives>` to see what is actually in there.\n")
	fmt.Fprintf(&b, "%s = %s\n", key(name), q(name))

	b.WriteString("\n[artifact]\n")
	for _, a := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(a.key), q(a.tmpl))
	}

	b.WriteString("\n# With a sha256 the checksum is enforced, and a mismatch always refuses the install.\n")
	b.WriteString("# toy init computed these lines just now, so they describe only this batch of files —\n")
	b.WriteString("# recompute them after every release (run toy init --force again).\n")
	b.WriteString("[sha256]\n")
	for i := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(assets[i].key), q(assets[i].sum))
	}
	if strings.ContainsAny(name, "\"\\") {
		return nil, fmt.Errorf("the tool name %q contains a quote or a backslash, which is awkward as both a toml key and a value. Pass --name with a different one", name)
	}
	return []byte(b.String()), nil
}

// iniReport states plainly what it guessed and which parts the author has to check.
// A generator that does not admit to guessing gets its guesses committed as fact.
func iniReport(name string, assets []iniAsset, skipped []string, guessedName bool, prefix string) {
	ui.Info("")
	ui.Info("Recognized %d platform(s):", len(assets))
	for _, a := range assets {
		ui.Info("    %-22s ← %s", a.key, a.file)
	}
	for _, s := range skipped {
		ui.Warn("skipped %s", s)
	}

	ui.Info("")
	ui.Info("The following were guessed — check them before you commit:")
	ui.Info("    the path on the right of [bin] — fix it if the package has subdirectories (a bin/ and so on). " +
		"A single wrapper directory around the whole tree needs no fixing; that layer is stripped when unpacking " +
		"(`tar tzf` shows you which case you have)")
	if guessedName {
		hint := ""
		// When the directory name and the asset name prefix disagree, the latter is
		// usually the real name (repository directories get renamed and cloned under
		// other names, while asset names are written by the build itself). Just hand
		// over the command.
		if p := prefix; p != "" && p != name {
			hint = "; the asset names suggest " + p + ": `toy init --force --name " + p + "`"
		}
		ui.Info("    name = %s — taken from the directory name or the asset name prefix%s", name, hint)
	}
	ui.Info("    desc / license — left empty, fill them in")
	if len(assets) > 0 && !strings.Contains(assets[0].tmpl, "{version}") {
		// A mapping without {version} has to be hand-edited on every release, which
		// is precisely what this command exists to abolish.
		ui.Warn("No version was recognized in the asset names, so there is no {version} placeholder — " +
			"which means every release makes you come back and edit this file. Pass --version with the " +
			"version of this batch and run it again")
	}
}

// iniNothingRecognized errors out when nothing at all was recognized. It NEVER writes
// an empty shell of a toy.toml — that would be a file that looks generated and
// installs for nobody.
func iniNothingRecognized(distDir string, files, skipped []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no file name in %s says which os/arch it is for, so [artifact] cannot be written", distDir)
	switch {
	case len(files) == 0:
		b.WriteString(" (there is not a single regular file in that directory)")
	default:
		fmt.Fprintf(&b, ". What it saw: %s", strings.Join(iniHead(files, iniMaxListed), ", "))
	}
	// \n MUST NOT be used to break lines here: this ends up going through ui.Error,
	// and ui escapes control characters (\n among them) into visible \xNN — that is
	// the guardrail against foreign text forging a "✓ signed" line, and it does not
	// get relaxed for the sake of layout. So this is punctuated rather than broken,
	// one long sentence like every other error in this project.
	for _, s := range skipped {
		b.WriteString("; skipped " + s)
	}
	b.WriteString(". A file name has to carry both the operating system and the architecture, e.g. " +
		"tool_1.2.3_linux_amd64.tar.gz / tool-1.2.3-darwin-arm64.zip. " +
		"Recognized spellings: linux / darwin (macos, osx) / windows; " +
		"amd64 (x86_64, x64) / arm64 (aarch64) / armv7 (armhf) / armv6 / 386 (i386); " +
		"a musl or gnu in the name adds a libc segment to the key. " +
		"If none of that fits, write [artifact] by hand — one line per platform.")
	return fmt.Errorf("%s", b.String())
}

func iniHead(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("… (%d more)", len(s)-n))
}

// iniSum computes the sha256 of one file.

func iniSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
