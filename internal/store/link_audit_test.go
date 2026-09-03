package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Audit (iron law 5's blind spot): the shadow check only blocks a program of the
// same name that ALREADY EXISTS on PATH. But toy itself execs a name resolved off
// PATH: git (internal/host.Tags uses exec.CommandContext(ctx,"git",…), and so do
// autGitRemote and doctor).
//
// On a machine with no git installed — macOS without the Xcode CLT, a slim
// container, a CI base image, all common — LookPath("git") finds nothing, so it
// does not count as shadowing, so a toy.toml with [bin] git = "payload" claims
// ~/.toy/bin/git. And ~/.toy/bin is exactly the directory toy doctor tells the
// user to put first on PATH. The next `toy install github:…` / `toy update` /
// `toy doctor` executes it.
func TestLinkLetsAPackageClaimTheNameToyItselfExecutes(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// A machine with no git.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	pkg := filepath.Join(s.Root, "pkg", "evil")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(pkg, "payload")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	link, err := s.Link("git", payload, false) // note: no --force
	if err != nil {
		return // fixed: toy refuses to hand out a name it execs itself
	}
	t.Fatalf("created %s without --force — it points at %s inside the package, "+
		"and the \"git\" toy execs is resolved off PATH (with BinDir usually first). "+
		"The next `toy install github:…` runs this file (iron law 1)", link, payload)
}

// --force must not waive it either: bin = "git" is not the same class of risk as
// bin = "sudo". Staring at "pass --force if you really mean it", a user cannot
// work out the consequence.
func TestForceCannotClaimTheNameToyItselfExecutes(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(s.Root, "pkg", "evil")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(pkg, "payload")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "GIT"} {
		if link, err := s.Link(name, payload, true); err == nil {
			t.Fatalf("--force created %s — the git toy execs is resolved off PATH", link)
		}
	}
}
