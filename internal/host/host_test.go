package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The built-in templates must build URLs that really exist — nothing outside a
// test will ever tell you one of these four strings is wrong.
func TestBuiltinURLs(t *testing.T) {
	tests := []struct {
		host        string
		raw         string
		clone       string
		release     string
		web         string
		placeholder bool // the shape is in the built-in table, but Lookup refuses it (see TestLookupRefusesPlaceholder)
	}{
		{
			host:    "github",
			raw:     "https://raw.githubusercontent.com/Derrity/tinyserve/v1.2.3/toy.toml",
			clone:   "https://github.com/Derrity/tinyserve",
			release: "https://github.com/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://github.com/Derrity/tinyserve",
		},
		{
			host:    "gitlab",
			raw:     "https://gitlab.com/Derrity/tinyserve/-/raw/v1.2.3/toy.toml",
			clone:   "https://gitlab.com/Derrity/tinyserve",
			release: "https://gitlab.com/Derrity/tinyserve/-/releases/v1.2.3/downloads/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://gitlab.com/Derrity/tinyserve",
		},
		{
			host:    "codeberg",
			raw:     "https://codeberg.org/Derrity/tinyserve/raw/tag/v1.2.3/toy.toml",
			clone:   "https://codeberg.org/Derrity/tinyserve",
			release: "https://codeberg.org/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:     "https://codeberg.org/Derrity/tinyserve",
		},
		{
			host:        "gitea",
			raw:         "https://gitea.example.com/Derrity/tinyserve/raw/tag/v1.2.3/toy.toml",
			clone:       "https://gitea.example.com/Derrity/tinyserve",
			release:     "https://gitea.example.com/Derrity/tinyserve/releases/download/v1.2.3/tinyserve_1.2.3_linux_amd64.tar.gz",
			web:         "https://gitea.example.com/Derrity/tinyserve",
			placeholder: true,
		},
	}
	const asset = "tinyserve_1.2.3_linux_amd64.tar.gz"
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			h, ok := Builtin()[tt.host]
			if !ok {
				t.Fatalf("the built-in table has no %q", tt.host)
			}
			// A real provider must also come back from Lookup unchanged; the shape is
			// the one Lookup refuses.
			if !tt.placeholder {
				got, err := Lookup(tt.host, nil)
				if err != nil {
					t.Fatalf("Lookup(%q) failed: %v", tt.host, err)
				}
				if got != h {
					t.Errorf("Lookup and Builtin disagree:\n%+v\n%+v", got, h)
				}
			}
			if h.Name != tt.host {
				t.Errorf("Name = %q, want %q", h.Name, tt.host)
			}
			if got := h.RawURL("Derrity", "tinyserve", "v1.2.3", "toy.toml"); got != tt.raw {
				t.Errorf("RawURL = %q\nwant     %q", got, tt.raw)
			}
			if got := h.CloneURL("Derrity", "tinyserve"); got != tt.clone {
				t.Errorf("CloneURL = %q\nwant       %q", got, tt.clone)
			}
			if got := h.ReleaseURL("Derrity", "tinyserve", "v1.2.3", asset); got != tt.release {
				t.Errorf("ReleaseURL = %q\nwant         %q", got, tt.release)
			}
			if got := h.WebURL("Derrity", "tinyserve"); got != tt.web {
				t.Errorf("WebURL = %q\nwant     %q", got, tt.web)
			}
		})
	}
}

// No placeholder may survive substitution, and no second package name may sneak in.
func TestNoLeftoverPlaceholders(t *testing.T) {
	for name, h := range Builtin() {
		urls := []string{
			h.RawURL("o", "r", "ref", "toy.toml"),
			h.CloneURL("o", "r"),
			h.ReleaseURL("o", "r", "v1", "a.tar.gz"),
			h.WebURL("o", "r"),
		}
		for _, u := range urls {
			if strings.Contains(u, "{") || strings.Contains(u, "}") {
				t.Errorf("%s: %q still has an unsubstituted placeholder", name, u)
			}
		}
	}
}

