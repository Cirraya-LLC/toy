// Package assetname infers the platform key and version string from a release
// asset's filename.
//
// It lives in its own package because two callers need the EXACT SAME
// inference: `toy init` reads filenames out of a local dist/, and the bulk
// index generator reads asset name lists off a repository. Two copies would
// drift, and the symptom of that drift is nasty — the entry the index
// generates stops matching what the author gets from `toy init`, so at the
// moment "the author's own toy.toml takes over" the thing users install
// silently changes, and nothing anywhere says a word.
//
// This package is pure: no disk, no network, no exec. The only difference
// between the two callers is where the filenames come from.
package assetname

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// osWords / archWords map the words that show up in filenames to GOOS/GOARCH.
// Case-insensitive; tokens split on - _ . (so x86_64 gets torn in two, and the
// adjacent-pair check below puts it back together).
var osWords = map[string]string{
	"linux":   "linux",
	"darwin":  "darwin",
	"macos":   "darwin",
	"osx":     "darwin",
	"apple":   "darwin",
	"windows": "windows",
	"win":     "windows",
}

var archWords = map[string]string{
	"amd64": "amd64", "x86_64": "amd64", "x64": "amd64",
	"arm64": "arm64", "aarch64": "arm64",
	"armv7": "armv7", "armhf": "armv7",
	"armv6": "armv6",
	// Bare "arm" has to be here: with GOARCH=arm and no goarm set, goreleaser's
	// default name_template produces exactly `tool_1.2.3_linux_arm.tar.gz`.
	// Without this line the Raspberry Pi build gets SILENTLY DROPPED — and
	// linux/arm is precisely the candidate platform.go falls back to for
	// armv6/armv7 (spec §3.1), so dropping it means Pi users cannot install.
	"arm": "arm",
	// i686 is the most common spelling for 32-bit Linux builds (ripgrep / fd /
	// zoxide all ship `i686-unknown-linux-gnu`); without this line that build
	// gets silently dropped.
	"386": "386", "i386": "386", "i686": "386",
}

// foreignWords means "if this word shows up, the asset is not for a system we
// support". word -> system name.
//
// android needs its own table, and it needs to BEAT an already-recognized
// linux: Rust's Android triples look like `aarch64-linux-android` /
// `armv7-linux-androideabi`, so after tokenizing, `linux` comes before
// `android` and first-match-wins in osWords calls it linux/arm64, linux/armv7.
// That's a bionic build, and it STEALS THE KEY belonging to the real Linux
// build (zoxide really ships this way: the armv7 key should hold
// armv7-unknown-linux-musleabihf). The symptom is a download URL that returns
// 200, bytes that are intact, and `Exec format error` at run time — far harder
// to track down than a 404.
//
// The BSDs and illumos/solaris are here NOT to beat anything (their triples
// contain no linux) but to classify accurately: without these lines they land
// in "cannot tell which platform this is", and that message asks the author to
// hand-write an [artifact] line — terrible advice for a system toy v1 does not
// support at all (spec §11).
var foreignWords = map[string]string{
	"android": "android", "androideabi": "android",
	"freebsd": "freebsd", "openbsd": "openbsd", "netbsd": "netbsd",
	"solaris": "solaris", "illumos": "illumos",
}

