package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// fakeRepo is everything the fake API knows about one repo.
type fakeRepo struct {
	desc, license, homepage string
	stars                   int
	tag                     string
	assets                  []string
	missing                 bool // the repo itself 404s
	noRelease               bool // the repo exists, but releases/latest 404s
	rateLimited             bool // 403 with 0 remaining quota
}

// fakeAPI stands up a fake GitHub API. Every test goes through it; nothing hits the network.
func fakeAPI(t *testing.T, repos map[string]fakeRepo) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/repos/")
		latest := strings.HasSuffix(p, "/releases/latest")
		id := strings.TrimSuffix(p, "/releases/latest")

		repo, ok := repos[id]
		w.Header().Set("X-RateLimit-Limit", "60")
		switch {
		case !ok || repo.missing:
			w.Header().Set("X-RateLimit-Remaining", "59")
			w.WriteHeader(http.StatusNotFound)
			return
		case repo.rateLimited:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "59")

		if !latest {
			json.NewEncoder(w).Encode(map[string]any{
				"description":      repo.desc,
				"homepage":         repo.homepage,
				"stargazers_count": repo.stars,
				"license":          map[string]any{"spdx_id": repo.license},
			})
			return
		}
		if repo.noRelease {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assets := make([]map[string]any, 0, len(repo.assets))
		for _, a := range repo.assets {
			assets = append(assets, map[string]any{"name": a})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": repo.tag, "assets": assets})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runFakeGen(t *testing.T, repos map[string]fakeRepo, list []string, out string, update bool) error {
	t.Helper()
	srv := fakeAPI(t, repos)
	return gen(context.Background(), genConfig{
		repos: list, out: out, update: update,
		prov: newGitHub(srv.URL, ""),
		host: host.Host{Release: "https://example.invalid/{owner}/{repo}/{tag}/{asset}"},
	})
}

func loadOut(t *testing.T, path string) *index.Index {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the generated index: %v", err)
	}
	idx, skipped, err := index.Load(data, "out")
	if err != nil {
		t.Fatalf("the generated index does not Load: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("the generated index holds %d invalid records", skipped)
	}
	return idx
}

var tinyserve = fakeRepo{
	desc: "A very small static file server", license: "MIT", stars: 42, tag: "v1.2.3",
	assets: []string{
		"tinyserve_1.2.3_linux_amd64.tar.gz",
		"tinyserve_1.2.3_linux_arm64.tar.gz",
		"tinyserve_1.2.3_darwin_arm64.tar.gz",
		"tinyserve_1.2.3_windows_amd64.zip",
		"checksums.txt",
	},
}

// What comes out has to be an inline entry the index accepts, and it must NEVER carry
// [sha256]: inline entries follow the upstream's newest tag, so pinning one version's
// bytes turns the author's next release into "checksum mismatch, refusing to install",
// which reads exactly like a poisoning warning.
func TestGenProducesInlineEntryAndNeverWritesSHA256(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	if err := runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, false); err != nil {
		t.Fatalf("gen failed: %v", err)
	}

	idx := loadOut(t, out)
	e, ok := idx.Find("Derrity/tinyserve")
	if !ok {
		t.Fatal("the entry is not in the index")
	}
	if e.Source != "github:Derrity/tinyserve" {
		t.Errorf("source = %q", e.Source)
	}
	if e.Toyfile == "" {
		t.Fatal("the entry has no inline toyfile -- that is a dangling pointer nobody can install")
	}
	if e.Stars != 42 || e.Desc == "" {
		t.Errorf("stars/desc did not come along: %d %q", e.Stars, e.Desc)
	}
	if e.Key != "" || e.Sig != "" {
		t.Error("the generator must not conjure up a key/sig -- that checkmark means \"the author signed this\"")
	}

	// Non-comment lines only: the header comment literally says "there is deliberately no
	// [sha256]", and that sentence is not content.
	for _, line := range strings.Split(e.Toyfile, "\n") {
		if t2 := strings.TrimSpace(line); !strings.HasPrefix(t2, "#") && strings.Contains(t2, "sha256") {
			t.Fatalf("the inline entry writes sha256: %q -- the next upstream release hands everyone a checksum mismatch", line)
		}
	}
	tf, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex)
	if err != nil {
		t.Fatalf("the inline toyfile does not parse: %v", err)
	}
	if len(tf.SHA256) != 0 {
		t.Fatal("it parsed with a sha256 section after all")
	}
	// No Windows (spec §11); checksums.txt is not an asset.
	want := map[string]string{
		"linux/amd64":  "tinyserve_{version}_linux_amd64.tar.gz",
		"linux/arm64":  "tinyserve_{version}_linux_arm64.tar.gz",
		"darwin/arm64": "tinyserve_{version}_darwin_arm64.tar.gz",
	}
	if len(tf.Artifact) != len(want) {
		t.Fatalf("[artifact] = %v", tf.Artifact)
	}
	for k, v := range want {
		if tf.Artifact[k] != v {
			t.Errorf("[artifact] %s = %q, want %q", k, tf.Artifact[k], v)
		}
	}
	if tf.Bin["tinyserve"] != "tinyserve" {
		t.Errorf("[bin] = %v", tf.Bin)
	}
}