// Callers get a copy; mutating it must not poison the next Builtin().
func TestBuiltinIsNotShared(t *testing.T) {
	m := Builtin()
	m["github"] = Host{Raw: "https://evil.example/{path}"}
	delete(m, "gitlab")
	if got := Builtin()["github"].Raw; !strings.HasPrefix(got, "https://raw.githubusercontent.com/") {
		t.Errorf("the built-in table was poisoned: %q", got)
	}
	if _, ok := Builtin()["gitlab"]; !ok {
		t.Error("the built-in table was poisoned: gitlab is gone")
	}
}

func TestLookup(t *testing.T) {
	overrides := map[string]Host{
		"mygit":  {Raw: "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}"},
		"github": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
	}
	tests := []struct {
		name      string
		lookup    string
		overrides map[string]Host
		wantRaw   string // empty means an error is expected
		wantName  string
	}{
		{name: "built-in", lookup: "github", wantRaw: "https://raw.githubusercontent.com/o/r/ref/p", wantName: "github"},
		{name: "case insensitive", lookup: "GitHub", wantRaw: "https://raw.githubusercontent.com/o/r/ref/p", wantName: "github"},
		{name: "shadows a built-in", lookup: "github", overrides: overrides, wantRaw: "https://mirror.example.com/o/r/ref/p", wantName: "github"},
		{name: "self-hosted", lookup: "mygit", overrides: overrides, wantRaw: "https://git.example.com/o/r/raw/tag/ref/p", wantName: "mygit"},
		{name: "unknown", lookup: "sourcehut"},
		{name: "empty name", lookup: ""},
		{name: "whitespace only", lookup: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := Lookup(tt.lookup, tt.overrides)
			if tt.wantRaw == "" {
				if !errors.Is(err, ErrUnknownHost) {
					t.Fatalf("want ErrUnknownHost, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup(%q) failed: %v", tt.lookup, err)
			}
			if got := h.RawURL("o", "r", "ref", "p"); got != tt.wantRaw {
				t.Errorf("RawURL = %q, want %q", got, tt.wantRaw)
			}
			if h.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", h.Name, tt.wantName)
			}
		})
	}
}

