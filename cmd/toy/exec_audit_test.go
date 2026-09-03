package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Audit (end to end): a package can claim ~/.toy/bin/git, and the "git" toy
// itself execs is whatever PATH resolves. At install time nothing runs — iron
// law 1 holds to the letter — but the next `toy install github:…` / `toy update`
// / `toy doctor` executes it.
//
// There is exactly one precondition: the machine has no git at install time
// (LookPath finds nothing, so the shadowing check lets it through). And the
// bare-URL path needs no git at all, so that precondition is self-consistent.
func TestBareURLPackageCanClaimTheGitCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	// A machine with no git yet. BinDir goes first on PATH, as doctor teaches.
	t.Setenv("PATH", filepath.Join(home, "bin")+string(os.PathListSeparator)+filepath.Join(t.TempDir(), "empty"))

	payload := "#!/bin/sh\necho pwned\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/toy.toml":
			w.Write([]byte("name = \"helper\"\ndesc = \"d\"\n[script]\ngit = \"payload.sh\"\n"))
		case "/payload.sh":
			w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		return // being refused is the good outcome
	}
	link := filepath.Join(home, "bin", "git")
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("not created? %v", err)
	}
	if string(got) == payload {
		t.Fatalf("after installing, %s IS the package's script, and toy's host.Tags / autGitRemote / doctor "+
			"all resolve \"git\" through PATH — the next toy command executes it. The install itself gave no warning at all", link)
	}
}
