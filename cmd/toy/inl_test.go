package main

// The six boundaries of index-inlined toyfiles (index.Entry.Toyfile) at the
// command layer, one test each. All of them run the real chain through the
// audFakeHost fake host -- ls-remote for the tag, fetch toy.toml, download the
// asset, symlink it -- except that the toy.toml on raw is sometimes missing on
// purpose.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

const inlBody = "#!/bin/sh\necho FROM-RELEASE\n"

type inlEnv struct {
	fh   *audFakeHost
	home string
	idx  string // path to the index file; write rewrites the entry at will
}

func inlSetup(t *testing.T) *inlEnv {
	t.Helper()
	fh := audNewFakeHost(t)
	_, home, _ := audIndexSetup(t, fh)
	return &inlEnv{fh: fh, home: home, idx: sgnSignedIndexPath(t, home)}
}

func (e *inlEnv) write(t *testing.T, entry index.Entry) {
	t.Helper()
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.idx, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *inlEnv) rawToyfile(owner, repo, tag string) string {
	return filepath.Join(e.fh.root, owner, repo, "raw", tag, "toy.toml")
}

// publishBare builds an upstream with NO toy.toml -- what the several thousand
// existing CLI tools actually look like.
func (e *inlEnv) publishBare(t *testing.T, owner, repo, tag string) {
	t.Helper()
	e.fh.publish(t, owner, repo, tag, inlBody)
	if err := os.Remove(e.rawToyfile(owner, repo, tag)); err != nil {
		t.Fatal(err)
	}
}

// inlToyfile is the packaging description an index maintainer writes. The asset
// name audtool is the file audFakeHost puts into the release.
func inlToyfile(bin string) string {
	return fmt.Sprintf("name = \"audtool\"\ndesc = \"written by the index\"\n[bin]\n%s = \"audtool\"\n[artifact]\n%q = \"audtool\"\n",
		bin, i1Plat())
}

func inlManifest(t *testing.T, home, id string) store.Manifest {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.LoadManifest(id)
	if err != nil {
		t.Fatalf("cannot read the install manifest for %s: %v", id, err)
	}
	return m
}

// Rule 3: when upstream has no toy.toml, the inlined one takes over and the
// install succeeds -- and the trust tier must land on store.TrustIndexed and
// say so honestly (iron law 8).
func TestInlineToyfileInstallsWhenUpstreamHasNone(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})

	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
			t.Fatalf("upstream has no toy.toml, the index supplied one, and it still will not install: %v", err)
		}
	})

	link := filepath.Join(e.home, "bin", "audtool")
	if b, err := os.ReadFile(link); err != nil || string(b) != inlBody {
		t.Fatalf("what got installed is not the bytes from the release: %q (err=%v)", b, err)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustIndexed {
		t.Fatalf("trust tier in the manifest = %q, want %q", got, store.TrustIndexed)
	}
	// Iron law 8: this line must say which half is not the author's, and it
	// must not look anything like a signed install.
	for _, want := range []string{"index", "author"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output does not say where the packaging description came from (missing %q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ signed") {
		t.Fatalf("the inlined tier printed the signed checkmark:\n%s", out)
	}
}

// Rule 1: when upstream has its own toy.toml, the inlined one is never even
// looked at -- otherwise a malicious entry could swap out the command name the
// author declared, and that is hijacking.
func TestUpstreamToyfileBeatsTheInlineOne(t *testing.T) {
	e := inlSetup(t)
	e.fh.publish(t, "alice", "tool", "v1.0.0", inlBody) // this version has a toy.toml on raw, declaring the command audtool
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("hijacked")})

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "hijacked")); err == nil {
		t.Fatal("the index's inlined toyfile overrode the author's own -- PATH now carries a command the author never declared")
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "audtool")); err != nil {
		t.Fatalf("the command the author declared did not get installed: %v", err)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustHost {
		t.Fatalf("we used the author's own toyfile, yet the trust tier is %q", got)
	}
}

// The other half of rule 1, and the security boundary this whole design rests
// on: ONLY a definite "does not exist" yields the floor. Timeouts, 5xx and
// unreadable files all keep failing as before -- a network blip must not
// quietly move the user from "the author's declaration" to "the index's
// declaration", with nothing anywhere saying so.
func TestTransientFailureNeverFallsBackToInline(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	// Replace toy.toml with a directory: it opens fine and then fails to read
	// (EISDIR) -- which is not "does not exist".
	if err := os.Mkdir(e.rawToyfile("alice", "tool", "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})

	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("fetching toy.toml merely failed (it was not a 404) and we still fell back to the index's inlined copy")
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "audtool")); err == nil {
		t.Fatal("this install should have failed outright, yet the command is already symlinked")
	}
}

// rsvIsNotFound is the entire basis for the boundary above, so pin it on its own.
//
// The test is on the TYPE, not on text: the reason phrase in "HTTP 500 ..." is
// whatever the server chose to send. Match the substring "HTTP 404" and an
// upstream answering `HTTP/1.1 500 HTTP 404 Not Found` gets a 500 counted as
// "does not exist". fetch has a matching regression test of its own.
func TestRsvIsNotFound(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"file:// will not open", fmt.Errorf("cannot fetch: %w", fs.ErrNotExist), true},
		{"a 404 wrapped by fetch", fmt.Errorf("cannot fetch: %w: %w", fs.ErrNotExist, errors.New("HTTP 404 Not Found")), true},
		{"HTTP 500 does not count", errors.New("https://x/toy.toml: HTTP 500 Internal Server Error"), false},
		{"nor does a 500 whose reason phrase says HTTP 404",
			errors.New("https://x/toy.toml: HTTP 500 HTTP 404 Not Found"), false},
		{"HTTP 403 does not count", errors.New("https://x/toy.toml: HTTP 403 Forbidden"), false},
		{"a timeout does not count", errors.New("context deadline exceeded"), false},
		{"a DNS failure does not count", errors.New("dial tcp: lookup x: no such host"), false},
		{"a read failure does not count", errors.New("read /x/toy.toml: is a directory"), false},
	} {
		if got := rsvIsNotFound(c.err); got != c.want {
			t.Errorf("%s: rsvIsNotFound = %v, want %v", c.name, got, c.want)
		}
	}
}