// Existing entries are not overwritten by default: they may have been hand-corrected
// (especially the right-hand side of [bin]).
func TestGenDoesNotOverwriteWithoutUpdate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	seed.Upsert(index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "hand-corrected description",
		Toyfile: "name = \"tinyserve\"\n\n[bin]\ntinyserve = \"nested/tinyserve\"\n\n" +
			"[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n",
	})
	data, _ := seed.Marshal()
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}

	repos := map[string]fakeRepo{"Derrity/tinyserve": tinyserve}
	if err := runFakeGen(t, repos, []string{"Derrity/tinyserve"}, out, false); err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	e, _ := loadOut(t, out).Find("Derrity/tinyserve")
	if !strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("a hand-corrected [bin] was clobbered without --update")
	}

	if err := runFakeGen(t, repos, []string{"Derrity/tinyserve"}, out, true); err != nil {
		t.Fatalf("gen --update failed: %v", err)
	}
	e, _ = loadOut(t, out).Find("Derrity/tinyserve")
	if strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("--update should have overwritten it and did not")
	}
}

// Signed entries are off limits even to --update: regenerating necessarily replaces the
// toyfile source, and the signature signs exactly that source -- overwriting quietly
// makes a "✓ signed" disappear.
func TestGenNeverTouchesSignedEntries(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	// Deliberately carries a toyfile too, so that only the "is signed" guard can save it --
	// otherwise the "do not downgrade pointer entries" guard would catch it first and this
	// test would not be testing what it claims to.
	seed.Upsert(index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve",
		Key: "a3f9", Sig: "sig", Desc: "signed inline entry",
		Toyfile: "name = \"tinyserve\"\n\n[bin]\ntinyserve = \"nested/tinyserve\"\n\n" +
			"[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n",
	})
	data, _ := seed.Marshal()
	os.WriteFile(out, data, 0o644)

	// Everything being deliberately skipped is not a failure (nothing errored), so this
	// exits 0.
	if err := runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, true); err != nil {
		t.Fatalf("a skip is not a failure: %v", err)
	}
	e, _ := loadOut(t, out).Find("Derrity/tinyserve")
	if e.Sig != "sig" || !strings.Contains(e.Toyfile, "nested/tinyserve") {
		t.Fatal("--update modified a signed entry -- regenerating would quietly invalidate that checkmark")
	}
}

// A pointer entry (upstream ships its own toy.toml) turning into an inline one is a
// TrustHost -> TrustIndexed downgrade, not a refresh. --update does not unlock it either.
func TestGenNeverDowngradesPointerEntries(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	seed := &index.Index{Version: index.Version}
	seed.Upsert(index.Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "pointer entry"})
	data, _ := seed.Marshal()
	os.WriteFile(out, data, 0o644)

	runFakeGen(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve},
		[]string{"Derrity/tinyserve"}, out, true)
	if e, _ := loadOut(t, out).Find("Derrity/tinyserve"); e.Toyfile != "" {
		t.Fatal("a pointer entry was downgraded into an inline entry")
	}
}

// One repo failing must not affect the others, and each kind of failure has to say
// something different in plain words.
func TestGenIsolatesFailuresAndClassifiesThem(t *testing.T) {
	repos := map[string]fakeRepo{
		"Derrity/tinyserve": tinyserve,
		"nobody/gone":       {missing: true},
		"nobody/norelease":  {noRelease: true},
		"nobody/sourceonly": {tag: "v1.0.0"},
		"nobody/bare":       {tag: "v1.0.0", assets: []string{"jq-linux-amd64", "jq-macos-arm64"}},
		"nobody/limited":    {rateLimited: true},
	}
	list := []string{"Derrity/tinyserve", "nobody/gone", "nobody/norelease",
		"nobody/sourceonly", "nobody/bare", "nobody/limited"}

	srv := fakeAPI(t, repos)
	out := filepath.Join(t.TempDir(), "idx.json")
	cfg := genConfig{repos: list, out: out, prov: newGitHub(srv.URL, "")}
	if err := gen(context.Background(), cfg); err != nil {
		t.Fatalf("one success means the batch must not fail as a whole: %v", err)
	}

	// A bare-binary repo (jqlang/jq has exactly this shape) has to produce an entry too:
	// install supports the single-file shape, and rejecting it wholesale would keep repos
	// like it out of the index forever.
	idx := loadOut(t, out)
	if len(idx.Packages) != 2 {
		t.Fatalf("produced %d entries, expected two: tinyserve and bare", len(idx.Packages))
	}

	// Each kind of failure gets its own wording checked: one blanket "no usable assets"
	// would send you to the repo page to figure out which of them it was.
	for _, tc := range []struct{ id, want string }{
		{"nobody/gone", "does not exist"},
		{"nobody/norelease", "never published a release"},
		{"nobody/sourceonly", "source-only"},
		{"nobody/limited", "rate limit"},
	} {
		r := genOne(context.Background(), cfg, &index.Index{Version: index.Version}, tc.id)
		if r.err == nil {
			t.Errorf("%s should have failed", tc.id)
			continue
		}
		if !strings.Contains(r.err.Error(), tc.want) {
			t.Errorf("the error for %s is %q, it should contain %q", tc.id, r.err, tc.want)
		}
	}

	// The bare-binary entry still comes out, and the right of [bin] is the TOOL NAME, not
	// the asset name.
	//
	// This assertion was flipped: it used to pin "the right-hand side should be the asset
	// name itself", and that was precisely a bug -- asset names vary by platform
	// (yq_linux_amd64 / yq_darwin_arm64) while [bin] is a single line, so every platform
	// except the one it happened to match got a symlink to a file that does not exist, and
	// only after the download and checksum both succeeded. That is exactly how
	// mikefarah/yq was broken on 5 of its 6 platforms in the index. Install now lands a
	// single file at the path [bin] names (insUnpack in cmd/toy/install.go), so this one
	// line holds on every platform.
	r := genOne(context.Background(), cfg, &index.Index{Version: index.Version}, "nobody/bare")
	if r.err != nil {
		t.Fatalf("a bare-binary repo should produce an entry: %v", r.err)
	}
	if got := r.pack.binPath; got != "jq" {
		t.Errorf("right of [bin] = %q, should be the tool name jq -- writing the asset name leaves broken symlinks on every other platform", got)
	}
	if !r.pack.bare {
		t.Error("this should be recognised as the bare-binary shape (it decides which comment goes above [bin])")
	}
}

