// Package host turns "git hosting provider" into a config table instead of three
// API adapters. Spec §2: never touch the GitHub/GitLab REST APIs — 60 requests
// per hour unauthenticated, and it would mean writing three of everything.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrUnknownHost = errors.New("host: unknown host")

// Host is four URL templates. Placeholders: {owner} {repo} {ref} {path} {tag} {asset}
type Host struct {
	Name    string `toml:"-"`
	Raw     string `toml:"raw"`     // fetch one file out of the repo
	Clone   string `toml:"clone"`   // what git ls-remote gets pointed at
	Release string `toml:"release"` // release asset download URL
	Web     string `toml:"web"`     // the repo page a human would open
	// Latest is the forge's "latest release" page, which redirects to the release the
	// author marked as latest. Optional. It is only ever consulted when the newest tag
	// has no release file (insPerformOrPrevious): versions are still discovered with
	// git ls-remote, and what this page names is only used if it is one of those tags.
	Latest string `toml:"latest"`
}

// placeholderOrigin is the domain used by the entries in the built-in table that
// are a URL *shape* rather than a hosting provider. RFC 2606 reserves it for
// documentation, so no real machine will ever answer there — which makes it both
// the marker and the safety net: Lookup recognizes it and refuses, so the user
// never gets a DNS error and has to guess what they typed wrong.
const placeholderOrigin = "https://gitea.example.com"

// giteaPaths is the path shape of the gitea/forgejo family; codeberg is just one
// instance of it. Swap the domain and you have another provider — that is what
// "hosts are config, not code" means.
func giteaPaths(origin string) Host {
	return Host{
		Raw:     origin + "/{owner}/{repo}/raw/tag/{ref}/{path}",
		Clone:   origin + "/{owner}/{repo}",
		Release: origin + "/{owner}/{repo}/releases/download/{tag}/{asset}",
		Web:     origin + "/{owner}/{repo}",
		Latest:  origin + "/{owner}/{repo}/releases/latest",
	}
}

// isPlaceholder reports whether a built-in entry is a shape rather than a real host.
func (h Host) isPlaceholder() bool {
	return strings.HasPrefix(h.Clone, placeholderOrigin)
}

// isEmpty means "this [hosts.x] section sets no templates at all" — usually a
// misspelled key in config.toml (clone_url instead of clone).
func (h Host) isEmpty() bool {
	return h.Raw == "" && h.Clone == "" && h.Release == "" && h.Web == ""
}

// Builtin returns the built-in host table: github / gitlab / codeberg / the
// generic gitea shape. Three lines in config.toml add another provider — hosts
// are config, not code.
//
// Note that the "gitea" entry is a **shape**, not a provider (the domain is an
// RFC 2606 reserved name): Lookup refuses it outright and hands the user the
// block below, because only they know their own instance's domain.
//
//	[hosts.mygit]
//	raw     = "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}"
//	clone   = "https://git.example.com/{owner}/{repo}"
//	release = "https://git.example.com/{owner}/{repo}/releases/download/{tag}/{asset}"
//	web     = "https://git.example.com/{owner}/{repo}"
func Builtin() map[string]Host {
	m := map[string]Host{
		"github": {
			Raw:     "https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}",
			Clone:   "https://github.com/{owner}/{repo}",
			Release: "https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			Web:     "https://github.com/{owner}/{repo}",
			Latest:  "https://github.com/{owner}/{repo}/releases/latest",
		},
		"gitlab": {
			Raw:     "https://gitlab.com/{owner}/{repo}/-/raw/{ref}/{path}",
			Clone:   "https://gitlab.com/{owner}/{repo}",
			Release: "https://gitlab.com/{owner}/{repo}/-/releases/{tag}/downloads/{asset}",
			Web:     "https://gitlab.com/{owner}/{repo}",
			Latest:  "https://gitlab.com/{owner}/{repo}/-/releases/permalink/latest",
		},
		"codeberg": giteaPaths("https://codeberg.org"),
		"gitea":    giteaPaths(placeholderOrigin),
	}
	for name, h := range m {
		h.Name = name
		m[name] = h
	}
	return m
}

// selfHostedHint is the line printed when a host is refused — one the user can
// paste straight into config.toml.
//
// It is a TOML **inline table** (one line, not a five-line [hosts.x] section)
// for a reason: this text goes out through ui.Error, and ui.safe escapes every
// control character into a visible \xNN — iron law 8 says nobody gets to overwrite
// the trust line with an injected \r or ESC, so that gate does not open for the
// sake of prettier formatting. A multi-line template full of \n\t would reach the
// user as the literal string "\x0a\x09", which is worse than no hint at all.
// An inline table is valid TOML 1.0, BurntSushi/toml accepts it, and it still
// copy-pastes as a single line.
func selfHostedHint(name string) string {
	const tmpl = `hosts.%s = { raw = "https://your.host/{owner}/{repo}/raw/tag/{ref}/{path}", ` +
		`clone = "https://your.host/{owner}/{repo}", ` +
		`release = "https://your.host/{owner}/{repo}/releases/download/{tag}/{asset}", ` +
		`web = "https://your.host/{owner}/{repo}", ` +
		`latest = "https://your.host/{owner}/{repo}/releases/latest" }`
	return "Add this line to ~/.toy/config.toml (copy the whole line): " + fmt.Sprintf(tmpl, name)
}

