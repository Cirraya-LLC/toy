package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// Filename inference itself lives in internal/assetname, and so do its unit
// tests (the Classify and Infer tables). This file only tests the cmd layer:
// how an inference result turns into a parsable toy.toml, how it lands on
// disk, and the two regressions that only mean anything if you run the WHOLE
// command.
//
// Do not copy assetname's table-driven cases back here. Copying them back
// gives you a second drifting test for a second drifting implementation --
// and killing the first one is exactly why that package was extracted.

func TestIniRenderProducesAParsableToyfile(t *testing.T) {
	assets := []iniAsset{
		{key: "linux/amd64", tmpl: "gadget_{version}_linux_amd64.tar.gz", sum: strings.Repeat("a", 64)},
		{key: "darwin/arm64", tmpl: "gadget_{version}_darwin_arm64.tar.gz", sum: strings.Repeat("b", 64)},
	}
	data, err := iniRender("gadget", assets)
	if err != nil {
		t.Fatalf("iniRender: %v", err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		t.Fatalf("the generated toy.toml does not parse: %v\n%s", err, data)
	}
	if tf.Name != "gadget" || tf.Bin["gadget"] != "gadget" {
		t.Fatalf("wrong name/bin: %+v", tf)
	}
	// Iron law 4: the files are sitting right there locally. If we can compute
	// the checksums we must write them -- skip this step and the whole package
	// has nothing but TOFU behind it.
	if tf.SHA256["linux/amd64"] == "" || tf.SHA256["darwin/arm64"] == "" {
		t.Fatalf("[sha256] is incomplete: %+v", tf.SHA256)
	}
	// Platform keys contain a slash, so a bare key will not do.
	if !strings.Contains(string(data), `"linux/amd64" = `) {
		t.Fatalf("platform keys must be quoted:\n%s", data)
	}
}

// The whole command end to end: build a dist, run init, check the file really
// lands and really parses.
func TestRunInitEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"gadget_3.0.1_linux_x86_64.tar.gz",
		"gadget_3.0.1_darwin_arm64.tar.gz",
		"gadget_3.0.1_windows_x86_64.zip",
		"checksums.txt",
	} {
		if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(t.Context(), []string{"--name", "gadget", dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		t.Fatalf("the toy.toml we wrote does not parse: %v\n%s", err, data)
	}
	if len(tf.Artifact) != 2 {
		t.Fatalf("want 2 platforms (Windows does not count), got %v", tf.Artifact)
	}
	if strings.Contains(string(data), "3.0.1") {
		t.Fatalf("the version was not replaced with {version}:\n%s", data)
	}
	// Never overwrite an existing file: the author may have hand-edited it.
	if err := runInit(t.Context(), []string{"--name", "gadget", dir}); err == nil {
		t.Fatal("a second init should refuse to overwrite")
	}
	if err := runInit(t.Context(), []string{"--force", "--name", "gadget", dir}); err != nil {
		t.Fatalf("--force should allow the overwrite: %v", err)
	}
}

// When nothing is recognized, never write an empty shell -- that is a file
// that looks like it worked and that nobody can install from.
func TestRunInitRefusesToWriteAnEmptyShell(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runInit(t.Context(), []string{dir})
	if err == nil {
		t.Fatal("recognizing no platform at all should be an error")
	}
	if !strings.Contains(err.Error(), "README") {
		t.Errorf("the error must say what it actually saw, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "toy.toml")); statErr == nil {
		t.Fatal("no toy.toml should have been written")
	}
}

// With GOARCH=arm and no goarm set, goreleaser's default name_template emits
// exactly `tool_1.2.3_linux_arm.tar.gz`. Bare "arm" was once missing from the
// mapping table, so that build got SILENTLY DROPPED: init printed "recognized
// N platforms", everything looked fine, and Raspberry Pi users hit "no build
// for this platform" at install time.
//
// linux/arm is not an optional key -- platform.go is what falls back to it for
// armv6/armv7 (spec 3.1). This runs the whole command instead of just calling
// the inference function: the missing link can just as easily be the cmd layer
// dropping it while turning results into [artifact], and that kind of loss is
// invisible in the inference unit tests.
func TestRunInitKeepsBareArm(t *testing.T) {
	tf := iniRunOnDist(t, []string{
		"tool_1.2.3_linux_arm.tar.gz",
		"tool_1.2.3_linux_armv6.tar.gz",
		"tool_1.2.3_linux_armv7.tar.gz",
		"tool_1.2.3_linux_amd64.tar.gz",
	})
	for _, want := range []string{"linux/arm", "linux/armv6", "linux/armv7", "linux/amd64"} {
		if tf.Artifact[want] == "" {
			t.Errorf("[artifact] is missing %s: %v", want, tf.Artifact)
		}
	}
}

// An ASSET we cannot recognize has to speak up. Dropping it silently looks
// exactly like the case above: the count looks right, and the missing platform
// waits for a user to report it. But checksums.txt / LICENSE were never assets
// to begin with, and warning about those would drown out the real one.
func TestRunInitReportsUnrecognizedAssetsButNotStrayFiles(t *testing.T) {
	files := []string{
		"tool_1.2.3_linux_amd64.tar.gz",
		"tool_1.2.3_freebsd_riscv64.tar.gz", // looks like an asset, unrecognized -> speak up
		"checksums.txt",                     // not an asset -> stay quiet
		"LICENSE",
		"README.md",
	}
	var lines []string
	for _, a := range assetnameSkipped(files) {
		lines = append(lines, a.File+": "+a.Skip)
	}
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "freebsd_riscv64") != 1 {
		t.Errorf("an unrecognized asset should be reported exactly once: %q", joined)
	}
	for _, quiet := range []string{"checksums.txt", "LICENSE", "README.md"} {
		if strings.Contains(joined, quiet) {
			t.Errorf("must not speak up about non-asset %s: %q", quiet, joined)
		}
	}
	// Speaking up is one thing; the platform we DID recognize still has to
	// land in [artifact].
	if tf := iniRunOnDist(t, files); len(tf.Artifact) != 1 {
		t.Errorf("want 1 platform, got %v", tf.Artifact)
	}
}

// iniRunOnDist builds a dist, runs runInit over it, and parses the toy.toml
// that comes out.
func iniRunOnDist(t *testing.T, files []string) *toyfile.Toyfile {
	t.Helper()
	dir := t.TempDir()
	dist := filepath.Join(dir, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(t.Context(), []string{"--name", "tool", dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		t.Fatalf("the toy.toml we wrote does not parse: %v\n%s", err, data)
	}
	return tf
}

// assetnameSkipped returns the skip list the cmd layer actually prints.
func assetnameSkipped(files []string) []assetname.Asset {
	return assetname.Infer(files, "").Skipped
}
