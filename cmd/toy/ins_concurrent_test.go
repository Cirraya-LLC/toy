package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Installing packages in parallel is what a Makefile does, and two runs of the
// SAME package used to collide twice over:
//
//	both extracted into <version>.new, one renamed it into place, and the other
//	chmod'd files that were no longer there -- `no such file or directory`, which
//	reads like a broken filesystem
//
//	and once that was locked, the "already at this version" check was still
//	outside the lock, so every run saw "not installed", queued up, and the losers
//	installed a second time onto `a link with that name already exists`
//
// Both left correct state behind. Both exited non-zero, for a reason no user
// could act on.
func TestConcurrentInstallsOfTheSamePackage(t *testing.T) {
	body := audReinstallTGZ(t, []string{"tool"})
	plat := runtime.GOOS + "/" + runtime.GOARCH
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = \"tool.tar.gz\"\n", plat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			w.Write([]byte(toml))
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runInstall(context.Background(), []string{srv.URL + "/toy.toml"})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d failed: %v -- every one of these either installs it or finds it already there", i, err)
		}
	}
	link := filepath.Join(home, "bin", "tool")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("no symlink after %d concurrent installs: %v", n, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink points at nothing: %v", err)
	}
	st, _, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := st.ListManifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("%d manifests after installing one package %d times, want 1", len(ms), n)
	}
}

// Different packages must not contend at all: the lock is per package, and that
// is the case worth keeping fast.
func TestConcurrentInstallsOfDifferentPackages(t *testing.T) {
	body := audReinstallTGZ(t, []string{"tool"})
	plat := runtime.GOOS + "/" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == "toy.toml" {
			name := filepath.Base(filepath.Dir(r.URL.Path))
			fmt.Fprintf(w, "name = %q\n[bin]\n%s = \"tool\"\n[artifact]\n%q = \"tool.tar.gz\"\n", name, name, plat)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	names := []string{"alpha", "bravo", "charlie", "delta"}
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runInstall(context.Background(), []string{srv.URL + "/" + n + "/toy.toml"})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("%s failed: %v", names[i], err)
		}
	}
	for _, n := range names {
		if _, err := os.Readlink(filepath.Join(home, "bin", n)); err != nil {
			t.Errorf("%s has no symlink: %v", n, err)
		}
	}
}
