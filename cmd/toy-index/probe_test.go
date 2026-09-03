package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
)

// tarEntry is one member of a test archive. The execute bit in mode is the whole point of
// these tests.
type tarEntry struct {
	name string
	mode int64
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		body := []byte("x")
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: e.mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer pretends to be the release download URL: it hands back the same archive
// for any asset name.
func releaseServer(t *testing.T, tgz []byte) host.Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return host.Host{Release: srv.URL + "/{owner}/{repo}/releases/download/{tag}/{asset}"}
}

func probeGen(t *testing.T, tgz []byte, out string) ([]genResult, error) {
	t.Helper()
	api := fakeAPI(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve})
	cfg := genConfig{
		repos: []string{"Derrity/tinyserve"}, out: out, probe: true,
		prov: newGitHub(api.URL, ""), host: releaseServer(t, tgz),
	}
	idx := &index.Index{Version: index.Version}
	r := genOne(context.Background(), cfg, idx, "Derrity/tinyserve")
	return []genResult{r}, r.err
}

// The entire point of --probe: turn the right of [bin] from a guess into something read
// out of a real archive.
func TestProbeFindsTheRealBinPath(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"bin/tinyserve", 0o755},
		{"README.md", 0o644},
		{"completions/tinyserve.bash", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	p := rs[0].pack
	if !p.binProbed {
		t.Fatal("the report says [bin] is a guess, but it was probed")
	}
	if p.binPath != "bin/tinyserve" {
		t.Fatalf("[bin] = %q, the executable in the archive is at bin/tinyserve", p.binPath)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `tinyserve = "bin/tinyserve"`) {
		t.Fatalf("the probed path did not make it into the toyfile:\n%s", rs[0].entry.Toyfile)
	}
}

// The cargo-dist shape: the whole tree wrapped in one "version + target triple" directory.
// archive now strips that layer (a single top-level entry that is a directory), so the
// probed path is the stable `tinyserve` -- this case used to FAIL THE WHOLE ENTRY, and
// ripgrep / fd / bat / delta / eza all have this shape, meaning that entire block of
// modern Rust CLIs could not be listed at all.
//
// The probed path has to match the layout install actually ends up with: both sides go
// through archive.Extract, so whatever is probed here is what lands in the installer's
// pkg directory.
func TestProbeSeesThroughACargoDistTopLevelDir(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"tinyserve_1.2.3_linux_amd64/tinyserve", 0o755},
		{"tinyserve_1.2.3_linux_amd64/README.md", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("probe failed (that directory layer should have been stripped): %v", err)
	}
	if p := rs[0].pack; p.binPath != "tinyserve" || !p.binProbed {
		t.Fatalf("[bin] = %q (probed=%v), want tinyserve", p.binPath, p.binProbed)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `tinyserve = "tinyserve"`) {
		t.Fatalf("the probed path did not make it into the toyfile:\n%s", rs[0].entry.Toyfile)
	}
}

// When the layer cannot be stripped (more than one top-level entry), that guard still has
// to stop it: the right of [bin] is used verbatim and does not expand {version}, so a path
// pinned to 1.2.3 installs today and breaks the moment upstream cuts a release.
func TestProbeRefusesAVersionPinnedBinPath(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"tinyserve_1.2.3_linux_amd64/tinyserve", 0o755},
		{"LICENSE", 0o644}, // a second top-level entry -> nothing gets stripped
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err == nil {
		t.Fatal("should have failed: this path is pinned to 1.2.3")
	}
	msg := rs[0].err.Error()
	// The error must quote the real probed path -- whoever writes it by hand has the answer
	// right in front of them.
	if !strings.Contains(msg, "tinyserve_1.2.3_linux_amd64/tinyserve") {
		t.Errorf("the error does not give the probed path: %s", msg)
	}
	if !strings.Contains(msg, "version") {
		t.Errorf("the error does not say why: %s", msg)
	}
}

// Same for a directory name carrying a platform: a toy.toml has one [bin] line, shared by
// every platform.
func TestUnstableBinPathCatchesPlatformDirs(t *testing.T) {
	for _, tc := range []struct{ path, version, want string }{
		{"tinyserve", "1.2.3", ""},
		{"bin/tinyserve", "1.2.3", ""},
		{"tinyserve_1.2.3_linux_amd64/tinyserve", "1.2.3", "version"},
		{"tinyserve-linux-amd64/tinyserve", "", "platform name"},
		// When the version cannot be recognised (version is empty) we must not Contains on
		// an empty string -- that matches everything.
		{"bin/tinyserve", "", ""},
	} {
		got := unstableBinPath(tc.path, tc.version)
		if (tc.want == "") != (got == "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Errorf("unstableBinPath(%q, %q) = %q, want it to contain %q", tc.path, tc.version, got, tc.want)
		}
	}
}

