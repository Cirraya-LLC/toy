package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A bare binary (a release asset with no archive suffix, e.g. jq-linux-amd64 or
// yq_darwin_arm64) has to land at the path [bin] names, not under the asset name.
//
// The asset name varies per platform; [bin] is a single line that does not. Land
// it under the asset name and every platform except the one [bin] happens to match
// symlinks to a file that does not exist — and it blows up only after the download
// and the checksum both succeeded. That is exactly how mikefarah/yq in the index
// was broken on 5 of its 6 platforms.
func TestBareBinaryLandsAtTheBinTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	// The asset name deliberately differs from both sides of [bin], standing in
	// for "the name it would have been downloaded under on another platform".
	const asset = "tool_someotherplatform_v9"
	plat := runtime.GOOS + "/" + runtime.GOARCH
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = %q\n", plat, asset)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			w.Write([]byte(toml))
			return
		}
		w.Write([]byte("#!/bin/sh\necho ok\n"))
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	link := filepath.Join(home, "bin", "tool")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the symlink was not created: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink points at a file that does not exist, %s — the bare binary still landed under the asset name: %v", target, err)
	}
	if filepath.Base(target) != "tool" {
		t.Fatalf("it should have landed as the tool on the right of [bin], but it is %s", filepath.Base(target))
	}
}