// verRe matches the version segment inside a filename. Three components only:
// two-component versions (1.2) are indistinguishable from other numbers in a
// filename, and guessing wrong means replacing a string that should not have
// been touched with {version}.
//
// Whatever follows `[-+]` is a CANDIDATE prerelease tag, not a verdict — see
// preWords.
var verRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?`)

// preWords are the words a real prerelease tag actually uses.
//
// This is the most expensive line in the package. In
// `ripgrep-15.2.0-aarch64-apple-darwin.tar.gz`, `-aarch64` looks exactly like
// `-rc.1`, so the regex swallows `15.2.0-aarch64` as the whole version, and
// after substitution the template becomes
// `ripgrep-{version}-apple-darwin.tar.gz` — THE ARCHITECTURE IS GONE, and the
// URL that gets built 404s for every single user. Our own synthetic fixtures
// (`tool_1.2.0_linux_amd64.tar.gz`, where the version is followed by `_`) can
// never hit this shape.
//
// The test is an allowlist, not "exclude the os/arch words": there are far too
// many architecture names in the world (aarch64 / riscv64gc / s390x / loong64 /
// i686 / armv7 / x86…) and missing one means an asset name whose version gets
// silently mangled. The prerelease vocabulary is much smaller, and getting it
// wrong fails safe — mistaking a real prerelease tag for a platform word costs
// at most a slightly shorter replacement, and the template is still correct.
var preWords = map[string]bool{
	"alpha": true, "beta": true, "rc": true, "pre": true, "preview": true,
	"dev": true, "devel": true, "next": true, "canary": true,
	"nightly": true, "snapshot": true, "unstable": true, "edge": true,
}

// trimFalsePre cuts `15.2.0-aarch64` back to `15.2.0` while leaving
// `1.0.0-rc.1` alone.
//
// Only '-' is touched: the build metadata in `1.2.3+20240101` is part of the
// tag, and install expands {version} to the whole string (semver.V.Number only
// strips the 'v'), so trimming it would make the template stop matching.
func trimFalsePre(v string) string {
	i := strings.IndexByte(v, '-')
	if i < 0 {
		return v
	}
	word := v[i+1:]
	if j := strings.IndexByte(word, '.'); j >= 0 {
		word = word[:j]
	}
	// Catch the run-together spellings too: rc1, beta2.
	word = strings.TrimRight(strings.ToLower(word), "0123456789")
	if preWords[word] {
		return v
	}
	return v[:i]
}

// findVersions is the only way out of verRe: every match runs through
// trimFalsePre.
func findVersions(s string) []string {
	all := verRe.FindAllString(s, -1)
	for i, v := range all {
		all[i] = trimFalsePre(v)
	}
	return all
}

// companionSuffixes are the things that share a name and prefix with a real
// asset but that toy can never install: checksums, signatures, distro
// packages, third-party installers, attestations.
//
// They must be filtered out BEFORE platform detection:
// `ripgrep-15.2.0-x86_64-apple-darwin.tar.gz.sha256` and
// `bat-musl_0.26.1_musl-linux-amd64.deb` contain every os/arch word the real
// asset does, so once recognized they compete for the same platform key —
// whichever sorts first wins, and the .deb often sorts first. Even when they
// lose, each one reports a "cannot recognize / duplicate" line that drowns out
// the one worth reading (§14.10).
var companionSuffixes = []string{
	".sha256", ".sha512", ".sha1", ".md5", ".sum", ".sig", ".asc", ".pem",
	".sbom", ".spdx", ".intoto", ".json", ".txt", ".sha256sum",
	".deb", ".rpm", ".apk", ".pkg", ".msi", ".dmg", ".snap", ".appimage",
	// BLAKE3 and minisign sidecars. watchexec ships `...-gnu.deb.b3`, and the
	// suffix that decides is the LAST one -- the `.deb` before it was already on
	// this list and still did not save us, because the check looks at the end of
	// the name. Without `.b3` here that file was picked as the artifact and
	// installed: a text file holding a hex digest, chmod +x'd, and the shell
	// reported `<sha256>: command not found` from line 1 of the "binary".
	".b3", ".blake3", ".minisig", ".cert", ".crt", ".pub", ".sigstore",
}

// Compression and package formats we cannot unpack are deliberately NOT in the
// list above. That list is consulted before platform detection and matches on the
// end of the name, so putting ".gz" in it silently threw away every .tar.gz --
// the formats belong here instead, where the question is "can we unpack this",
// and where a rejection is reported with a reason instead of vanishing.

// notAnArchiveWeHandle is the companion list restricted to the entries that mean
// "we cannot unpack this", used where the question is specifically about format
// rather than about sidecar files. `.tar.gz` is deliberately NOT caught by the
// `.gz` entry: matching is by full suffix, and `.tar.gz` is handled elsewhere
// before this list is consulted.
func UnpackableSuffix(name string) string {
	l := strings.ToLower(name)
	if strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") {
		return ""
	}
	// .bz2 and a bare .gz are handled: compress/bzip2 and compress/gzip are both in
	// the standard library, so they cost no dependency (§10). .xz is not, and a
	// library for it would break that rule -- so .tar.xz stays refused.
	for _, sfx := range []string{".bz", ".xz", ".lzma", ".zst", ".zstd",
		".lz4", ".br", ".7z", ".rar", ".tar.xz", ".txz",
		".deb", ".rpm", ".apk", ".pkg", ".msi", ".dmg", ".snap"} {
		if strings.HasSuffix(l, sfx) {
			return sfx
		}
	}
	return ""
}

// foreignOS reports whether the goos detect returned is a foreign system.
// android gets its own message from Classify (it has to explain the "the name
// says linux" part), so it is excluded here.
func foreignOS(goos string) bool {
	if goos == "" || goos == "android" {
		return false
	}
	for _, v := range foreignWords {
		if v == goos {
			return true
		}
	}
	return false
}

func looksLikeCompanion(f string) bool {
	l := strings.ToLower(f)
	for _, ext := range companionSuffixes {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// Reasons for skipping. These are written for humans and get printed verbatim
// in toy init's output.
const (
	SkipWindows      = "Windows build, and toy v1 does not support Windows (spec §11) — skipped"
	SkipUnrecognized = "cannot tell which platform this is (no OS or arch recognized) — skipped. " +
		"If it really is a release asset, add a line for it to [artifact] yourself"
	SkipNotAnAsset = "does not look like a release asset (not an archive) — skipped"
	SkipAndroid    = "Android (bionic) build — the name says linux, but it will not run on a Linux machine — skipped"
)

// SkipForeignOS is "recognized, but that is not a system toy supports". Kept
// separate from SkipUnrecognized: that one asks the author to hand-write an
// [artifact] line, and for these systems writing one still would not install.
func SkipForeignOS(goos string) string {
	return goos + " build, and toy v1 only supports Linux and macOS (spec §11) — skipped"
}

// Asset is one release asset as recognized.
type Asset struct {
	File     string // original filename
	Platform string // "linux/amd64" / "linux/amd64/musl" / "linux/armv7"; empty if unrecognized
	Version  string // version string found in this filename; empty if none
	Template string // asset name with the version replaced by {version}; only Infer fills this
	Skip     string // non-empty means skip it; the content is the human-readable reason
	// Notable means this skip is worth telling the author about. checksums.txt
	// and LICENSE were never assets to begin with; a line for each of them
	// would drown out the one that matters (say, an unrecognized
	// freebsd_riscv64).
	Notable bool
}

// Classify recognizes one filename. Pure: no disk, no network.
func Classify(name string) Asset {
	a := Asset{File: name, Version: firstVersion(name)}
	if looksLikeCompanion(name) {
		// Silent: one of these sits next to every real asset, and speaking up
		// drowns out the real line.
		a.Skip = SkipNotAnAsset
		return a
	}
	key, goos := detect(name)
	switch {
	case goos == "windows":
		a.Skip, a.Notable = SkipWindows, true
	case goos == "android":
		a.Skip, a.Notable = SkipAndroid, true
	case foreignOS(goos):
		a.Skip, a.Notable = SkipForeignOS(goos), true
	case key == "" && looksLikeArchive(name):
		a.Skip, a.Notable = SkipUnrecognized, true
	case key == "":
		a.Skip = SkipNotAnAsset
	default:
		a.Platform = key
	}
	return a
}

// Result is what recognizing a batch of filenames produces.
type Result struct {
	// Assets are the recognized assets, sorted by platform key, with Template
	// already filled in.
	Assets []Asset
	// Artifact is the [artifact] section of toy.toml: platform key -> asset
	// name template.
	Artifact map[string]string
	// Version is this batch's version string (either forceVersion or whatever
	// was found in the filenames).
	Version string
	// Skipped holds only the skips worth telling the author about (Notable).
	Skipped []Asset
	// Name is the package name derived from the assets' common prefix; empty
	// if it cannot be derived.
	Name string
	// Unreplaced non-empty means the generated templates STILL CONTAIN a
	// version string that could not be turned into {version}. Such an entry is
	// pinned to this one release: the moment upstream ships a new version,
	// everyone installing gets a 404.
	//
	// The test is "the template still has a version in it", not "the template
	// has no {version}": plenty of projects ship asset names with no version at
	// all (`eza_x86_64-unknown-linux-gnu.tar.gz`, `yq_linux_amd64.tar.gz`), and
	// those templates are naturally stable across releases. Warning about them
	// would be a false positive — and a warning that fires every time is no
	// warning at all.
	Unreplaced string
}

// InferTag is Infer plus one extra fact: which tag this batch of assets was
// released under (the bulk index generator can get that from the host's API;
// `toy init` cannot).
//
// When the tag really does appear in the asset names, THE TAG WINS: the regex
// can only guess, and in `ripgrep-15.2.0-aarch64-apple-darwin.tar.gz`,
// `-aarch64` and `-rc.1` look identical (see preWords). Do not guess when the
// answer is known.
//
// The replacement always uses `TrimPrefix(tag, "v")` and nothing else — that is
// exactly what install expands {version} into (semver.V.Number). Substituting
// with a package-name-prefixed tag like `jq-1.8.2` would expand back missing
// the `jq-` part.
func InferTag(files []string, tag string) Result {
	v := tagVersion(tag)
	if v != "" && anyContainsVersion(files, v) {
		return Infer(files, v)
	}
	res := Infer(files, "")
	if res.Version == "" && v != "" {
		// No version in the asset names (the cargo-dist shape). Nothing to
		// substitute, but the version itself still has to be recorded — probe
		// uses it to judge whether the [bin] path is stable.
		res = Infer(files, v)
	}
	return res
}

// tagVersion turns a tag into the string it is safe to substitute literally
// inside asset names, or returns empty when that is not safe.
//
// Two gates: it must start with a digit (a package-prefixed tag like
// `jq-1.8.2` would not expand back correctly), and it must contain a dot (a
// bare tag of `2` would bite into the middle of `tool-2.0.1-…`).
func tagVersion(tag string) string {
	v := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if v == "" || v[0] < '0' || v[0] > '9' || !strings.Contains(v, ".") {
		return ""
	}
	return v
}

// anyContainsVersion reports whether v appears in some filename AS A WHOLE
// numeric segment. The boundary check is not optional: tag `2.0` matches inside
// `tool-2.0.1-linux.tar.gz`, and the resulting
// `tool-{version}.1-linux.tar.gz` is a template nobody can install.
func anyContainsVersion(files []string, v string) bool {
	for _, f := range files {
		for i := 0; i+len(v) <= len(f); i++ {
			if f[i:i+len(v)] != v {
				continue
			}
			if i > 0 && isVerByte(f[i-1]) {
				continue
			}
			if j := i + len(v); j < len(f) && isVerByte(f[j]) {
				continue
			}
			return true
		}
	}
	return false
}

func isVerByte(c byte) bool { return c == '.' || (c >= '0' && c <= '9') }

// Infer recognizes a batch of filenames. A non-empty forceVersion wins — it is
// the only way out when the filenames carry no three-component version (things
// like 2024.09).
//
// When one platform has several files, the first in sort order is kept
// (shipping both .tar.gz and .zip is common), and the fact is recorded in
// Skipped so the author can pick for themselves.
func Infer(files []string, forceVersion string) Result {
	res := Result{Artifact: map[string]string{}}

	var hits []Asset
	for _, f := range files {
		a := Classify(f)
		if a.Skip != "" {
			if a.Notable {
				res.Skipped = append(res.Skipped, a)
			}
			continue
		}
		hits = append(hits, a)
	}
	if len(hits) == 0 {
		return res
	}

	res.Version = strings.TrimPrefix(strings.TrimSpace(forceVersion), "v")
	if res.Version == "" {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.File
		}
		res.Version = commonVersion(names)
	}

	seen := map[string]string{}
	for _, h := range hits {
		if first, dup := seen[h.Platform]; dup {
			h.Skip = "same platform (" + h.Platform + ") as " + first +
				"; only one can be kept, so the first one won. To keep this one instead, edit that line in [artifact] yourself"
			h.Notable = true
			h.Platform = ""
			res.Skipped = append(res.Skipped, h)
			continue
		}
		seen[h.Platform] = h.File
		// Without turning the version into {version}, the author has to come
		// back and edit toy.toml on every release — and "push a tag, that's the
		// release" is this product's selling point.
		h.Template = h.File
		if res.Version != "" {
			h.Template = strings.ReplaceAll(h.Template, res.Version, "{version}")
		}
		res.Assets = append(res.Assets, h)
		res.Artifact[h.Platform] = h.Template
	}
	sort.Slice(res.Assets, func(i, j int) bool { return res.Assets[i].Platform < res.Assets[j].Platform })
	res.Name = commonPrefix(res.Assets)
	for _, a := range res.Assets {
		if left := findVersions(a.Template); len(left) > 0 {
			res.Unreplaced = left[0]
			break
		}
	}
	return res
}

// looksLikeArchive judges whether a file looks like a release asset, and is
// only used to decide WHETHER TO SPEAK UP when we cannot recognize it. Prefer
// missing one over crying wolf: checksums.txt, LICENSE and README.md all end up
// in dist/, and a warning line for each of them drowns out the one line the
// author actually needs to look at.
func looksLikeArchive(f string) bool {
	l := strings.ToLower(f)
	for _, ext := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2", ".tar.zst", ".zip"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// detect recognizes the platform key in a filename. Returns an empty key when
// it cannot; the second return value is the GOOS it recognized (Windows has to
// be explained when skipped, never dropped silently).
//
// Rust target triples (x86_64-unknown-linux-gnu / aarch64-apple-darwin) need no
// special case: after tokenizing, every word we need is there, and the extra
// segments like unknown / pc are simply not recognized.
func detect(file string) (key, goos string) {
	toks := tokens(file)
	var arch, libc string
	for i, tok := range toks {
		// Foreign systems first: android has to beat a linux already recognized
		// in the same filename, not queue up behind it.
		if v, ok := foreignWords[tok]; ok {
			return "", v
		}
		if v, ok := osWords[tok]; ok && goos == "" {
			goos = v
		}
		if v, ok := archWords[tok]; ok && arch == "" {
			arch = v
		}
		// '_' tore x86_64 in half; put it back with the adjacent pair.
		if i+1 < len(toks) {
			if v, ok := archWords[tok+"_"+toks[i+1]]; ok && arch == "" {
				arch = v
			}
		}
		switch tok {
		case "musl", "gnu":
			libc = tok
		}
	}
	if goos == "" || arch == "" {
		return "", goos
	}
	if goos == "windows" {
		return "", goos
	}
	switch {
	// armv6/armv7 already name a generation; hanging a libc off that means
	// nothing (and nobody ships that way).
	case strings.HasPrefix(arch, "armv"):
		return goos + "/" + arch, goos
	case goos == "linux" && libc != "":
		return goos + "/" + arch + "/" + libc, goos
	default:
		return goos + "/" + arch, goos
	}
}

func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
}

// firstVersion takes the first version string that appears in this filename.
func firstVersion(file string) string {
	if all := findVersions(file); len(all) > 0 {
		return all[0]
	}
	return ""
}

// commonVersion picks the version string that appears most often. A goreleaser
// batch shares one version, so "the most common one" is it; ties break on
// lexicographic order so the same filenames always produce the same result.
func commonVersion(files []string) string {
	count := map[string]int{}
	for _, f := range files {
		// The same version can appear twice in one filename (directory prefix +
		// package name); count it once.
		for _, v := range slices.Compact(slices.Sorted(slices.Values(findVersions(f)))) {
			count[v]++
		}
	}
	best := ""
	for v, n := range count {
		if best == "" || n > count[best] || (n == count[best] && v < best) {
			best = v
		}
	}
	return best
}

// commonPrefix digs the tool name out of the asset names: take the common
// prefix, cut everything from the version segment on, then trim trailing
// separators. The batch `hello_1.2.0_linux_amd64.tar.gz` yields `hello`.
func commonPrefix(assets []Asset) string {
	if len(assets) == 0 {
		return ""
	}
	p := assets[0].File
	for _, a := range assets[1:] {
		for !strings.HasPrefix(a.File, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	if loc := verRe.FindStringIndex(p); loc != nil {
		p = p[:loc[0]]
	}
	p = strings.TrimRight(p, "-_.")
	// For the run-together `-v` spelling (`fd-v10.5.0-x86_64-…`), cutting the
	// version leaves the prefix `fd-v`. That v is the tag's prefix, not part of
	// the name — keep it and the command gets named `fd-v`.
	if n := len(p); n > 1 && (p[n-1] == 'v' || p[n-1] == 'V') && strings.ContainsRune("-_.", rune(p[n-2])) {
		p = strings.TrimRight(p[:n-1], "-_.")
	}
	return p
}