// Lookup checks overrides first, then the built-in table.
func Lookup(name string, overrides map[string]Host) (Host, error) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return Host{}, fmt.Errorf("%w: the host name is empty (write it as github:owner/repo)", ErrUnknownHost)
	}
	// Override keys are whatever the user named them, so try the literal spelling
	// first and fall back to lowercase.
	// Name is always stored lowercase: it becomes the directory name in
	// ~/.toy/pkg/<host>/… (spec §6), and both spellings must land in the same
	// directory or the same package gets installed twice.
	for _, key := range []string{name, lower} {
		if h, ok := overrides[key]; ok {
			if h.isEmpty() {
				return Host{}, fmt.Errorf("%w: [hosts.%s] in config.toml sets no templates at all "+
					"(the four keys are raw / clone / release / web; a misspelled key is not an error, "+
					"it just leaves the entry empty), and it shadows the built-in host of the same name. %s",
					ErrUnknownHost, key, selfHostedHint(key))
			}
			h.Name = lower
			return h, nil
		}
	}
	builtin := Builtin()
	if h, ok := builtin[lower]; ok {
		// A shape is not a provider: using it as-is aims at a domain that does not
		// exist, and all the user sees is a DNS error that tells them nothing about
		// what they were supposed to configure. Stop here and hand them the template.
		if h.isPlaceholder() {
			return Host{}, fmt.Errorf("%w: %q is the URL **shape** of gitea/forgejo, not a hosting "+
				"provider — only you know your own instance's domain. %s",
				ErrUnknownHost, name, selfHostedHint(lower))
		}
		return h, nil
	}
	// Built-ins and overrides can collide ([hosts.github] shadowing the built-in);
	// the list must name each host once.
	seen := make(map[string]bool, len(builtin)+len(overrides))
	known := make([]string, 0, len(builtin)+len(overrides))
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			known = append(known, k)
		}
	}
	for k, h := range builtin {
		if !h.isPlaceholder() { // never list something we would turn around and refuse
			add(k)
		}
	}
	for k := range overrides {
		add(strings.ToLower(k))
	}
	sort.Strings(known)
	return Host{}, fmt.Errorf(
		"%w: %q. Known hosts are %s; a self-hosted instance needs its own entry. %s",
		ErrUnknownHost, name, strings.Join(known, ", "), selfHostedHint(lower))
}

// expand substitutes the placeholders in a template. An empty template yields an
// empty string, which is how callers tell this host does not support that capability.
// NewReplacer makes a single pass, so a substituted value that itself contains
// {owner} is not expanded a second time.
func expand(tmpl string, kv ...string) string {
	if tmpl == "" {
		return ""
	}
	return strings.NewReplacer(kv...).Replace(tmpl)
}

func (h Host) RawURL(owner, repo, ref, path string) string {
	return expand(h.Raw, "{owner}", owner, "{repo}", repo, "{ref}", ref, "{path}", path)
}

func (h Host) CloneURL(owner, repo string) string {
	return expand(h.Clone, "{owner}", owner, "{repo}", repo)
}

// ReleaseURL builds the download URL for a release asset.
//
// asset is the only one of the four inputs that comes from the **package author**
// (the [artifact] value in toy.toml), and on all four providers it is a flat file
// name — so the whole thing gets path-escaped:
//   - "../../../x" gets escaped away; otherwise an author could point the download
//     at someone else's release on the same domain while the install line still
//     truthfully reads "source github.com/<themselves>" (breaking iron law 8)
//   - asset names containing spaces / ? / # no longer produce an invalid or
//     truncated URL
//
// owner/repo/tag are not escaped: internal/ref and internal/semver restrict them
// to [A-Za-z0-9._-], and slashes are legitimate in the ref/path positions
// (subdirectories, branch names containing slashes).
func (h Host) ReleaseURL(owner, repo, tag, asset string) string {
	return expand(h.Release, "{owner}", owner, "{repo}", repo, "{tag}", tag,
		"{asset}", url.PathEscape(asset))
}

func (h Host) WebURL(owner, repo string) string {
	return expand(h.Web, "{owner}", owner, "{repo}", repo)
}

func (h Host) LatestURL(owner, repo string) string {
	return expand(h.Latest, "{owner}", owner, "{repo}", repo)
}