// The error has to be actionable: name what is known now, and how to add a
// self-hosted one.
func TestUnknownHostErrorIsActionable(t *testing.T) {
	_, err := Lookup("sourcehut", map[string]Host{"mygit": {}})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"sourcehut", "github", "mygit", "config.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// An unset template yields an empty string so the caller can say "this host does
// not support that" instead of building half a URL.
func TestEmptyTemplate(t *testing.T) {
	var h Host
	if got := h.RawURL("o", "r", "ref", "p"); got != "" {
		t.Errorf("RawURL = %q, want an empty string", got)
	}
	if got := h.CloneURL("o", "r"); got != "" {
		t.Errorf("CloneURL = %q, want an empty string", got)
	}
	if got := h.ReleaseURL("o", "r", "v1", "a"); got != "" {
		t.Errorf("ReleaseURL = %q, want an empty string", got)
	}
	if got := h.WebURL("o", "r"); got != "" {
		t.Errorf("WebURL = %q, want an empty string", got)
	}
}

func TestParseLsRemote(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "empty input", out: ""},
		{name: "whitespace only", out: "\n  \n\t\n"},
		{
			name: "ordinary",
			out: "8f1c0a2\trefs/tags/v0.9.0\n" +
				"3b2d4e6\trefs/tags/v1.0.0\n" +
				"9a7c1f0\trefs/tags/v1.10.0\n",
			want: []string{"v0.9.0", "v1.0.0", "v1.10.0"},
		},
		{
			// The dereference line of an annotated tag: --refs usually filters it out,
			// and when it does not, it must not turn into an unparsable version like
			// "v1.0.0^{}", nor duplicate v1.0.0.
			name: "annotated dereference line",
			out: "3b2d4e6\trefs/tags/v1.0.0\n" +
				"aa11bb2\trefs/tags/v1.0.0^{}\n" +
				"cc33dd4\trefs/tags/v2.0.0^{}\n",
			want: []string{"v1.0.0", "v2.0.0"},
		},
		{
			name: "every non-tag ref is ignored",
			out: "3b2d4e6\tHEAD\n" +
				"3b2d4e6\trefs/heads/main\n" +
				"77ee88f\trefs/pull/12/head\n" +
				"3b2d4e6\trefs/tags/v1.0.0\n",
			want: []string{"v1.0.0"},
		},
		{
			name: "CRLF and malformed lines",
			out:  "3b2d4e6\trefs/tags/v1.0.0\r\na line with no tab\n\n5c6d7e8\trefs/tags/v1.1.0\n",
			want: []string{"v1.0.0", "v1.1.0"},
		},
		{
			name: "non-semver tags come back as-is (picking a version is the semver package's job)",
			out:  "3b2d4e6\trefs/tags/release-2024\n5c6d7e8\trefs/tags/nightly\n",
			want: []string{"release-2024", "nightly"},
		},
		{
			name: "tag names containing slashes",
			out:  "3b2d4e6\trefs/tags/cli/v1.0.0\n",
			want: []string{"cli/v1.0.0"},
		},
		{
			name: "a tag name that is only ^{} gets dropped",
			out:  "3b2d4e6\trefs/tags/^{}\n5c6d7e8\trefs/tags/v1.0.0\n",
			want: []string{"v1.0.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLsRemote(tt.out)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseLsRemote = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTagsEmptyCloneURL(t *testing.T) {
	// The "and does not run git" half has to be asserted for real: checking only
	// err != nil would still pass with the guard ripped out — git would report its
	// own error about the empty URL on our behalf.
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	writeFakeGit(t, dir, "#!/bin/sh\n: > %s\necho \"deadbee\trefs/tags/v1.0.0\"\n", marker)
	t.Setenv("PATH", dir)

	for _, empty := range []string{"", "  ", "\t\n"} {
		if _, err := Tags(context.Background(), empty); err == nil {
			t.Fatalf("a clone URL of %q should be an error", empty)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("clone URL %q: git was started anyway", empty)
		}
	}
}

// stderr is where the remote server can write whatever it likes, and we only ever
// use the first line.
func TestCappedBufferDropsTheTail(t *testing.T) {
	var c cappedBuffer
	chunk := []byte(strings.Repeat("x", 4096))
	for range 256 { // 1 MiB
		n, err := c.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatalf("Write = (%d, %v); it must claim a full write or git gets an early EPIPE", n, err)
		}
	}
	if got := len(c.String()); got != maxStderrBytes {
		t.Errorf("the buffer kept %d bytes, the cap is %d", got, maxStderrBytes)
	}
}

// With no git on PATH, the error must tell the user to install git, not throw
// "exec: not found" at them.
func TestTagsWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Tags(context.Background(), "https://example.com/o/r")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "git") || !strings.Contains(err.Error(), "install") {
		t.Errorf("the error is not actionable: %v", err)
	}
}

// An iron-law-grade one: a private repo makes git sit on an interactive password
// prompt forever. Use a fake git that echoes the environment it received back as
// tag names, to confirm the variables really are set.
func TestTagsDisablesTerminalPrompt(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"echo \"deadbee\trefs/tags/prompt-$GIT_TERMINAL_PROMPT\"\n" +
		"echo \"deadbee\trefs/tags/askpass-[$GIT_ASKPASS]\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	tags, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	want := []string{"prompt-0", "askpass-[]"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("the environment was not set: %#v, want %#v", tags, want)
	}
}

// When git itself fails, relay its first stderr line to the user — that sentence
// is usually the actual reason.
func TestTagsReportsGitStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: repository not found' >&2\nexit 128\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	_, err := Tags(context.Background(), "https://example.com/o/nope")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "repository not found") {
		t.Errorf("the error does not carry git's own words: %v", err)
	}
}

// An expired context must say "timed out", not dump "signal: killed" on the user.
func TestTagsTimeout(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// dir goes first so the fake git still wins; the rest is there for the sleep
	// inside the script.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Tags(ctx, "https://example.com/o/r")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("it does not say it timed out: %v", err)
	}
}

