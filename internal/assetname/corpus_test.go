package assetname

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// The corpus is REAL: `testdata/real-assets.txt` holds asset names scraped from
// the latest release of 9 well-known GitHub repositories, each with its own TAG=.
//
// This file exists because the fixtures we invent ourselves
// (`tool_1.2.0_linux_amd64.tar.gz`, version followed immediately by `_`) CAN
// NEVER HIT the shape that is most common in the real world —
// `ripgrep-15.2.0-aarch64-apple-darwin.tar.gz`, where the version is followed by
// `-` and an architecture name. That one shape pointed the [artifact] section of
// 3 out of 4 real repositories at files that do not exist, with every unit test
// green. ADD A REAL ASSET NAME TO THE CORPUS BEFORE YOU ADD A NEW RULE.

type corpusRepo struct {
	Repo, Tag string
	Files     []string
}

func loadCorpus(t *testing.T) map[string]corpusRepo {
	t.Helper()
	data, err := os.ReadFile("testdata/real-assets.txt")
	if err != nil {
		t.Fatalf("cannot read the corpus: %v", err)
	}
	out := map[string]corpusRepo{}
	cur := ""
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "### "):
			cur = strings.TrimSpace(line[4:])
			out[cur] = corpusRepo{Repo: cur}
		case strings.HasPrefix(line, "TAG="):
			r := out[cur]
			r.Tag = strings.TrimSpace(line[4:])
			out[cur] = r
		case strings.TrimSpace(line) != "":
			r := out[cur]
			r.Files = append(r.Files, strings.TrimSpace(line))
			out[cur] = r
		}
	}
	if len(out) == 0 {
		t.Fatal("the corpus is empty")
	}
	return out
}

func inferRepo(t *testing.T, repo string) Result {
	t.Helper()
	r, ok := loadCorpus(t)[repo]
	if !ok {
		t.Fatalf("the corpus has no %s", repo)
	}
	return InferTag(r.Files, r.Tag)
}

