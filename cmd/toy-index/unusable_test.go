package main

import "testing"

// Both of these were found by installing every index entry, not by reading code,
// and both got as far as a symlink on PATH before anything noticed.
func TestFormatsWeCannotUnpackAreNeverTakenForBareBinaries(t *testing.T) {
	// restic ships restic_0.18.1_linux_amd64.bz2. It is not an archive we handle,
	// the platform in the name reads fine, and the old blocklist let it fall
	// through to "must be a bare binary then" -- so a bzip2 stream was chmod +x'd
	// and the shell said `cannot execute binary file: Exec format error`.
	for _, bad := range []string{
		"tool_1.0.0_linux_amd64.xz",
		"tool_1.0.0_linux_amd64.zst",
		"tool_1.0.0_linux_amd64.7z",
		"tool-linux-amd64.tar.xz",
		"tool_1.0.0_linux_amd64.deb",
	} {
		if installable(bad) {
			t.Errorf("installable(%q) = true; we cannot unpack it and it is not a runnable binary", bad)
		}
	}
	// And the formats that really are fine still are.
	// .bz2 and a bare .gz joined this list once compress/bzip2 and compress/gzip
	// were wired up -- both are standard library, so they cost no dependency.
	for _, ok := range []string{
		"tool_1.0.0_linux_amd64.tar.gz", "tool_1.0.0_linux_amd64.tgz",
		"tool_1.0.0_linux_amd64.zip", "jq-linux-amd64",
		"restic_0.19.1_linux_amd64.bz2", "tool_1.0.0_linux_amd64.gz",
		"tool_1.0.0_linux_amd64.tar.bz2",
	} {
		if !installable(ok) {
			t.Errorf("installable(%q) = false; this one we handle", ok)
		}
	}
}

// The named list will never be complete. .flatpak was missing until goreleaser
// shipped one, and it was installed as a bare binary -- exactly the way restic's
// .bz2 had been one round earlier. So the real test is the other way round: a bare
// binary in the wild has no extension at all, and anything ending in something
// extension-shaped is a format we simply have not recognized.
func TestAnUnknownExtensionIsRefusedRatherThanAssumedExecutable(t *testing.T) {
	for _, bad := range []string{
		"goreleaser_2.18.0_linux_amd64.flatpak",
		"tool_1.0.0_linux_amd64.nupkg",
		"tool_1.0.0_linux_amd64.jar",
		"tool-1.0.0-1.x86_64.rpm",
		"tool_1.0.0_x86_64.apk",
		"tool-1.0.0-1-x86_64.pkg.tar.zst",
	} {
		if installable(bad) {
			t.Errorf("installable(%q) = true; an extension we do not recognize is not a binary", bad)
		}
	}
	// And the shapes a real bare binary takes stay installable: no extension at
	// all, or a tail that is a version rather than a format.
	for _, ok := range []string{
		"jq-linux-amd64", "yq_linux_amd64", "jq-macos-arm64",
		"caddy_2.7.6_linux_amd64", "tool-1.2.3-linux-amd64",
	} {
		if !installable(ok) {
			t.Errorf("installable(%q) = false; that is what a bare binary looks like", ok)
		}
	}
}
