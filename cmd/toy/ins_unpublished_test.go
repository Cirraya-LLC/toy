package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// A tag routinely goes up before its release files do. rclone's v1.75.2 was tagged at
// 09:45 UTC and its first file arrived at 10:50; the daily index check ran at 10:42,
// got a 404, and opened an issue blaming the entry. For that whole window every
// `toy install rclone` failed the same way.

// unpublishedSetup gives alice/tool a published v1.0.0 and a v1.1.0 that is tagged,
// with its toy.toml in place, but whose release file has not been uploaded.
func unpublishedSetup(t *testing.T, mutate func(*host.Host)) (*audFakeHost, string) {
	t.Helper()
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho one-zero\n")
	fh.publish(t, "alice", "tool", "v1.1.0", "#!/bin/sh\necho one-one\n")
	if err := os.RemoveAll(filepath.Join(fh.root, "alice", "tool", "rel", "v1.1.0")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	h := fh.tmpl()
	if mutate != nil {
		mutate(&h)
	}
	cfg := store.DefaultConfig()
	cfg.Hosts = map[string]host.Host{"fake": h}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return fh, home
}

func installedBody(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "bin", "audtool"))
	if err != nil {
		t.Fatalf("nothing installed: %v", err)
	}
	return string(b)
}

func TestATaggedButUnpublishedReleaseFallsBackToTheOneBefore(t *testing.T) {
	_, home := unpublishedSetup(t, nil)
	var err error
	out := captureStderr(t, func() {
		err = runInstall(context.Background(), []string{"fake:alice/tool"})
	})
	if err != nil {
		t.Fatalf("install failed instead of falling back: %v\n%s", err, out)
	}
	if got := installedBody(t, home); !strings.Contains(got, "one-zero") {
		t.Fatalf("installed %q, want the v1.0.0 release", got)
	}
	// Said out loud, naming both releases (iron law 8).
	for _, want := range []string{"v1.1.0 is tagged, but its release file", "v1.0.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
	// And nobody is told to report a stale entry: nothing is stale, a release is
	// just still being published.
	if strings.Contains(out, issueURL) {
		t.Errorf("a release still being published was reported as a stale entry:\n%s", out)
	}
}

// --version means exactly that version. Quietly installing a different one would be
// the opposite of what was asked.
func TestAPinnedVersionDoesNotFallBack(t *testing.T) {
	_, home := unpublishedSetup(t, nil)
	var err error
	captureStderr(t, func() {
		err = runInstall(context.Background(), []string{"--version", "v1.1.0", "fake:alice/tool"})
	})
	if err == nil {
		t.Fatalf("--version v1.1.0 installed something although v1.1.0 has no release file: %q",
			installedBody(t, home))
	}
	// And it has to fail for the right reason. Expecting "some error" passed for a
	// while on a missing git binary, which proves nothing about falling back.
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want the 404 on v1.1.0's file, got: %v", err)
	}
}

// `toy update` on a package already at the release before the unpublished one has
// nothing to do; that is "already current", not a failure.
func TestUpdateWaitsQuietlyForAnUnpublishedRelease(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho one-zero\n")
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.DefaultConfig()
	cfg.Hosts = map[string]host.Host{"fake": fh.tmpl()}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := runInstall(context.Background(), []string{"fake:alice/tool"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Now v1.1.0 is tagged, with its files still on their way.
	fh.publish(t, "alice", "tool", "v1.1.0", "#!/bin/sh\necho one-one\n")
	if err := os.RemoveAll(filepath.Join(fh.root, "alice", "tool", "rel", "v1.1.0")); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() { err = runUpdate(context.Background(), nil) })
	if err != nil {
		t.Fatalf("update failed instead of waiting: %v\n%s", err, out)
	}
	if got := installedBody(t, home); !strings.Contains(got, "one-zero") {
		t.Fatalf("installed %q, want v1.0.0 to stay", got)
	}
	if !strings.Contains(out, "already at v1.0.0") {
		t.Errorf("update should say it is already at v1.0.0:\n%s", out)
	}
}

// Only a definite 404 means "not published yet". A 500 is upstream having a bad day,
// and moving someone to an older release over it would be wrong -- the same line the
// inline-toyfile fallback draws (spec §5.1 rule 3).
func TestAServerErrorDoesNotFallBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "v1.1.0") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("#!/bin/sh\necho one-zero\n"))
	}))
	defer srv.Close()
	_, home := unpublishedSetup(t, func(h *host.Host) {
		h.Release = srv.URL + "/{owner}/{repo}/rel/{tag}/{asset}"
	})
	var err error
	out := captureStderr(t, func() {
		err = runInstall(context.Background(), []string{"fake:alice/tool"})
	})
	if err == nil {
		t.Fatalf("a 500 on v1.1.0 fell back and installed %q", installedBody(t, home))
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("want the 500 from v1.1.0's download, got: %v", err)
	}
	if strings.Contains(out, "is tagged, but its release file") {
		t.Errorf("a 500 was described as an unpublished release:\n%s", out)
	}
}
