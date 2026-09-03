package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func audReinstallTGZ(t *testing.T, names []string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: 4})
		tw.Write([]byte("BODY"))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Audit: the rule in insPerform that "the download and TOFU are finished before
// anything on disk is touched, so a refusal partway through leaves the installed
// version untouched" only covers those two steps.
//
// The order in which it actually acts is: insRemovePkgDir(pkgDir) -> MkdirAll ->
// insUnpack. On a --force reinstall (and on the bare-URL path, which is always
// unversioned) pkgDir is the same directory as the installed version's, so one
// failed unpack destroys the copy already running on PATH — while the manifest
// is still there, `toy list` still lists it, and the symlink in bin/ now dangles.
func TestFailedReinstallWipesTheWorkingCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	good := audReinstallTGZ(t, []string{"tool"})
	bad := audReinstallTGZ(t, []string{"tool", "../../../../pwned"}) // unpacking hits ErrTraversal
	payload := good
	plat := runtime.GOOS + "/" + runtime.GOARCH
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = \"tool.tar.gz\"\n", plat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			w.Write([]byte(toml))
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		t.Fatalf("the first install already failed: %v", err)
	}
	link := filepath.Join(home, "bin", "tool")
	if b, err := os.ReadFile(link); err != nil || string(b) != "BODY" {
		t.Fatalf("wrong right after the first install: %v %q", err, b)
	}

	payload = bad
	err := runInstall(context.Background(), []string{"--force", srv.URL + "/toy.toml"})
	t.Logf("result of the reinstall: %v", err)
	if b, rerr := os.ReadFile(link); rerr != nil {
		t.Fatalf("a failed reinstall destroyed the working install: %s can no longer be read (%v), "+
			"while the manifest is still there and toy list still lists it", link, rerr)
	} else {
		t.Logf("still there: %q", b)
	}
}