// TagFromLatest reads the tag out of where a latest-release page redirected to: the
// last path segment on GitHub (/releases/tag/v1.2.3), Gitea and Forgejo (the same) and
// GitLab (/-/releases/v1.2.3). A repository with no release redirects to /releases,
// which yields "releases" -- callers only trust an answer that is one of the tags git
// ls-remote listed.
func TagFromLatest(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	p := strings.TrimRight(u.Path, "/")
	t, err := url.PathUnescape(p[strings.LastIndex(p, "/")+1:])
	if err != nil {
		return ""
	}
	return t
}

// —— resolving git to an absolute path ————————————————————————————
//
// The other half of iron law 1: Tags execs git **by name** off PATH, and
// ~/.toy/bin is the directory packages can write symlinks into — the very one
// doctor tells users to put first on PATH. Nothing runs at install time, true,
// but the next toy command executes that package as git. Execution was only
// deferred by one command.
// The internal/store half refuses to link a package's executable as `git`; this
// half guarantees that even if a bin/git is already sitting there, toy will not
// run it.

// toyBinDir derives ~/.toy/bin.
//
// These few lines duplicate store.Open + Store.BinDir: internal/store already
// imports internal/host, so importing back would be an import cycle. **The rules
// on both sides must stay identical, word for word** (empty root → TOY_HOME →
// ~/.toy, then made absolute, then the bin subdirectory). Change one side and
// forget the other and this exclusion fails silently — and failing looks exactly
// like working.
//
// If it cannot be derived (no TOY_HOME and no home directory) this returns an
// empty string and the caller excludes nothing.
func toyBinDir() string {
	root := os.Getenv("TOY_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		root = filepath.Join(home, ".toy")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return filepath.Join(abs, dirBinName)
}

// dirBinName holds the same value as dirBin in store, for the same reason as
// above (it cannot be imported).
const dirBinName = "bin"

// lookupGit walks PATH itself and returns git's absolute path.
//
// Not exec.LookPath: it cannot exclude a directory, and excluding ~/.toy/bin is
// the entire point here.
func lookupGit() (string, error) {
	binDir := toyBinDir()
	var inToyBin string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// Relative entries ("." and that empty entry that shows up everywhere on
		// PATH) mean the directory the toy process **happens to be in**. Running toy
		// from a directory someone else can write to must not hand away iron law 1 —
		// and a real git is never found only in some relative directory anyway.
		if !filepath.IsAbs(dir) {
			continue
		}
		cand := filepath.Join(dir, "git")
		if !executableFile(cand) {
			continue
		}
		if binDir != "" && sameDir(dir, binDir) {
			inToyBin = cand // remembered only so the error can be specific; keep looking for a real git
			continue
		}
		return cand, nil
	}
	if inToyBin != "" {
		return "", fmt.Errorf("host: refusing to run git — the only git on your PATH is %s, "+
			"which sits in toy's own bin directory, meaning it came from a package you installed. "+
			"toy discovers versions with `git ls-remote`, so running it would hand that package "+
			"code execution — **this is very likely an attack**. Run `toy list` to find which "+
			"package put it there, uninstall it, then install a real git "+
			"(Debian/Ubuntu: sudo apt install git; macOS: xcode-select --install)",
			inToyBin)
	}
	return "", errors.New("host: git not found. toy discovers versions with `git ls-remote`, " +
		"so install git first (Debian/Ubuntu: sudo apt install git; macOS: xcode-select --install)")
}

// executableFile reports whether p is a regular file that can be executed.
// Symlinks are followed (everything in bin/ is a symlink).
func executableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// sameDir / canonDir are the same thing as the identically named functions in
// store (again because it cannot be imported): make absolute, then resolve
// symlinks (/usr/bin is often a symlink to /bin), falling back to string
// comparison when that fails.
func sameDir(a, b string) bool { return canonDir(a) == canonDir(b) }

func canonDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// maxStderrLine caps the length (in bytes) of the message relayed from git to the user.
const maxStderrLine = 300

// maxStderrBytes caps the stderr buffer. git's `remote: …` lines are written by
// the **remote server**, so leaving it unbounded lets the other side decide how
// much memory we eat — and we only ever use the first line.
const maxStderrBytes = 8 << 10

