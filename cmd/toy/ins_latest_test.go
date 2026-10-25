package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// latestSetup is opencode's shape: v1.0.0 is a release with files, and two newer tags
// on another release line have none. latestTo is where the forge's latest-release page
// redirects ("" for no redirect at all).
func latestSetup(t *testing.T, latestTo string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latestTo == "" || r.URL.Path != "/alice/tool/releases/latest" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, latestTo, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func latestInstall(t *testing.T, latestTo string) (string, string, error) {
	t.Helper()
	base := latestSetup(t, "")
	if latestTo != "" {
		base = latestSetup(t, latestTo)
	}
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho one-zero\n")
	for _, tag := range []string{"v2.0.0", "v2.1.0"} {
		fh.publish(t, "alice", "tool", tag, "#!/bin/sh\necho two\n")
		if err := os.RemoveAll(filepath.Join(fh.root, "alice", "tool", "rel", tag)); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	h := fh.tmpl()
	h.Latest = base + "/{owner}/{repo}/releases/latest"
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.DefaultConfig()
	cfg.Indexes = nil
	cfg.Hosts = map[string]host.Host{"fake": h}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() { err = runInstall(context.Background(), []string{"fake:alice/tool"}) })
	return home, out, err
}

// The newest tags are not releases; the forge says v1.0.0 is the latest. That is what
// gets installed, and the warning says whose word it was.
func TestFallsBackToTheReleaseTheForgeMarksLatest(t *testing.T) {
	home, out, err := latestInstall(t, "/alice/tool/releases/tag/v1.0.0")
	if err != nil {
		t.Fatalf("install failed instead of taking the latest release: %v\n%s", err, out)
	}
	if got := installedBody(t, home); !strings.Contains(got, "one-zero") {
		t.Fatalf("installed %q, want v1.0.0", got)
	}
	for _, want := range []string{"v2.1.0 is tagged, but its release file", "Installing v1.0.0", "marks as latest"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
}

// The page's answer is remote content: a tag git ls-remote did not list is ignored,
// and so is a repository with no release (GitHub redirects that to /releases).
// Either way it is the old one-step fallback, which here finds nothing.
func TestALatestPageNamingNoKnownTagIsIgnored(t *testing.T) {
	for _, to := range []string{"/alice/tool/releases/tag/v9.9.9", "/alice/tool/releases", ""} {
		home, out, err := latestInstall(t, to)
		if err == nil {
			t.Errorf("latest -> %q: installed %q although no tag it names has files", to, installedBody(t, home))
		}
		if strings.Contains(out, "marks as latest") {
			t.Errorf("latest -> %q was believed:\n%s", to, out)
		}
	}
}
