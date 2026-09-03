package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The original accusation (since fixed; this is now the regression test):
// host.Tags ran git via exec.CommandContext(ctx,"git",…), i.e. **by name**,
// resolved off the calling process's PATH. But ~/.toy/bin is both the directory
// packages can write symlinks into and the one doctor tells users to put first on
// PATH — so a toy.toml with [bin] git = "payload" claims that name, nothing runs
// at install time (iron law 1 holds to the letter), and the very next toy command
// executes it as git.
//
// The fix has two halves: the internal/store half refuses to link a package as
// `git` (not even --force gets through); this half is lookupGit — walk PATH
// ourselves, resolve an absolute path, and **skip ~/.toy/bin**.
// Both halves are required: store's only stops newly installed packages, this one
// also refuses to run a git that is already sitting in bin/.
//
// Note that "a fake git anywhere else on PATH gets executed" is out of scope —
// that is just what PATH means, and the user's own PATH is the user's business.
// The dangerous one is the directory **packages can write to**.
func TestTagsRefusesGitFromToyBinDir(t *testing.T) {
	// bin/ is the only git on PATH, arranged first the way doctor tells users to.
	t.Run("only the one in bin", func(t *testing.T) {
		binDir, marker := toyBinWithFakeGit(t)
		t.Setenv("PATH", binDir)

		_, err := Tags(context.Background(), "https://example.invalid/o/r")

		mustNotHaveRun(t, marker, filepath.Join(binDir, "git"))
		if err == nil {
			t.Fatal("want an error")
		}
		// Iron law 8: no vague "git not found". The user has to know this is not a
		// broken environment.
		if !strings.Contains(err.Error(), "attack") || !strings.Contains(err.Error(), binDir) {
			t.Errorf("error does not say this is a likely attack, nor which file it is: %v", err)
		}
	})

	// A real git sits behind it: that is the one that must run. This pins down
	// "resolve to an absolute path, then exec" — exec by name still puts the fake
	// from bin/ first on PATH.
	t.Run("bin/ ahead of the real git", func(t *testing.T) {
		binDir, marker := toyBinWithFakeGit(t)
		real := t.TempDir()
		script := "#!/bin/sh\necho \"deadbee\trefs/tags/v1.0.0\"\n"
		if err := os.WriteFile(filepath.Join(real, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+real)

		tags, err := Tags(context.Background(), "https://example.invalid/o/r")

		mustNotHaveRun(t, marker, filepath.Join(binDir, "git"))
		if err != nil {
			t.Fatalf("Tags failed: %v", err)
		}
		if len(tags) != 1 || tags[0] != "v1.0.0" {
			t.Fatalf("tags = %#v — that is not the real git in %s", tags, real)
		}
	})
}

// toyBinWithFakeGit builds a TOY_HOME whose bin/ holds a fake git that leaves a
// trace when it runs — exactly what a package installed via [bin] git = "payload"
// would look like.
func toyBinWithFakeGit(t *testing.T) (binDir, marker string) {
	t.Helper()
	home := t.TempDir()
	binDir = filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(home, "it-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOY_HOME", home)
	return binDir, marker
}

func mustNotHaveRun(t *testing.T, marker, fake string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("toy executed %s — a file some package installed into bin/ (iron law 1)", fake)
	}
}

// A fake git lying in bin/ must not take toy out entirely — skip it and keep
// looking for the real one.
func TestLookupGitSkipsToyBinAndKeepsLooking(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	real := t.TempDir()
	for _, d := range []string{binDir, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+real)

	got, err := lookupGit()
	if err != nil {
		t.Fatalf("lookupGit failed: %v", err)
	}
	if want := filepath.Join(real, "git"); got != want {
		t.Fatalf("lookupGit = %q, want %q (the one in bin/ must be skipped)", got, want)
	}
}

// Relative PATH entries ("." and that ubiquitous empty one) mean the directory the
// toy process happens to be in. Running toy from a directory someone else can write
// to must not hand away iron law 1.
func TestLookupGitSkipsRelativePATHEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("TOY_HOME", t.TempDir())
	t.Setenv("PATH", "."+string(os.PathListSeparator)) // "." and the empty entry, both spellings

	if got, err := lookupGit(); err == nil {
		t.Fatalf("lookupGit = %q — that git was picked up from the current directory", got)
	}
}
