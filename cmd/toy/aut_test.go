package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
)

func TestAutParseRemote(t *testing.T) {
	cases := []struct {
		remote              string
		domain, owner, repo string
		wantErr             bool
	}{
		{remote: "https://github.com/Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "https://github.com/Derrity/tinyserve", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "https://user@gitlab.com/o/r.git", domain: "gitlab.com", owner: "o", repo: "r"},
		{remote: "ssh://git@github.com:22/Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "git@github.com:Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "git@Codeberg.ORG:o/r", domain: "codeberg.org", owner: "o", repo: "r"},
		{remote: "  https://github.com/o/r.git\n", domain: "github.com", owner: "o", repo: "r"},
		// A local-path origin has no domain, so there is no host to infer.
		// Fail loudly instead of guessing one.
		{remote: "/srv/git/tinyserve.git", wantErr: true},
		// GitLab subgroups are three levels deep; a ref only has owner/repo.
		{remote: "https://gitlab.com/group/sub/repo.git", wantErr: true},
		{remote: "https://github.com/onlyowner", wantErr: true},
		{remote: "", wantErr: true},
	}
	for _, c := range cases {
		domain, owner, repo, err := autParseRemote(c.remote)
		if c.wantErr {
			if err == nil {
				t.Errorf("autParseRemote(%q) should have failed, got %s/%s/%s", c.remote, domain, owner, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("autParseRemote(%q) failed: %v", c.remote, err)
			continue
		}
		if domain != c.domain || owner != c.owner || repo != c.repo {
			t.Errorf("autParseRemote(%q) = %s %s %s, want %s %s %s",
				c.remote, domain, owner, repo, c.domain, c.owner, c.repo)
		}
	}
}

// TestAutSignMessageIsFrozen freezes the bytes a signature covers. Change them
// and every signature already out in the wild stops verifying at once -- and
// the error the user sees reads like the author's private key leaked.
func TestAutSignMessageIsFrozen(t *testing.T) {
	got := string(autSignMessage("Derrity/tinyserve", "github:Derrity/tinyserve", []byte("name = \"tinyserve\"\n")))
	want := "Derrity/tinyserve\ngithub:Derrity/tinyserve\nname = \"tinyserve\"\n"
	if got != want {
		t.Fatalf("the signed message changed:\ngot  %q\nwant %q", got, want)
	}
}

func TestAutHostName(t *testing.T) {
	if got := autHostName("github.com", nil); got != "github" {
		t.Errorf("github.com should map back to github, got %q", got)
	}
	if got := autHostName("CODEBERG.org", nil); got != "codeberg" {
		t.Errorf("domain comparison must be case-insensitive, got %q", got)
	}
	// A self-hosted instance the user configured in config.toml must be
	// recognized without touching any code.
	mine := map[string]host.Host{"mygit": {Clone: "https://git.example.com/{owner}/{repo}"}}
	if got := autHostName("git.example.com", mine); got != "mygit" {
		t.Errorf("a self-hosted host should be recognized, got %q", got)
	}
	if got := autHostName("bitbucket.org", nil); got != "" {
		t.Errorf("an unknown domain should return \"\" so the caller asks for --source, got %q", got)
	}
}

func TestAutMirrorHandler(t *testing.T) {
	dir := t.TempDir()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, sum), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Neither directory listings nor arbitrary paths may be exposed, so plant a
	// file whose name is deliberately not a hash.
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(autMirrorHandler(dir))
	defer srv.Close()

	body, code := autGet(t, srv.Client(), srv.URL+"/"+sum)
	if code != http.StatusOK || body != "payload" {
		t.Errorf("fetch by sha256 = %d %q, want 200 \"payload\"", code, body)
	}
	for _, path := range []string{"/", "/secret.txt", "/" + strings.ToUpper(sum), "/../../etc/passwd", "/" + sum + "x"} {
		if _, code := autGet(t, srv.Client(), srv.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (only 64 lowercase hex chars are served)", path, code)
		}
	}
	resp, err := srv.Client().Post(srv.URL+"/"+sum, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", resp.StatusCode)
	}
}

func autGet(t *testing.T, c *http.Client, url string) (string, int) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), resp.StatusCode
}