// Really run git once (against a local repo, no network): proof that the command
// line is right and that annotated tags come back clean.
func TestTagsAgainstLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "one")
	run("tag", "v1.0.0")                          // lightweight tag
	run("tag", "-a", "v1.2.0", "-m", "annotated") // annotated: adds a ^{} line
	run("tag", "release-2024")                    // not semver, comes back anyway

	tags, err := Tags(context.Background(), repo)
	if err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	got := map[string]bool{}
	for _, tag := range tags {
		if strings.Contains(tag, "^") || strings.HasPrefix(tag, "refs/") {
			t.Errorf("tag not stripped clean: %q", tag)
		}
		got[tag] = true
	}
	for _, want := range []string{"v1.0.0", "v1.2.0", "release-2024"} {
		if !got[want] {
			t.Errorf("missing tag %q, got %v", want, tags)
		}
	}
	if len(tags) != 3 {
		t.Errorf("duplicate or extra tags: %v", tags)
	}
}

// The gitea entry is a "shape", not a provider: using it as-is aims at an RFC 2606
// reserved domain and all the user gets is a DNS error — which tells them nothing
// about what they were supposed to configure. Stop it here and hand them a template
// they can paste.
//
// This only asserts that the template names the host and the four keys. What the
// template looks like ([hosts.x] section vs. hosts.x = {…} inline table) is
// formatting, not what this test pins down — the thing worth pinning is "does it
// work when pasted back", and that one lives in hint_test.go, where the line is fed
// to a TOML parser.
func TestLookupRefusesPlaceholder(t *testing.T) {
	_, err := Lookup("gitea", nil)
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("want ErrUnknownHost, got %v", err)
	}
	for _, want := range []string{"config.toml", "hosts.gitea", "raw", "clone", "release", "web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// What we refuse must never show up in another error's "known hosts" list.
	_, err = Lookup("sourcehut", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if i := strings.Index(err.Error(), "Known hosts are"); i >= 0 {
		list, _, _ := strings.Cut(err.Error()[i:], ";")
		if strings.Contains(list, "gitea") {
			t.Errorf("the known list must not contain the refused gitea: %q", list)
		}
	}
	// But once the user points it at a real domain in config.toml, the same name
	// must work.
	h, err := Lookup("gitea", map[string]Host{"gitea": giteaPaths("https://git.example.com")})
	if err != nil {
		t.Fatalf("still refused after configuring a real domain: %v", err)
	}
	if got, want := h.CloneURL("o", "r"), "https://git.example.com/o/r"; got != want {
		t.Errorf("CloneURL = %q, want %q", got, want)
	}
}

// A [hosts.github] with a misspelled key (clone_url instead of clone) becomes an
// empty section that shadows the built-in github — from then on github is entirely
// broken, and the error surfaces three packages away.
func TestLookupRejectsEmptyOverride(t *testing.T) {
	_, err := Lookup("github", map[string]Host{"github": {}})
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("want ErrUnknownHost, got %v", err)
	}
	for _, want := range []string{"[hosts.github]", "raw", "clone", "release", "web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// When a built-in and an override share a name, the "known hosts" list must not
// say that name twice.
func TestKnownHostsListedOnce(t *testing.T) {
	_, err := Lookup("sourcehut", map[string]Host{
		"github": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
		"GitHub": {Raw: "https://mirror.example.com/{owner}/{repo}/{ref}/{path}"},
	})
	if err == nil {
		t.Fatal("want an error")
	}
	i := strings.Index(err.Error(), "Known hosts are")
	if i < 0 {
		t.Fatalf("the error has no known-hosts list: %v", err)
	}
	list, _, _ := strings.Cut(err.Error()[i:], ";")
	if n := strings.Count(strings.ToLower(list), "github"); n != 1 {
		t.Errorf("github appears %d times in the known list: %q", n, list)
	}
}

// Name becomes the directory name in ~/.toy/pkg/<host>/…, so both spellings must
// land in the same directory or the same package gets installed twice.
func TestLookupNameIsCanonicalLowercase(t *testing.T) {
	overrides := map[string]Host{"MyGit": {Raw: "https://git.example.com/{owner}/{repo}/{ref}/{path}"}}
	h, err := Lookup("MyGit", overrides)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if h.Name != "mygit" {
		t.Errorf("Name = %q, want %q", h.Name, "mygit")
	}
}

// asset is the only one of the four inputs that comes from the package author (the
// [artifact] value in toy.toml). On all four providers it is a flat file name, so
// slashes and .. are escaped away: otherwise an author could point the download at
// someone else's release on the same domain while the install line still truthfully
// reads "source github.com/<themselves>" — exactly the silent lie iron law 8 forbids.
func TestReleaseURLEscapesAsset(t *testing.T) {
	h, err := Lookup("github", nil)
	if err != nil {
		t.Fatal(err)
	}
	const base = "https://github.com/victim/repo/releases/download/v1.0.0/"
	tests := []struct{ asset, want string }{
		// an ordinary asset name must not change by a single byte
		{"toy_1.0.0_linux_amd64.tar.gz", base + "toy_1.0.0_linux_amd64.tar.gz"},
		{"../../../attacker/repo/releases/download/v9/evil", base + "..%2F..%2F..%2Fattacker%2Frepo%2Freleases%2Fdownload%2Fv9%2Fevil"},
		{"my tool.tar.gz", base + "my%20tool.tar.gz"},
		{"x?a=b", base + "x%3Fa=b"},
		{"x#frag", base + "x%23frag"},
	}
	for _, tt := range tests {
		got := h.ReleaseURL("victim", "repo", "v1.0.0", tt.asset)
		if got != tt.want {
			t.Errorf("ReleaseURL(asset=%q) = %q\nwant %q", tt.asset, got, tt.want)
		}
		if !strings.HasPrefix(got, base) {
			t.Errorf("asset %q escaped the release path: %q", tt.asset, got)
		}
	}
}

// Iron law 1: never execute anyone else's code. In `git ls-remote <url>`, a url
// starting with '-' is read by git as an option, and --upload-pack=<command> is
// arbitrary command execution off the shelf.
// What is asserted is that git was never started at all — so this fake git leaves
// a file behind the moment it runs.
func TestTagsRefusesOptionLikeURL(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	// Shell builtins only: PATH has been replaced with a directory holding nothing
	// but the fake git, so using touch would fail "because touch was not found" —
	// and the test would then pass for the wrong reason (the first version really
	// did). This fake git both leaves the file and makes Tags succeed, so either
	// check fires.
	writeFakeGit(t, dir, "#!/bin/sh\n: > %s\necho \"deadbee\trefs/tags/v1.0.0\"\n", marker)
	t.Setenv("PATH", dir)

	for _, bad := range []string{"--upload-pack=touch /tmp/pwned", "-u", "  --exec=x  "} {
		if _, err := Tags(context.Background(), bad); err == nil {
			t.Fatalf("%q should have been refused", bad)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%q: git really was started", bad)
		}
	}
}

// Nothing else will tell you the command line is wrong: the fake git echoes its own
// arguments back as tag names.
// This also pins the `--` separator — without it, the refusal above would be the
// only line of defence.
func TestTagsCommandLine(t *testing.T) {
	dir := t.TempDir()
	writeFakeGit(t, dir, "#!/bin/sh\nfor a in \"$@\"; do echo \"deadbee\trefs/tags/$a\"; done\n")
	t.Setenv("PATH", dir)

	got, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	want := []string{"ls-remote", "--tags", "--refs", "--", "https://example.com/o/r"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git's arguments were %#v, want %#v", got, want)
	}
}

// git reads the .git/config of whatever repo the CURRENT DIRECTORY belongs to, and
// url.insteadOf and credential.helper there can both make git go execute something
// else. Running toy from a directory someone else can write to must not hand away
// iron law 1, so git has to run in a neutral directory.
func TestTagsDoesNotRunInCurrentDirectory(t *testing.T) {
	bin := t.TempDir()
	writeFakeGit(t, bin, "#!/bin/sh\necho \"deadbee\trefs/tags/cwd\"\necho \"deadbee\trefs/tags/$(pwd)\"\n")
	t.Setenv("PATH", bin)

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	tags, err := Tags(context.Background(), "https://example.com/o/r")
	if err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("got %#v", tags)
	}
	if strings.HasPrefix(tags[1], work) {
		t.Errorf("git ran in the current directory %q (it reads the .git/config there)", work)
	}
}

// That WaitDelay line looks very much like something you could delete in passing,
// so this pins the scenario it protects: a helper process git spawned outlives git
// and still holds the stderr pipe, and killing git itself does not make Output()
// return. Without WaitDelay this call hangs until that grandchild exits on its own.
func TestTagsSurvivesOrphanHoldingPipes(t *testing.T) {
	dir := t.TempDir()
	// `sleep 30 &` inherits the stdout/stderr pipes; the expiring ctx can only kill
	// the foreground sh.
	writeFakeGit(t, dir, "#!/bin/sh\nsleep 30 &\nsleep 20\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Tags(ctx, "https://example.com/o/r")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("want a timeout error, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Tags was wedged by a grandchild holding the pipes — WaitDelay is not working")
	}
}

// The `remote: …` lines are written by the remote server and travel all the way
// into the error and onto the user's terminal. An error message that can write
// anything it likes there (change colours, use \r to paint over the sentence
// before it) is not an error message.
func TestTagsSanitizesGitStderr(t *testing.T) {
	dir := t.TempDir()
	writeFakeGit(t, dir,
		"#!/bin/sh\nprintf 'remote: \\033[31mfatal\\033[0m\\rHACKED %s\\n' >&2\nexit 128\n",
		strings.Repeat("A", 2000))
	t.Setenv("PATH", dir)

	_, err := Tags(context.Background(), "https://example.com/o/r")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, bad := range []string{"\x1b", "\r", "\a", "\b"} {
		if strings.Contains(msg, bad) {
			t.Errorf("the error still contains the control char %q: %q", bad, msg)
		}
	}
	if len(msg) > 2*maxStderrLine {
		t.Errorf("the error was not capped, length %d: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "fatal") {
		t.Errorf("the useful words were stripped along with the rest: %q", msg)
	}
}

func writeFakeGit(t *testing.T, dir, script string, args ...any) {
	t.Helper()
	if len(args) > 0 {
		script = fmt.Sprintf(script, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// When Ctrl-C lands during ls-remote, the error must still carry context.Canceled —
// runUpdate's `case errors.Is(err, context.Canceled): return err` depends on it
// entirely, and swallowing it means every remaining package gets tried after the
// user already hit Ctrl-C.
// This also pins the timeout branch: it must still say "timed out" AND be
// recognizable via errors.Is as DeadlineExceeded.
func TestTagsPropagatesContextError(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Tags(canceled, "https://example.com/o/r"); !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled error does not match context.Canceled, so callers will treat Ctrl-C as an ordinary failure and keep going: %v", err)
	}

	timeout, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	_, err := Tags(timeout, "https://example.com/o/r")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the timeout error does not match context.DeadlineExceeded: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("it timed out but does not say so: %v", err)
	}
}

func TestTagFromLatest(t *testing.T) {
	cases := map[string]string{
		"https://github.com/anomalyco/opencode/releases/tag/v1.19.4": "v1.19.4",
		"https://codeberg.org/forgejo/forgejo/releases/tag/v17.0.0":  "v17.0.0",
		"https://gitlab.com/gitlab-org/cli/-/releases/v1.124.0":      "v1.124.0",
		"https://github.com/BurntSushi/ripgrep/releases/tag/15.2.0":  "15.2.0",
		"https://github.com/octocat/Hello-World/releases":            "releases", // no release: callers reject it
		"/a/b/releases/tag/jq-1.8.2/":                                "jq-1.8.2",
		"::not a url":                                                "",
	}
	for in, want := range cases {
		if got := TagFromLatest(in); got != want {
			t.Errorf("TagFromLatest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuiltinHostsHaveALatestPage(t *testing.T) {
	for _, name := range []string{"github", "gitlab", "codeberg"} {
		h, err := Lookup(name, nil)
		if err != nil || !strings.Contains(h.LatestURL("o", "r"), "/o/r/") {
			t.Errorf("%s: latest page %q, %v", name, h.LatestURL("o", "r"), err)
		}
	}
}
