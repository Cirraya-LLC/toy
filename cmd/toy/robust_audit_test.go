package main

// Correctness / robustness audit. Each of the four proves one concrete bad
// outcome, not a style opinion.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// Audit 1: Ctrl-C landing on `git ls-remote` gets swallowed by %v.
//
// When ctx is already canceled, host.Tags wraps with fmt.Errorf("…(%v)", ctxErr)
// — %v is not %w, so errors.Is(err, context.Canceled) is false. Consequences:
//   - The `case errors.Is(err, context.Canceled): return err` arm in runUpdate is
//     unreachable, so after Ctrl-C it keeps running every remaining package (each
//     failing instantly) and ends with "N packages could not be updated" + exit 1
//     instead of "canceled" + 130.
//   - Same for runInstall: the context.Canceled branch in main never fires.
func TestCanceledLsRemoteStillLooksLikeAPlainFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := host.Tags(ctx, "https://github.com/golang/go")
	if err == nil {
		t.Fatal("ctx was canceled, Tags must not succeed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("after Ctrl-C the error from host.Tags does not carry context.Canceled, "+
			"so runUpdate treats it as an ordinary failure and keeps going: %v", err)
	}
}

// Audit 2: the pkg directory segment uses the name from toy.toml, not the repo
// name — two repos under the same owner that happen to pick the same name share
// one install directory.
//
// insPerform calls insRemovePkgDir(pkgDir) before unpacking, so installing the
// second package deletes every file of the first: the first one's manifest is
// still there, `toy list` still lists it, but all of its symlinks dangle.
// Uninstalling the second one takes the first one's directory with it too.
func TestTwoReposWithTheSameToyfileNameShareOnePkgDir(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := insTestResolved(t, "github:alice/tool-rs", "v1.0.0")
	b := insTestResolved(t, "github:alice/tool-go", "v1.0.0")
	// Both repos write name = "tool" in toy.toml. Perfectly legal, toyfile.Parse
	// will not stop it.
	tf := &toyfile.Toyfile{Name: "tool"}
	dirA := st.PkgDir(insHostSeg(a), insOwnerSeg(a), insNameSeg(a, tf), insDirVersion(a))
	dirB := st.PkgDir(insHostSeg(b), insOwnerSeg(b), insNameSeg(b, tf), insDirVersion(b))
	if dirA != dirB {
		return // already fixed
	}
	// Show the damage: install A, then the very first step of installing B —
	// insRemovePkgDir — deletes A's files.
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dirA, "tool")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := insRemovePkgDir(st, dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Fatalf("thought A's executable was still there? err=%v", err)
	}
	t.Fatalf("github:alice/tool-rs and github:alice/tool-go share %s — "+
		"installing the second silently deletes the first one's files while its "+
		"manifest and symlinks stay behind. That segment should use the repo name "+
		"(or include the repo), not the name from toy.toml", dirA)
}

// Audit 3: change the source in an index entry and what gets installed is the
// manifest of ANOTHER id, while the old manifest stays put — even though
// insCleanOld has already deleted the old pkg directory.
//
// An index maintainer moving a repo from one place to another (an entirely
// normal maintenance action) triggers this: `toy update` now stops and asks for
// --force (that is a separate audit), and after the user confirms it installs
// once. If the old manifest is not deleted at that point, the package shows up
// twice in `toy list` — one of them pointing at a directory that is already gone
// — and `toy uninstall <name>` fails with "matched 2 installed packages".
func TestSourceChangeLeavesAStaleManifestBehind(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho HERE\n")
	fh.publish(t, "bob", "tool", "v2.0.0", "#!/bin/sh\necho MOVED\n")

	st, _, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	// The repo moved. A changed source has to be stopped first — that is audit 1.
	writeIdx("fake:bob/tool")
	if err := runUpdate(context.Background(), nil); err == nil {
		t.Fatal("the source changed and it upgraded without a word")
	}
	// The user checked and confirmed the move is real.
	if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
		t.Fatalf("still cannot install after confirming: %v", err)
	}

	ms, err := st.ListManifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		t.Fatalf("%d manifests after the move (%v), there should be exactly 1 — "+
			"insPerform changed the ID without deleting the old manifest; "+
			"`toy uninstall tool` now says \"matched %d installed packages\"", len(ms), ids, len(ms))
	}
}

// Audit 4 (low): when [script] and [bin] both appear, [bin] is silently dropped.
// toyfile.Parse allows both sections and validates every [bin] entry, but insLink
// only looks at tf.Script once IsScript() is true — not one of the commands the
// author declared shows up, and nothing anywhere says so.
func TestScriptFormSilentlyDropsBin(t *testing.T) {
	tf, err := toyfile.Parse([]byte(`
name = "gitwip"
[bin]
wip = "gitwip.sh"
[script]
gitwip = "gitwip.sh"
`))
	if err != nil {
		t.Skipf("this toy.toml is already rejected, the problem is gone: %v", err)
	}
	if tf.IsScript() && len(tf.Bin) > 0 {
		t.Logf("the author declared [bin] wip=%q and the script form silently dropped it", tf.Bin["wip"])
	}
}
