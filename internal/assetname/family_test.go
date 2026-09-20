package assetname

import "testing"

// atuin ships two programs in one release: atuin-<triple> (the CLI) and
// atuin-server-<triple> (the server), and not both for every platform. They never
// collide on a platform key -- they divide the keys up -- so nothing noticed, and
// the entry that came out installed the server on linux/amd64 and the CLI on
// darwin/arm64 under one [bin] line that could only be right for one of them.
//
// One entry that installs a different program depending on the machine is worse
// than one that does not install at all, and it stayed invisible until the index
// was installed on a second platform.
func TestTwoProgramsInOneReleaseDoNotShareAnEntry(t *testing.T) {
	r := InferTag([]string{
		"atuin-aarch64-apple-darwin.tar.gz",
		"atuin-aarch64-unknown-linux-gnu.tar.gz",
		"atuin-x86_64-unknown-linux-gnu.tar.gz",
		"atuin-server-x86_64-apple-darwin.tar.gz",
		"atuin-server-x86_64-unknown-linux-gnu.tar.gz",
	}, "v18.22.0")
	for plat, asset := range r.Artifact {
		if got := familyStem(asset); got != "atuin" {
			t.Errorf("%s = %s (family %q); every platform must carry the same program", plat, asset, got)
		}
	}
	if len(r.Artifact) != 3 {
		t.Errorf("kept %d platforms, want the 3 belonging to the CLI: %v", len(r.Artifact), r.Artifact)
	}
	// And the ones dropped have to say why, or the author cannot tell that the
	// other program exists.
	var said bool
	for _, s := range r.Skipped {
		if s.Notable && s.Skip != "" {
			said = true
		}
	}
	if !said {
		t.Error("dropping half a release must be reported, not silent")
	}
}

// The ordinary case must not change: musl and gnu come after the arch word, so
// every variant a normal project ships is one family.
func TestVariantsOfOneProgramStayTogether(t *testing.T) {
	r := InferTag([]string{
		"rg-x86_64-unknown-linux-gnu.tar.gz",
		"rg-x86_64-unknown-linux-musl.tar.gz",
		"rg-aarch64-apple-darwin.tar.gz",
		"rg-x86_64-apple-darwin.tar.gz",
	}, "v15.2.0")
	if len(r.Artifact) != 4 {
		t.Fatalf("kept %d of 4 platforms; these are all the same program: %v", len(r.Artifact), r.Artifact)
	}
}

// chezmoi ships the same program two ways: bare binaries (chezmoi-linux-amd64) and
// tarballs (chezmoi_0.20.0_linux_arm64.tar.gz). Without trimming the version off
// the stem the second reads as an add-on of the first, and the entry would lose
// three platforms on the next regeneration.
func TestTwoPackagingShapesOfOneProgramAreOneFamily(t *testing.T) {
	r := InferTag([]string{
		"chezmoi-darwin-amd64", "chezmoi-linux-amd64", "chezmoi-linux-amd64-musl",
		"chezmoi_0.20.0_linux_i386.tar.gz", "chezmoi_0.20.0_linux_arm64.tar.gz",
		"chezmoi_0.20.0_linux_armv6.tar.gz",
	}, "v0.20.0")
	if len(r.Artifact) != 6 {
		t.Fatalf("kept %d of 6 platforms; all of these are chezmoi: %v", len(r.Artifact), r.Artifact)
	}
}
