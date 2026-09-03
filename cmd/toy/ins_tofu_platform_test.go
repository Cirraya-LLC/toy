package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/sign"
)

// TOFU pins have to be recorded per ASSET NAME, not per ref@version.
//
// Why: the same ref@version hits a different candidate on a musl machine than on
// a glibc one (linux/amd64/musl vs linux/amd64), which means two completely
// different sets of bytes. With a shared $HOME (an NFS home, dotfiles syncing
// ~/.toy), the second machine gets told "the content was swapped out" — a
// terrifying false alarm, landing in exactly the place you can least afford to
// cry wolf.
//
// An asset name is already a platform discriminator, so appending it is enough;
// no need to invent a "platform" concept.
func TestTOFUPinsPerAssetNotPerVersion(t *testing.T) {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	asset := "tool_gnu.tar.gz"
	body := audReinstallTGZ(t, []string{"tool"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			fmt.Fprintf(w, "name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = %q\n", plat, asset)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	ref := srv.URL + "/toy.toml"
	if err := runInstall(context.Background(), []string{ref}); err != nil {
		t.Fatalf("the first install failed: %v", err)
	}

	// Change the asset name and the bytes: "same package, another machine with a
	// different libc".
	asset = "tool_musl.tar.gz"
	body = audReinstallTGZ(t, []string{"tool", "extra"})
	if err := runInstall(context.Background(), []string{"--reinstall", ref}); err != nil {
		if errors.Is(err, sign.ErrChanged) {
			t.Fatalf("a different asset name (i.e. a different platform) was misreported as tampering — "+
				"the TOFU id does not include the asset name: %v", err)
		}
		t.Fatalf("install failed: %v", err)
	}

	// The other direction: same asset name, different bytes. THAT is the one to stop.
	body = audReinstallTGZ(t, []string{"tool", "another"})
	err := runInstall(context.Background(), []string{"--reinstall", ref})
	if !errors.Is(err, sign.ErrChanged) {
		t.Fatalf("same asset name with different bytes must report ErrChanged, got: %v", err)
	}
}