// cappedBuffer keeps only the first chunk; everything after that is accepted and
// dropped on the floor.
// Write must lie and claim it wrote everything: a short write makes os/exec's
// copy goroutine fail and close the pipe early, git gets EPIPE, and the reason we
// could have read is gone.
type cappedBuffer struct{ b bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if n := maxStderrBytes - c.b.Len(); n > 0 {
		c.b.Write(p[:min(n, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.b.String() }

// Tags lists every tag name via `git ls-remote --tags <clone-url>`.
// One command, identical behaviour on every git host, no API, no token, no rate limit.
// It must carry a context deadline; the refs/tags/ prefix and the ^{} suffix
// (the dereference line of an annotated tag) are stripped.
func Tags(ctx context.Context, cloneURL string) ([]string, error) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return nil, errors.New("host: the clone URL is empty — this host has no clone template, so versions cannot be discovered")
	}
	// Iron law 1: toy never executes anyone else's code. But in
	// `git ls-remote <url>`, a url starting with '-' is read by git as an option —
	// and --upload-pack=<command> is arbitrary command execution off the shelf
	// (git really does run it, verified). We pass a `--` separator below AND refuse
	// here: a string starting with '-' is never a valid clone URL anyway.
	if strings.HasPrefix(cloneURL, "-") {
		return nil, fmt.Errorf("host: refusing to run git — the clone URL %q starts with '-', "+
			"which git reads as a command-line option (--upload-pack=<command> is arbitrary "+
			"command execution)", cloneURL)
	}
	// Absolute path, guaranteed not to come from ~/.toy/bin — see lookupGit above.
	// Not cached: one PATH walk is a handful of stats and it is followed by a network
	// round trip, so caching buys nothing while making a changed PATH impossible to
	// pick up within a process.
	git, err := lookupGit()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, git, "ls-remote", "--tags", "--refs", "--", cloneURL)
	// GIT_TERMINAL_PROMPT=0: a private repo or a typo'd name makes git sit on an
	// interactive password prompt forever, and at that point the context deadline can
	// only kill git itself — what the user sees is an inexplicable hang.
	// The two ASKPASS vars stop it from popping up a GUI password dialog instead.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	// Do not run git in the current directory: git reads the .git/config of
	// **whatever repo the current directory belongs to**, and url.<base>.insteadOf
	// and credential.helper there can both make git execute something else. Running
	// toy from a directory someone else can write to must not hand away iron law 1.
	// "/" rather than os.TempDir() — on a shared machine anyone can `git init` in
	// /tmp, which just changes who gets to poison it.
	cmd.Dir = "/"
	// git spawns helper processes of its own (git-remote-https and friends). When the
	// context expires Go only kills git itself, and if a helper still holds the stderr
	// pipe, Output() waits forever — the deadline would be worthless.
	cmd.WaitDelay = 2 * time.Second
	// stdout is deliberately uncapped: it is git's own ref advertisement, sized by the
	// number of tags on the remote, and a repo with enough tags to exhaust memory is
	// not one you can install anything from. stderr is free-form text the other side
	// writes as much of as it likes, so that end must be capped.
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("host: git not found. toy discovers versions with `git ls-remote`, " +
				"so install git first (Debian/Ubuntu: sudo apt install git; macOS: xcode-select --install)")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// %w, not %v: callers up the stack (runUpdate / main) use
			// errors.Is(err, context.Canceled) to recognize Ctrl-C and stop immediately.
			// Swallow that wrapping and Ctrl-C decays into an ordinary failure — every
			// remaining package gets tried, fails instantly, and the user gets a wall of
			// "could not update".
			if errors.Is(ctxErr, context.Canceled) {
				// Cancellation is not a timeout; do not report the user's own Ctrl-C as a
				// network failure.
				return nil, ctxErr
			}
			return nil, fmt.Errorf("host: timed out reading tags from %s (%w) — the network may be "+
				"down, or the repository does not exist / needs authentication", cloneURL, ctxErr)
		}
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("host: `git ls-remote --tags %s` failed: %s", cloneURL, msg)
		}
		return nil, fmt.Errorf("host: `git ls-remote --tags %s` failed: %w", cloneURL, err)
	}
	return parseLsRemote(string(out)), nil
}

// parseLsRemote turns ls-remote output into a list of tag names.
// Each line looks like "<sha>\trefs/tags/v1.2.3"; an annotated tag adds a second
// line ending in "refs/tags/v1.2.3^{}". With --refs git has already filtered those
// out, but the output comes from someone else, so strip and dedupe again ourselves.
func parseLsRemote(out string) []string {
	var tags []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name, ok := strings.CutPrefix(fields[len(fields)-1], "refs/tags/")
		if !ok { // refs/heads/… and friends, not a tag
			continue
		}
		name = strings.TrimSuffix(name, "^{}")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tags = append(tags, name)
	}
	return tags
}

// firstLine relays the first line of git's stderr to the user — that sentence is
// usually the actual reason.
//
// But the `remote: …` lines are **written by the remote server** and travel all
// the way into our error message and onto the user's terminal. So control
// characters are stripped (ANSI escapes can change colours and move the cursor,
// \r can repaint a line and overwrite what was said before it) and the length is
// capped: an error message that can write anything it likes on someone else's
// terminal is not an error message.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= maxStderrLine {
			b.WriteString("…")
			break
		}
		if r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