// Hitting the rate limit without a token has to say something useful: suggest setting
// GITHUB_TOKEN, and report the remaining quota.
func TestRateLimitTellsYouToSetAToken(t *testing.T) {
	srv := fakeAPI(t, map[string]fakeRepo{"a/b": {rateLimited: true}})
	c := newGitHub(srv.URL, "")
	_, err := c.Fetch(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("should have reported the rate limit")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "quota") {
		t.Errorf("the rate-limit error does not say what to do about it: %v", err)
	}
	if !strings.Contains(c.Quota(), "0 remaining") {
		t.Errorf("Quota() = %q", c.Quota())
	}
}

// The version number has to become the {version} placeholder -- pinned to this one
// release, the entry points at a file that does not exist as soon as upstream cuts a new
// one ("cut a tag and you have shipped" is this product's selling point).
// When the tag has a recognisable version but the filenames do not (2024.09 and friends),
// fall back to the tag.
func TestVersionPlaceholderFallsBackToTheTag(t *testing.T) {
	p, err := makePack("a", "b", repoInfo{
		Tag:    "2024.09",
		Assets: []string{"b_2024.09_linux_amd64.tar.gz", "b_2024.09_darwin_arm64.tar.gz"},
	})
	if err != nil {
		t.Fatalf("makePack failed: %v", err)
	}
	got := p.res.Artifact["linux/amd64"]
	if !strings.Contains(got, "{version}") {
		t.Fatalf("not turned into a placeholder: %q", got)
	}
}

func TestReadRepoListStripsCommentsAndDupes(t *testing.T) {
	f := filepath.Join(t.TempDir(), "repos.txt")
	os.WriteFile(f, []byte("# header comment\n\nDerrity/tinyserve\n cli/cli  # the binary is called gh\nderrity/TINYSERVE\n"), 0o644)
	got, err := readRepoList(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Derrity/tinyserve", "cli/cli"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The client picks versions with `git ls-remote --tags` + semver (spec §2). A repo whose
// tags look like `jq-1.8.2` yields not a single parseable version -- the entry generates
// fine, and then `toy install` dies at step one with an error that never hints the tag
// SHAPE is the problem. An entry nobody can install does not belong in the index; same
// class of judgement as unstableBinPath.
func TestGenRefusesReposWhoseTagsAreNotSemver(t *testing.T) {
	out := filepath.Join(t.TempDir(), "idx.json")
	err := runFakeGen(t, map[string]fakeRepo{
		"jqlang/jq": {
			desc: "Command-line JSON processor", tag: "jq-1.8.2",
			assets: []string{"jq-linux-amd64", "jq-macos-arm64"},
		},
	}, []string{"jqlang/jq"}, out, false)
	if err == nil {
		t.Fatal("want the batch to fail: the only line in it produces no entry")
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("no index file should have been written -- not one entry was produced")
	}
}

// That column in the report must print the name that ACTUALLY LANDS ON PATH.
//
// name is the package directory name, binName is the command name, and on a repo like
// ripgrep the two differ (name=ripgrep, binName=rg). This column is the only checkpoint
// §14.9 rule 2 leaves for a person -- print name and they are checking a string that
// never shows up on PATH at all.
func TestReportListsTheNameThatActuallyLandsOnPATH(t *testing.T) {
	if got := reportBinNames([]genResult{{
		written: true, id: "o/r",
		pack: pack{name: "ripgrep", binName: "rg"},
	}}); len(got) != 1 || !strings.HasPrefix(got[0], "rg\t") {
		t.Fatalf("that column of the report = %v, want it to start with \"rg\"", got)
	}
}
