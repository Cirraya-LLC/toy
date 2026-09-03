package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// Original suspicion: one `--force` switch waives both "install the same version
// again" and "this version's bytes changed". It held up — a user who just wants
// a reinstall had no other option than to switch off TOFU along with it (iron
// laws 4/6), and stopping exactly those two things is the whole point of TOFU.
//
// Already fixed: `--reinstall` handles reinstalling (an everyday action) and
// `--force` only waives what a safety check reports. This test now guards that
// dividing line — a reinstall must not quietly waive a changed TOFU pin.
func TestReinstallDoesNotWaiveTheVersionPin(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("the first install already failed: %v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("wrong content right after the first install: %q", b)
	}

	// Same tag, the release asset's bytes swapped out — exactly what iron law 6
	// exists to catch.
	asset := filepath.Join(fh.root, "alice", "tool", "rel", "v1.0.0", "audtool")
	if err := os.WriteFile(asset, []byte("#!/bin/sh\necho EVIL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --force on its own must not trigger a reinstall: all it does now is waive
	// what a safety check reports. And it has to say so, or the user walks away
	// believing the thing they waived actually happened.
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("--force must not error here, it should say \"already on this version\": %v", err)
		}
	})
	if !strings.Contains(out, "--reinstall") {
		t.Fatalf("--force alone installed nothing and never told the user that reinstalling takes --reinstall:\n%s", out)
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("--force triggered a reinstall by itself and pulled in %q", b)
	}

	// This is the dividing line itself: intent to reinstall must not waive
	// "this version's content changed".
	err := runInstall(context.Background(), []string{"--reinstall", "alice/tool"})
	if err == nil {
		t.Fatal("--reinstall waived a checksum change too — precisely what splitting it out was meant to prevent (iron laws 4/6)")
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("the installed version was touched even though the install was refused: %q", b)
	}

	// Both intents stated, and every waiver has to be printed item by item (iron law 8).
	out = captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("still cannot install with both --reinstall and --force: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "sha256") {
		t.Fatalf("--force waived a checksum change without spelling out what it waived:\n%s", out)
	}
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho EVIL\n" {
		t.Fatalf("both switches given and the new content still was not installed: %q", b)
	}
}

// Original suspicion: `toy install foo@v1.0.0` installing something older than
// the v2.0.0 already on disk is a hole. Overturned — the user ASKED for that,
// and blocking it makes "I want to roll back one version" impossible.
//
// The real problem sits next door: an upstream deleting its newest tag (account
// taken over, release pulled, a self-hosted host swapped out) travels the exact
// same path, and back then the output said nothing but "installed t 1.0.0" —
// not one word about a downgrade. So it still installs, but it has to say so
// (iron law 8). This test guards that sentence.
func TestDowngradeIsAllowedButAnnounced(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho OLD\n")
	fh.publish(t, "alice", "tool", "v2.0.0", "#!/bin/sh\necho NEW\n")
	_, home, writeIdx := audIndexSetup(t, fh)
	writeIdx("fake:alice/tool")

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho NEW\n" {
		t.Fatalf("the default is the highest tag, got %q", b)
	}

	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool@1.0.0"}); err != nil {
			t.Fatalf("naming an older version explicitly got blocked, and the user asked for it: %v", err)
		}
	})
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho OLD\n" {
		t.Fatalf("asked for @1.0.0 and did not get v1.0.0: %q", b)
	}
	if !strings.Contains(out, "downgrade") {
		t.Fatalf("went from v2.0.0 back to v1.0.0 without one word about it being a downgrade — "+
			"an upstream pulling its newest tag takes the same path, and there the user sees nothing at all:\n%s", out)
	}
}

// insIsOlder only dares call something "older" when both sides are valid semver:
// a guessed "this is a downgrade" is worse than saying nothing (insIsNewer lets
// the same cases through under a different rule — the two functions are not
// mirror images, do not merge them).
func TestIsOlderStaysQuietWhenItCannotTell(t *testing.T) {
	for _, c := range []struct {
		oldTag, newTag string
		want           bool
	}{
		{"v2.0.0", "v1.0.0", true},
		{"v1.0.0", "v2.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"nightly", "v1.0.0", false}, // the installed one is not semver
		{"v2.0.0", "nightly", false}, // this one is not semver
	} {
		got := insIsOlder(store.Manifest{Tag: c.oldTag}, &resolved{Tag: c.newTag})
		if got != c.want {
			t.Errorf("insIsOlder(%q -> %q) = %v, want %v", c.oldTag, c.newTag, got, c.want)
		}
	}
}

// insTOFU's own half was always right. Pinned here: without force it must
// refuse, and the error has to be recognizable via errors.Is(err, sign.ErrChanged).
func TestTOFURejectsAChangedPinWithoutForce(t *testing.T) {
	ring, err := sign.OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "github:o/r@v1.0.0"
	if err := ring.Pin(id, "sha256", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := insTOFU(ring, id, "sha256", strings.Repeat("b", 64), "artifact", false); err == nil {
		t.Fatal("without force this must be refused, iron law 6")
	}
}