func TestAutWildcard(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		if !autWildcard(addr) {
			t.Errorf("%q listens on every interface; the user must be warned", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if autWildcard(addr) {
			t.Errorf("%q is loopback only; no warning belongs here", addr)
		}
	}
}

// TestAutSignedEntryVerifies walks the real chain: what we sign must verify
// against the public key in the entry, and flipping a single byte of the
// toy.toml must break it -- otherwise the signature publish hands out protects
// nothing at all.
func TestAutSignedEntryVerifies(t *testing.T) {
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const id, source = "Derrity/tinyserve", "github:Derrity/tinyserve"
	raw := []byte("name = \"tinyserve\"\n")
	sig := sign.Sign(kp.Private, autSignMessage(id, source, raw))
	pub := hex.EncodeToString(kp.Public)

	if err := sign.Verify(pub, autSignMessage(id, source, raw), sig); err != nil {
		t.Fatalf("we cannot verify what we just signed ourselves: %v", err)
	}
	tampered := []byte("name = \"tinyserve\"\n# one line slipped in\n")
	if err := sign.Verify(pub, autSignMessage(id, source, tampered), sig); err == nil {
		t.Fatal("a tampered toy.toml verified")
	}
	if err := sign.Verify(pub, autSignMessage("someone/else", source, raw), sig); err == nil {
		t.Fatal("a swapped id verified -- the signature must bind the name")
	}
}

// TestAutPublishedEntryLoadsIntoAnIndex nails down "pasting it into an index
// does not break it": the entry publish prints must load back through
// index.Load unchanged, and the sig must not get lost on the way -- that is
// what the client uses to decide whether it dares print the signed checkmark.
func TestAutPublishedEntryLoadsIntoAnIndex(t *testing.T) {
	entry := index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "tiny", Sig: "AAAA",
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	idx, skipped, err := index.Load([]byte(`{"version":1,"packages":[`+string(body)+`]}`), "test")
	if err != nil || skipped != 0 || len(idx.Packages) != 1 {
		t.Fatalf("pasted into an index it does not load back: err=%v skipped=%d packages=%d", err, skipped, len(idx.Packages))
	}
	got, ok := idx.Find("Derrity/tinyserve")
	if !ok {
		t.Fatal("the entry made it into the index but cannot be found")
	}
	if got.Sig != "AAAA" {
		t.Fatalf("the sig was lost on the way: %q", got.Sig)
	}
}

// autGitPath must skip toy's own bin directory.
//
// Handing bare "git" to exec runs the first git on PATH, and per the README and
// `toy doctor` ~/.toy/bin IS on PATH -- so any installed package that provides
// a command called git gets executed by `toy publish`. That is a hole straight
// through iron law 1, not an inconvenience.
func TestGitPathSkipsToyOwnBinDir(t *testing.T) {
	planted, real := t.TempDir(), t.TempDir()
	put := func(dir string) string {
		p := filepath.Join(dir, "git")
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fake, want := put(planted), put(real)

	// toy's own bin comes first -- that is exactly how the docs tell users to
	// put it on PATH.
	t.Setenv("PATH", planted+string(os.PathListSeparator)+real)
	got, err := autGitPath(planted)
	if err != nil {
		t.Fatalf("there is a real git on PATH and it still failed: %v", err)
	}
	if got == fake {
		t.Fatalf("picked %s out of toy's own bin directory -- publish would execute it (iron law 1)", fake)
	}
	if got != want {
		t.Fatalf("picked %s, want %s", got, want)
	}

	// When that is the only one left we must refuse, and name the file, so the
	// user knows what to uninstall.
	t.Setenv("PATH", planted)
	switch _, err := autGitPath(planted); {
	case err == nil:
		t.Fatal("the only git on PATH came from an installed package and autGitPath handed it over anyway")
	case !strings.Contains(err.Error(), fake):
		t.Fatalf("it refused, but without naming the file the user has nothing to act on: %v", err)
	}
}

// Relative PATH entries ("." and the empty entry that turns up everywhere)
// point at the process's CURRENT directory. Running toy inside a directory
// somebody else can write to must not mean handing over iron law 1.
func TestGitPathIgnoresRelativePathEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "."+string(os.PathListSeparator))
	if got, err := autGitPath(""); err == nil {
		t.Fatalf("PATH holds only relative entries, yet autGitPath handed over %s from the current directory", got)
	}
}

// The whole path end to end: an installed package squats on the name git, and
// `toy publish` must not execute it.
func TestPublishDoesNotRunGitFromToyBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	if err := os.WriteFile(filepath.Join(bin, "git"),
		[]byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user put ~/.toy/bin on PATH the way the README and `toy doctor` say
	// to, and this machine has no real git installed yet.
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "toy.toml"),
		[]byte("name = \"t\"\n[script]\nt = \"t.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runPublish(t.Context(), []string{dir})
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatalf("`toy publish` executed the git in ~/.toy/bin (%s showed up) -- iron law 1 is broken", marker)
	}
	if err == nil {
		t.Fatal("there is no usable git on PATH, so source cannot be inferred and publish must not succeed")
	}
	// doctor has to make the same judgement, or it will point at that package
	// and report "git found at: ...".
	if c := qryCheckGit(bin); c.ok {
		t.Fatalf("`toy doctor` called an installed package a healthy git: %q", c.title)
	}
}