// Rule 2: an inlined toyfile never reaches the signed checkmark, signature or
// not. The signature covers what THE PERSON WHO SUBMITTED THIS INDEX RECORD
// wrote, and that signer is not the author -- the checkmark would claim more
// than it can (14.3). The fingerprint still has to be kept, or TOFU can no
// longer stop a key swap (spec 4).
func TestInlineToyfileNeverReachesSignedTrust(t *testing.T) {
	kp := sgnKey(t)
	const id, source = "alice/tool", "fake:alice/tool"
	raw := []byte(inlToyfile("audtool"))
	tf, err := toyfile.ParseFrom(raw, toyfile.OriginIndex)
	if err != nil {
		t.Fatal(err)
	}
	res := &resolved{Toyfile: tf, Raw: raw, Trust: store.TrustIndexed}
	e := index.Entry{ID: id, Source: source, Toyfile: string(raw),
		Key: hex.EncodeToString(kp.Public), Sig: sign.Sign(kp.Private, autSignMessage(id, source, raw))}

	rsvApplySignature("local", e, res)
	if res.Trust != store.TrustIndexed {
		t.Fatalf("a signature lifted the inlined toyfile to %q -- the signer is not the author, so that checkmark claims too much", res.Trust)
	}
	if res.Signer == "" {
		t.Fatal("the fingerprint was dropped: nothing stops a key swap now")
	}

	// Control: the same signature over an upstream toyfile still earns the
	// checkmark -- do not break that path along the way.
	up, err := toyfile.ParseFrom(raw, toyfile.OriginUpstream)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &resolved{Toyfile: up, Raw: raw, Trust: store.TrustHost}
	rsvApplySignature("local", e, upstream)
	if upstream.Trust != store.TrustSigned {
		t.Fatalf("the author's own toyfile verified and got no checkmark: %q", upstream.Trust)
	}
}

// Rule 6, downgrade direction: upstream's toy.toml is gone and the index's
// inlined one takes over. Somebody else just made a decision on the author's
// behalf -- stop and require --force, and say what was waived when it goes
// through.
func TestUpstreamDisappearingIsADowngrade(t *testing.T) {
	e := inlSetup(t)
	e.fh.publish(t, "alice", "tool", "v1.0.0", inlBody)
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("the first install failed: %v", err)
	}

	// The new version has no toy.toml upstream; only the index's copy is left.
	e.publishBare(t, "alice", "tool", "v2.0.0")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("trust dropped from \"the hosting account\" to \"packaging description from the index\" and it installed without a word")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("still will not install after confirming: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "index") {
		t.Fatalf("--force waived a downgrade without the output showing which tier we landed on:\n%s", out)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustIndexed {
		t.Fatalf("trust tier after install = %q", got)
	}
}

// Rule 6, upgrade direction: the author later added a toy.toml of their own.
// That is good news, so install normally -- but say something, because who
// decides "which asset, what the command is called" just changed hands.
func TestUpstreamTakingOverIsAnnounced(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("the first install failed: %v", err)
	}

	e.fh.publish(t, "alice", "tool", "v2.0.0", inlBody) // for this version the author wrote their own toy.toml
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
			t.Fatalf("the author came on board and now it will not install: %v", err)
		}
	})
	if !strings.Contains(out, "no longer come from the index") {
		t.Fatalf("the packaging description changed hands and the output does not say so:\n%s", out)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustHost {
		t.Fatalf("trust tier after switching to the author's copy = %q, want %q", got, store.TrustHost)
	}
}

// `toy publish --inline` puts the toy.toml verbatim into the entry, and that
// entry has to load back out of an index.
func TestPublishInlineEmbedsTheToyfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOY_HOME", t.TempDir())
	body := inlToyfile("audtool")
	if err := os.WriteFile(filepath.Join(dir, "toy.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := inlPublish(t, "--inline", "--source", "github:alice/tool", dir)
	if entry.Toyfile != body {
		t.Fatalf("the toyfile in the entry does not match the local one:\n%q\n%q", entry.Toyfile, body)
	}
	idx, skipped, err := index.Load([]byte(`{"version":1,"packages":[`+inlJSON(t, entry)+`]}`), "test")
	if err != nil || skipped != 0 || len(idx.Packages) != 1 {
		t.Fatalf("an inlined entry pasted into an index does not load back: err=%v skipped=%d", err, skipped)
	}

	// Without --inline it stays the old pointer-only entry (spec 5).
	if plain := inlPublish(t, "--source", "github:alice/tool", dir); plain.Toyfile != "" {
		t.Fatalf("no --inline was given and it inlined anyway: %q", plain.Toyfile)
	}
}

func inlJSON(t *testing.T, e index.Entry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// inlPublish runs runPublish once and reads back the entry it wrote to stdout
// (entries go to stdout, human-facing notes to stderr; spec 9).
func inlPublish(t *testing.T, args ...string) index.Entry {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := runPublish(context.Background(), args)
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("publish failed: %v\n%s", runErr, out)
	}
	var e index.Entry
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("what publish printed is not a usable entry: %v\n%s", err, out)
	}
	return e
}
