package main

// After --force was narrowed it still governs five things (checksum / author key /
// shadowing / trust downgrade / source). Iron law 8 says the trust state has to be
// printed honestly for the user — a SILENT waiver is the same as not having the
// check at all — so this file pins, one by one, that a real waiver has to say what
// it waived. Checksum and author key live in hash_audit_test.go and
// sign_audit_test.go; the other three are here.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
)

// The source changed: an index entry is just a pointer, and editing one line of
// source swaps the content behind an installed command for another repo's.
// --force may waive it, but not without a word.
func TestForceAnnouncesASourceChange(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	fh.publish(t, "bob", "tool", "v2.0.0", "#!/bin/sh\necho MOVED\n")
	_, _, writeIdx := audIndexSetup(t, fh)

	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	writeIdx("fake:bob/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("the source changed and it installed without a word")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("still cannot install after confirming: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "fake:bob/tool") {
		t.Fatalf("--force waived a source change and the output does not say which repo it moved to:\n%s", out)
	}
}

// Trust downgrade: strip the sig off an index entry and "✓ signed" quietly turns
// into "· source …". TOFU records what it looked like last time, so this has to
// stop too — and say so when it is waived.
func TestForceAnnouncesATrustDowngrade(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, _ := audIndexSetup(t, fh)

	raw, err := os.ReadFile(filepath.Join(fh.root, "alice", "tool", "raw", "v1.0.0", "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	idxPath := sgnSignedIndexPath(t, home)
	sgnWriteSignedIndex(t, idxPath, "alice/tool", "fake:alice/tool", raw, sgnKey(t))
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	// Same record, sig and key removed.
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
		{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInstall(context.Background(), []string{"--reinstall", "alice/tool"}); err == nil {
		t.Fatal("the check mark quietly disappeared and it installed anyway")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("still cannot install after confirming: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "signed") {
		t.Fatalf("--force waived a trust downgrade and never said this package is no longer signed:\n%s", out)
	}
}

// Command-name shadowing (iron law 5): the `bin = "sudo"` class of thing.
// --force is its only way through, which makes it all the more important to name
// exactly which file on PATH gets shadowed.
func TestForceAnnouncesAShadowedCommand(t *testing.T) {
	p := i1New(t, "name = \"shadowpkg\"\n[script]\nshadowme = \"s.sh\"\n")
	p.file(t, "s.sh", "#!/bin/sh\necho MINE\n", 0o644)

	other := t.TempDir()
	victim := filepath.Join(other, "shadowme")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// i1New already pointed TOY_HOME at p.home; bin coming first is what the docs teach.
	t.Setenv("PATH", filepath.Join(p.home, "bin")+string(os.PathListSeparator)+other)

	url := "file://" + filepath.Join(p.dir, "toy.toml")
	if err := runInstall(context.Background(), []string{url}); err == nil {
		t.Fatal("it shadowed shadowme on PATH without --force (iron law 5)")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", url}); err != nil {
			t.Fatalf("still cannot install with --force: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, victim) {
		t.Fatalf("--force shadowed %s on PATH and the output never said whom it shadowed:\n%s", victim, out)
	}
}

// `toy install -h` has to spell out the difference between --reinstall and --force.
// It used to throw the flag package's output into io.Discard, leaving the user with
// a single syntax line — and the split between these two switches is written
// nowhere but in their usage strings, so nobody could see it.
func TestInstallHelpExplainsTheTwoSwitches(t *testing.T) {
	out := captureStderr(t, func() {
		if err := runInstall(t.Context(), []string{"-h"}); !errors.Is(err, errUsage) {
			t.Fatalf("-h should take the usage path: %v", err)
		}
	})
	for _, want := range []string{"-reinstall", "-force", "same version again", "safety check"} {
		if !strings.Contains(out, want) {
			t.Errorf("`toy install -h` does not contain %q, so the user cannot tell which switch to use:\n%s", want, out)
		}
	}
}