// Every case pins the whole template, not just "does it have {version}": the
// symptom of this bug is precisely a template that does have {version} and looks
// perfectly fine, only with an architecture segment missing from the middle.
func TestCorpusArtifacts(t *testing.T) {
	cases := []struct {
		repo string
		want map[string]string
	}{{
		// Rust target triple with `-aarch64` right after the version: this is the
		// case that swallowed `15.2.0-aarch64` as the whole version, and after
		// substitution the aarch64 was gone.
		repo: "BurntSushi/ripgrep",
		want: map[string]string{
			"darwin/amd64":     "ripgrep-{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "ripgrep-{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/musl": "ripgrep-{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "ripgrep-{version}-aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm64/musl": "ripgrep-{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/armv7":      "ripgrep-{version}-armv7-unknown-linux-gnueabihf.tar.gz",
		},
	}, {
		// goreleaser mixing `-` and `_`: the segment that goes missing is `linux`.
		repo: "junegunn/fzf",
		want: map[string]string{
			"linux/amd64":  "fzf-{version}-linux_amd64.tar.gz",
			"linux/arm64":  "fzf-{version}-linux_arm64.tar.gz",
			"linux/armv6":  "fzf-{version}-linux_armv6.tar.gz",
			"linux/armv7":  "fzf-{version}-linux_armv7.tar.gz",
			"darwin/amd64": "fzf-{version}-darwin_amd64.tar.gz",
			"darwin/arm64": "fzf-{version}-darwin_arm64.tar.gz",
		},
	}, {
		// The run-together `-v` in `fd-v10.5.0-`: the v has to stay in the template
		// ({version} carries no v) and must not be taken for part of the tool name
		// (see TestCorpusNames below).
		repo: "sharkdp/fd",
		want: map[string]string{
			"darwin/amd64":     "fd-v{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "fd-v{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/gnu":  "fd-v{version}-x86_64-unknown-linux-gnu.tar.gz",
			"linux/amd64/musl": "fd-v{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "fd-v{version}-aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm64/musl": "fd-v{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/arm":        "fd-v{version}-arm-unknown-linux-gnueabihf.tar.gz",
			// i686 = 32-bit Linux. Without that line in archWords, these two get
			// silently dropped.
			"linux/386/gnu":  "fd-v{version}-i686-unknown-linux-gnu.tar.gz",
			"linux/386/musl": "fd-v{version}-i686-unknown-linux-musl.tar.gz",
		},
	}, {
		// The asset names carry NO VERSION AT ALL (the cargo-dist shape; yq looks
		// like this too): producing no {version} is correct, and the template is
		// naturally stable across releases.
		repo: "eza-community/eza",
		want: map[string]string{
			"linux/amd64/gnu":  "eza_x86_64-unknown-linux-gnu.tar.gz",
			"linux/amd64/musl": "eza_x86_64-unknown-linux-musl.tar.gz",
			"linux/arm64/gnu":  "eza_aarch64-unknown-linux-gnu.tar.gz",
			"linux/arm":        "eza_arm-unknown-linux-gnueabihf.tar.gz",
		},
	}, {
		// THE ANDROID TRAP: `aarch64-linux-android` /
		// `armv7-linux-androideabi` contain the word `linux`, and
		// first-match-wins calls them linux/arm64 and linux/armv7 — while the
		// armv7 key should hold armv7-unknown-linux-musleabihf. That is a bionic
		// build STEALING THE REAL LINUX BUILD'S SLOT. The symptom is a download
		// URL that returns 200, bytes that are intact, and Exec format error at
		// run time.
		repo: "ajeetdsouza/zoxide",
		want: map[string]string{
			"darwin/amd64":     "zoxide-{version}-x86_64-apple-darwin.tar.gz",
			"darwin/arm64":     "zoxide-{version}-aarch64-apple-darwin.tar.gz",
			"linux/amd64/musl": "zoxide-{version}-x86_64-unknown-linux-musl.tar.gz",
			"linux/arm":        "zoxide-{version}-arm-unknown-linux-musleabihf.tar.gz",
			"linux/arm64/musl": "zoxide-{version}-aarch64-unknown-linux-musl.tar.gz",
			"linux/armv7":      "zoxide-{version}-armv7-unknown-linux-musleabihf.tar.gz",
			"linux/386/musl":   "zoxide-{version}-i686-unknown-linux-musl.tar.gz",
		},
	}, {
		// BARE BINARIES (no archive suffix). install supports the single-file
		// form, so these have to be recognized. `jq-1.8.2.tar.gz` in the same
		// release is SOURCE CODE: it carries no os/arch word, so it is never
		// taken for an asset — putting a source tarball on PATH would be a
		// disaster.
		repo: "jqlang/jq",
		want: map[string]string{
			"linux/amd64":  "jq-linux-amd64",
			"linux/arm64":  "jq-linux-arm64",
			"linux/armv7":  "jq-linux-armhf",
			"linux/386":    "jq-linux-i386",
			"darwin/amd64": "jq-macos-amd64", // the macos alias
			"darwin/arm64": "jq-macos-arm64",
		},
	}}

	for _, c := range cases {
		res := inferRepo(t, c.repo)
		for plat, want := range c.want {
			if got := res.Artifact[plat]; got != want {
				t.Errorf("%s [%s] = %q, want %q", c.repo, plat, got, want)
			}
		}
		for plat, got := range res.Artifact {
			if _, ok := c.want[plat]; !ok {
				t.Errorf("%s has an extra platform [%s] = %q", c.repo, plat, got)
			}
		}
	}
}

// Source tarballs sit in the same release (jq-1.8.2.tar.gz / .zip). They carry
// no os/arch word, so they cannot enter [artifact] to begin with — this
// assertion is here to stop some future change to bare-binary recognition from
// letting them in.
func TestCorpusNeverInstallsASourceTarball(t *testing.T) {
	res := inferRepo(t, "jqlang/jq")
	for plat, tmpl := range res.Artifact {
		if strings.HasPrefix(tmpl, "jq-1.8.2") {
			t.Errorf("[%s] points at the source tarball %q", plat, tmpl)
		}
	}
}

// Companion files (checksums, signatures, .deb/.rpm, third-party installers,
// attestations) share a name and prefix with the real asset and contain every
// os/arch word it does. Fail to filter them out BEFORE platform detection and
// they compete with the real asset for the same platform key (.deb often sorts
// before .tar.gz, and winning means an entry that installs a deb package);
// even when they lose, each one reports a line that drowns out the one worth
// reading.
func TestCorpusIgnoresCompanionFiles(t *testing.T) {
	noisy := []string{".sha256", ".deb", ".rpm", ".msi", ".pkg", "attestation.json", "checksums.txt", "sha256sum.txt"}
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		var seen []string
		for _, a := range res.Assets {
			seen = append(seen, a.File)
		}
		for _, s := range res.Skipped {
			seen = append(seen, "(reported) "+s.File)
		}
		for _, f := range seen {
			for _, n := range noisy {
				if strings.HasSuffix(strings.ToLower(strings.TrimPrefix(f, "(reported) ")), n) {
					t.Errorf("%s: companion file %s must not show up in the result", repo, f)
				}
			}
		}
	}
}

// When a corpus asset name carries a version, the template must not keep it —
// keeping it means an entry pinned to this one release that 404s the moment
// upstream ships a new one. Conversely, names that never carried a version
// (eza) must not be reported as anomalies.
func TestCorpusLeavesNoVersionBehind(t *testing.T) {
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		if res.Unreplaced != "" {
			t.Errorf("%s left the version %q in its templates", repo, res.Unreplaced)
		}
		for _, a := range res.Assets {
			if res.Version != "" && strings.Contains(a.Template, res.Version) {
				t.Errorf("%s: %q still contains %q", repo, a.Template, res.Version)
			}
		}
	}
	// eza's asset names carry no version, so there should be neither a {version}
	// nor any complaint.
	eza := inferRepo(t, "eza-community/eza")
	if eza.Unreplaced != "" {
		t.Errorf("eza's asset names never had a version; reporting %q is wrong", eza.Unreplaced)
	}
	for _, a := range eza.Assets {
		if strings.Contains(a.Template, "{version}") {
			t.Errorf("eza must not produce {version}: %q", a.Template)
		}
	}
}

// The tool name is the command name on the left of [bin] — get it wrong as
// `fd-v` and what lands on PATH after install is a command called fd-v.
func TestCorpusNames(t *testing.T) {
	for repo, want := range map[string]string{
		"BurntSushi/ripgrep": "ripgrep",
		"junegunn/fzf":       "fzf",
		"sharkdp/fd":         "fd",
		"sharkdp/bat":        "bat",
		"cli/cli":            "gh",
		"jqlang/jq":          "jq",
	} {
		if got := inferRepo(t, repo).Name; got != want {
			t.Errorf("%s name = %q, want %q", repo, got, want)
		}
	}
}

// The tag is the known answer; the regex is only a guess — do not guess when
// the answer is known.
//
// The corpus alone cannot test this (with the regex tightened it produces the
// right version there too), so this uses a shape the regex is GUARANTEED to get
// wrong: `-alpha` is a legitimate prerelease spelling, but this batch's tag is
// v1.2.3 and `alpha` is just a segment of the filename. The regex swallows
// `1.2.3-alpha` as the whole version, the template loses `-alpha`, and install
// expanding {version} = 1.2.3 gets a 404.
func TestKnownTagBeatsTheRegex(t *testing.T) {
	files := []string{
		"tool-1.2.3-alpha-x86_64-unknown-linux-gnu.tar.gz",
		"tool-1.2.3-alpha-aarch64-apple-darwin.tar.gz",
	}
	got := InferTag(files, "v1.2.3").Artifact["linux/amd64/gnu"]
	if want := "tool-{version}-alpha-x86_64-unknown-linux-gnu.tar.gz"; got != want {
		t.Errorf("template = %q, want %q", got, want)
	}
	if v := InferTag(files, "v1.2.3").Version; v != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3 (the tag wins)", v)
	}
}

// A tag is only substituted literally when it appears AS A WHOLE NUMERIC
// SEGMENT. The tag `v2.0` matches inside `tool-2.0.1-…`, and the resulting
// `tool-{version}.1-…` is a template nobody can install.
func TestKnownTagNeedsAWholeMatch(t *testing.T) {
	files := []string{"tool-2.0.1-x86_64-unknown-linux-gnu.tar.gz"}
	got := InferTag(files, "v2.0").Artifact["linux/amd64/gnu"]
	if want := "tool-{version}-x86_64-unknown-linux-gnu.tar.gz"; got != want {
		t.Errorf("template = %q, want %q (the regex should find 2.0.1, not let 2.0 bite in)", got, want)
	}
}

// A package-name-prefixed tag (jq-1.8.2) must not be substituted literally:
// install expands {version} to `TrimPrefix(tag,"v")`, so substituting back
// would lose the `jq-` part.
func TestKnownTagRejectsAPackageNamePrefix(t *testing.T) {
	res := inferRepo(t, "jqlang/jq")
	for _, a := range res.Assets {
		if strings.Contains(a.Template, "{version}") {
			t.Errorf("a tag like jq-1.8.2 must not be used for substitution: %q", a.Template)
		}
	}
}

// Prerelease tags and architecture names look identical; the test is an
// allowlist (preWords), not "exclude the os/arch words".
func TestTrimFalsePre(t *testing.T) {
	for in, want := range map[string]string{
		"15.2.0-aarch64": "15.2.0",
		"0.74.3-linux":   "0.74.3",
		"0.10.0-riscv64": "0.10.0",
		"15.2.0-s390x":   "15.2.0",
		"15.2.0-x86":     "15.2.0", // what is left of x86_64 after '_' splits it
		"1.0.0-rc.1":     "1.0.0-rc.1",
		"1.0.0-rc1":      "1.0.0-rc1",
		"1.0.0-beta.2":   "1.0.0-beta.2",
		"1.0.0-alpha":    "1.0.0-alpha",
		"1.2.3+20240101": "1.2.3+20240101", // build metadata is part of the tag; do not trim
		"1.2.3":          "1.2.3",
	} {
		if got := trimFalsePre(in); got != want {
			t.Errorf("trimFalsePre(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every repository in the corpus has to actually produce something — one that
// recognizes 0 platforms is one we cannot index.
func TestCorpusEveryRepoYieldsSomething(t *testing.T) {
	for repo, r := range loadCorpus(t) {
		res := InferTag(r.Files, r.Tag)
		if len(res.Artifact) == 0 {
			t.Errorf("%s recognized no platform at all", repo)
		}
		plats := make([]string, 0, len(res.Artifact))
		for p := range res.Artifact {
			plats = append(plats, p)
		}
		sort.Strings(plats)
		t.Logf("%s (%s) -> %s", repo, r.Tag, strings.Join(plats, " "))
	}
}

// Unreplaced is what drives the "404 the moment upstream ships a new version"
// warning — it has to actually fire. The scenario: the version given does not
// match the one in the asset names (a typo in --version, or a tag that
// disagrees with the asset names).
func TestUnreplacedFiresWhenAVersionIsLeftBehind(t *testing.T) {
	res := Infer([]string{"tool-1.2.3-x86_64-unknown-linux-gnu.tar.gz"}, "9.9.9")
	if res.Unreplaced != "1.2.3" {
		t.Fatalf("Unreplaced = %q, want 1.2.3", res.Unreplaced)
	}
}