// If it cannot be settled, say so; NEVER guess one. Guessing wrong shows up as a user
// whose new command on PATH is a completion script, while the report says "probed".
func TestPickExecutableRefusesToGuess(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, mode os.FileMode) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
	write("tinyserve", 0o755)
	write("helper", 0o755)
	write("README.md", 0o644)
	files := []string{"tinyserve", "helper", "README.md"}

	// One of them is named exactly like the command -> use it.
	if hit, cands := pickExecutable(dir, files, "tinyserve"); hit != "tinyserve" || cands != nil {
		t.Errorf("the same-named one should win: hit=%q cands=%v", hit, cands)
	}
	// None matches the name -> hand over the candidates, do not pick.
	hit, cands := pickExecutable(dir, files, "somethingelse")
	if hit != "" {
		t.Errorf("undecidable, yet it picked %q", hit)
	}
	if len(cands) != 2 {
		t.Errorf("the candidates should be those two executables, got %v", cands)
	}
	// Exactly one executable -> that is it, even if the name does not match (authors giving
	// the binary a different name is common).
	if hit, _ := pickExecutable(dir, []string{"helper", "README.md"}, "tinyserve"); hit != "helper" {
		t.Errorf("with a single executable it should just use it, got %q", hit)
	}
	// Not a single executable.
	if hit, cands := pickExecutable(dir, []string{"README.md"}, "tinyserve"); hit != "" || cands != nil {
		t.Errorf("with no executable it should hand back nothing: %q %v", hit, cands)
	}
}

// A failed probe is not a failure: the entry still comes out, [bin] is just still a guess
// -- but the report has to say so.
func TestProbeFailureStillProducesAnEntryButSaysSo(t *testing.T) {
	// The server only serves .tar.gz, so hand it garbage and let extraction fail.
	rs, err := probeGen(t, []byte("not a tarball"), filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("a failed probe must not fail the whole entry: %v", err)
	}
	p := rs[0].pack
	if p.binProbed {
		t.Fatal("the probe did not succeed, yet it claims it did")
	}
	if !strings.Contains(strings.Join(p.notes, "\n"), "probe failed") {
		t.Fatalf("the report does not mention the failed probe: %v", p.notes)
	}
}

func TestProbePickPrefersLinuxAmd64(t *testing.T) {
	art := map[string]string{"linux/arm64": "a", "linux/amd64": "b", "darwin/arm64": "c"}
	if plat, tmpl := probePick(art); plat != "linux/amd64" || tmpl != "b" {
		t.Errorf("picked %q/%q", plat, tmpl)
	}
	if plat, _ := probePick(map[string]string{"darwin/arm64": "c"}); plat != "" {
		t.Errorf("with no linux build it should give up on probing, picked %q", plat)
	}
}

// The BASENAME of the probed file is the command name the author actually ships.
//
// The default comes from the asset-name prefix, and the two are often not the same thing:
// BurntSushi/ripgrep's asset is called ripgrep-15.2.0-....tar.gz while the file inside is
// called rg. Taking the default writes `[bin] ripgrep = "rg"` -- it installs and runs,
// only the name on PATH appears in no documentation anywhere (nobody has ever written
// `ripgrep --version`). This failure is invisible in status codes and --probe's report
// says "probed" too; only reading both sides turns it up.
func TestProbedBasenameWinsOverTheAssetNamePrefix(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"ts", 0o755}, // the real name inside differs from the asset-name prefix tinyserve
		{"README.md", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	p := rs[0].pack
	if p.binName != "ts" {
		t.Fatalf("left of [bin] = %q, want ts (the basename of the file in the archive)", p.binName)
	}
	// The package/directory name still follows the repo and the asset name -- the only thing
	// that changed is the name put on PATH.
	if p.name != "tinyserve" {
		t.Errorf("name = %q, it should not follow along (that is the package directory name)", p.name)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `ts = "ts"`) {
		t.Fatalf("the [bin] in the toyfile is wrong:\n%s", rs[0].entry.Toyfile)
	}
	// Changing the name has to be said in the report: that column is the one look §14.9
	// rule 2 leaves for a person.
	var said bool
	for _, n := range p.notes {
		if strings.Contains(n, "ts") && strings.Contains(n, "tinyserve") {
			said = true
		}
	}
	if !said {
		t.Errorf("the command name changed but the notes do not say so: %v", p.notes)
	}
}

// The other direction: produce no noise when the basename matches the default. A note that
// fires every time is no note at all.
func TestProbedBasenameSaysNothingWhenItMatches(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{{"bin/tinyserve", 0o755}})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	p := rs[0].pack
	if p.binName != "tinyserve" {
		t.Fatalf("binName = %q, want tinyserve", p.binName)
	}
	for _, n := range p.notes {
		if strings.Contains(n, "command name taken from the probed file") {
			t.Errorf("the name did not change, yet it reported one: %s", n)
		}
	}
}
