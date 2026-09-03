package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// Original suspicion: a remote toy.toml could write [artifact] as file:///…, so
// toy reads an arbitrary local file, installs it into ~/.toy/pkg as if it were a
// binary, symlinks it onto PATH, and drops it into ~/.toy/cache/<sha256> along the
// way — the very directory `toy serve` hands out.
//
// This is unreachable now: another fix (toyfile refuses a URL as the value of
// artifact/script) closed it on the way past. Addresses may only be assembled by
// toy from the host templates, and a hardcoded URL is refused at parse time.
// Flipped into a regression test: that refusal has to happen, and the error has
// to tell the author what to write instead.
func TestArtifactValueCannotBeAURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	toml := fmt.Sprintf("name = \"pwn\"\n[bin]\npwn = \"id_rsa\"\n[artifact]\n%q = \"file://%s\"\n",
		runtime.GOOS+"/"+runtime.GOARCH, secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, toml)
	}))
	defer srv.Close()

	err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"})
	if err == nil {
		t.Fatalf("a remote toy.toml wrote [artifact] as file://%s and toy read it anyway — "+
			"the value may only be an asset name from the release, toy assembles the address itself", secret)
	}
	if !errors.Is(err, toyfile.ErrUnsafeTarget) {
		t.Fatalf("it was refused, but not because the value is unsafe (the error does not carry ErrUnsafeTarget), "+
			"so a different toy.toml could slip through again: %v", err)
	}
	// The author reads this sentence; it has to say what is wrong and what to write.
	for _, want := range []string{"URL", "asset name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not contain %q, so the author cannot tell what to change: %v", want, err)
		}
	}
	// Nothing at all should have landed.
	if _, err := os.Stat(filepath.Join(home, "bin", "pwn")); !os.IsNotExist(err) {
		t.Errorf("it was refused and still created the symlink: %v", err)
	}
}

// Original suspicion: the username and password in a bare URL get written
// verbatim into the install manifest and the TOFU record (plaintext on disk), and
// show up in the "source" column of `toy list`.
//
// That install path is unreachable now as well — it also relied on writing the
// artifact value as file:// (see above). Flipped into a regression test: a bare
// URL carrying credentials has to be refused at parse time, and that password must
// neither hit the disk nor appear in any line of output the user sees (error text
// ends up in logs and in issues, which spreads further than the disk does).
func TestCredentialsInBareURLNeverLandOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	payload := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = \"file://%s\"\n",
		runtime.GOOS+"/"+runtime.GOARCH, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, toml)
	}))
	defer srv.Close()

	u := strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1) + "/toy.toml"
	err := runInstall(context.Background(), []string{u})
	if err == nil {
		t.Fatal("this toy.toml wrote [artifact] as file://, it should have been refused at parse time")
	}
	// The credentials must not reach the user's output either. Scrubbing is
	// internal/ui's job (safe() -> redactURLs, "plug the source once"); this
	// exercises the same path main takes. Scrubbing a second time inside cmd
	// would only add an implementation that drifts away from it.
	out := captureStderr(t, func() { ui.Error("%v", err) })
	if strings.Contains(out, "hunter2") {
		t.Errorf("the password from the bare URL was printed verbatim to the user (it ends up in logs and issues):\n%s", out)
	}

	var hits []string
	filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, e := os.ReadFile(p); e == nil && strings.Contains(string(b), "hunter2") {
			hits = append(hits, p)
		}
		if strings.Contains(p, "hunter2") {
			hits = append(hits, p+" (in the file name)")
		}
		return nil
	})
	if len(hits) > 0 {
		t.Errorf("the password is sitting in plaintext in these files: %v", hits)
	}
}

// Audit: path handling and Range in toy serve. The verdict should be 404 / 416
// across the board, with nothing read out of bounds.
func TestAuditMirrorHandlerPaths(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644)
	name := strings.Repeat("ab", 32)
	os.WriteFile(filepath.Join(dir, name), []byte("hello-mirror"), 0o644)
	h := autMirrorHandler(dir)

	for _, p := range []string{
		"/../secret.txt", "/%2e%2e/secret.txt", "/", "//etc/passwd", "/secret.txt",
		"/" + strings.ToUpper(name), "/" + name + "/../" + name,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://x"+p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%q -> %d %q", p, w.Code, w.Body.String())
		}
	}
	for _, rng := range []string{"bytes=0-99999999999999999999", "bytes=-9223372036854775808", "bytes=18446744073709551615-"} {
		req := httptest.NewRequest(http.MethodGet, "http://x/"+name, nil)
		req.Header.Set("Range", rng)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK && w.Code != http.StatusRequestedRangeNotSatisfiable && w.Code != http.StatusPartialContent {
			t.Errorf("Range %q -> %d", rng, w.Code)
		}
		if strings.Contains(w.Body.String(), "nope") {
			t.Errorf("Range %q read a different file", rng)
		}
	}
}
