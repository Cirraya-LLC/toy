package main

// Build an inline toyfile (spec §5.1) out of one repo's release facts.

import (
	"fmt"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// descMax caps the desc length in runes. desc is remote content: it goes verbatim into
// the index JSON and from there into `toy search` results, so a few-KB description
// would push a whole screen of search results off the top.
const descMax = 160

// pack is everything recognised about one repo.
type pack struct {
	owner, repo string
	info        repoInfo
	res         assetname.Result
	name        string // tool name (= toy.toml's name, also the package directory name)
	binName     string // the command name on the left of [bin]. Defaults to name; --probe replaces it with the real file's basename
	binPath     string // right of [bin]: the in-package path after extraction
	binProbed   bool   // true = read out of a real archive, false = guessed
	bare        bool   // true = the assets are bare binaries
	notes       []string
}

// installable reports that toy can actually install this asset.
//
// Archives, plus BARE BINARIES WHOSE PLATFORM IS RECOGNISABLE (`jq-linux-amd64` and
// friends -- when archive.IsArchive is false the install side just downloads and
// chmod +x's, see the default branch of insUnpack). Reject bare binaries wholesale and
// a repo like jqlang/jq, which only ships single files, gets judged "not one installable
// asset" and never makes it into the index at all.
//
// Bare files whose platform cannot be recognised (checksums.txt, signatures,
// sha256sum.txt) are stopped right here; the test borrows assetname.Classify rather than
// keeping a second copy of that word list.
//
// The one hard rejection is .tar.xz: internal/archive returns ErrUnsupported for it
// (needs an external xz), so letting it through only produces an entry that generates
// fine and installs never.
func installable(name string) bool {
	// Formats we cannot unpack come first, before IsArchive: IsArchive recognizes
	// .tar.xz as a format it knows OF, and answers ErrUnsupported only at
	// extraction time.
	//
	// It also has to be an allowlist rather than "whatever we did not recognize",
	// because the fall-through case treats a name as a bare binary and chmod +x's
	// it. A blocklist let restic's `restic_0.18.1_linux_amd64.bz2` through -- no
	// recognized archive suffix, a perfectly readable platform in the name -- and
	// the shell answered `cannot execute binary file: Exec format error`, after
	// the download and the checksum had both passed.
	if assetname.UnpackableSuffix(name) != "" {
		return false
	}
	if archive.IsArchive(name) {
		return true
	}
	return assetname.Classify(name).Platform != ""
}

// allBareBinaries reports that EVERY asset in this package is a bare binary (no archive
// suffix). It only decides which comment to write above [bin] -- the path on the right
// is <name> either way, because install lands a single file at the path [bin] names
// rather than at the asset name.
func allBareBinaries(res assetname.Result) bool {
	if len(res.Assets) == 0 {
		return false
	}
	for _, a := range res.Assets {
		if archive.IsArchive(a.Template) {
			return false
		}
	}
	return true
}

// makePack turns remote facts into a pack ready to render. A non-nil second return value
// means this repo cannot produce an entry.
func makePack(owner, repo string, info repoInfo) (pack, error) {
	p := pack{owner: owner, repo: repo, info: info}

	var names []string
	for _, a := range info.Assets {
		if installable(a) {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return p, noUsableAssets(info)
	}

	// Version inference goes through the same function `toy init` uses
	// (internal/assetname), and that is not incidental: two implementations would drift,
	// and drift means that at the moment upstream adds its own toy.toml and the inline
	// entry steps aside, what users install quietly changes -- with nothing anywhere
	// saying so.
	//
	// This side knows one thing toy init does not: the release tag. So it calls InferTag
	// -- literal substitution when the tag really appears in the asset name, with the
	// regex only stepping in when there is no tag ("15.2.0-aarch64" would otherwise be
	// swallowed whole as the version, see assetname.preWords).
	p.res = assetname.InferTag(names, info.Tag)
	if len(p.res.Assets) == 0 {
		return p, fmt.Errorf("none of the %d installable assets in the release has a recognisable os/arch "+
			"in its filename, so [artifact] cannot be written. Saw: %s", len(names), strings.Join(head(names, 6), ", "))
	}

	p.name, p.binPath = pickName(repo, p.res)
	p.binName = p.name
	// Bare binaries need no special case any more: install lands a single file at the
	// path on the right of [bin] rather than at the asset name (insUnpack in
	// cmd/toy/install.go), so <name> = "<name>" holds on every platform. Writing the
	// asset name instead used to leave a broken symlink on every platform except the one
	// it happened to match -- that is exactly how mikefarah/yq was broken on 5 of its 6
	// platforms in the index.
	p.bare = allBareBinaries(p.res)
	for _, s := range p.res.Skipped {
		p.notes = append(p.notes, s.File+": "+s.Skip)
	}
	if v := p.res.Unreplaced; v != "" {
		// A version string still sitting in the template means this entry is pinned to
		// this one release: the next upstream version hands installers a 404 download
		// URL. A human has to look at this one.
		//
		// The test is "a version number is still in there", not "there is no {version}":
		// projects whose asset names never carry a version (eza, yq) are stable across
		// releases by construction, warning about them is a false positive, and a warning
		// that fires every time drowns out this real one.
		p.notes = append(p.notes, "the [artifact] template still spells out the version "+v+" instead of "+
			"{version} -- the next upstream release makes it point at a file that does not exist; fix it by hand")
	}
	return p, nil
}

// noUsableAssets splits "cannot produce an entry" into cases, each saying its own thing
// in plain words. One blanket "no usable assets" would send you to the repo page to
// figure out which of them it was.
func noUsableAssets(info repoInfo) error {
	if len(info.Assets) == 0 {
		return fmt.Errorf("the latest release (%s) has no assets at all -- most likely a source-only release. "+
			"GitHub's auto-attached source tarballs do not count, and toy never builds from source (rule 2)", info.Tag)
	}
	return fmt.Errorf("the latest release (%s) has %d assets and toy can install none of them. "+
		"Saw: %s. Archives and bare binaries with a recognisable platform are both accepted; the rest "+
		"(.tar.xz cannot be unpacked; checksums, signatures, .deb/.rpm and installers are not executables) "+
		"need a hand-written entry",
		info.Tag, len(info.Assets), strings.Join(head(info.Assets, 6), ", "))
}

// pickName settles the tool name and the default for the right of [bin].
//
// THE ASSET-NAME PREFIX BEATS THE REPO NAME: a repo called cli/cli shipping a binary
// called gh is ordinary, and the asset name is written by the author's own build script,
// so it is closer to the real name than the repo name is.
func pickName(repo string, res assetname.Result) (name, binPath string) {
	name = repo
	if n := res.Name; cleanName(n) {
		name = n
	}
	// The right-hand side is the in-package path AFTER extraction. goreleaser does not
	// wrap things in a directory by default, so "a file with the same name as the
	// command" is the best guess available -- but it is a guess, and only --probe
	// settles it.
	return name, name
}

// cleanName decides whether this name is awkward as a command name or a directory name.
// The real validation lives in toyfile.ParseFrom (the rendered toyfile always goes
// through it); this only picks the more presentable candidate.
func cleanName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '+':
		default:
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// renderToyfile writes the inline toy.toml source.
//
// NEVER WRITE [sha256]: inline entries follow the upstream's newest tag (the client
// picks the highest semver out of git ls-remote), while [sha256] pins one version's
// bytes. Leave it in and the author's next release hands everyone "checksum mismatch,
// refusing to install" -- which reads exactly like a poisoning warning when all that
// happened is a normal release. First install is still pinned by trust on first use
// (second half of rule 4).
// This was just fixed once on --inline in cmd/toy/author.go; do not reintroduce it here.
func renderToyfile(p pack) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by toy-index gen from the asset names of %s/%s's latest release (%s).\n",
		p.owner, p.repo, tomlSafe(p.info.Tag, 64))
	b.WriteString("# This is an INLINE packaging description (spec §5.1): the bytes come from the author's own\n")
	b.WriteString("# release, but which asset to install and what to name the command is the index talking --\n")
	b.WriteString("# so the client downgrades the trust level to index-toyfile.\n")
	b.WriteString("# There is deliberately no [sha256]: inline entries follow the upstream's newest tag, so\n")
	b.WriteString("# pinning one version's bytes would turn the next release into a checksum mismatch that reads\n")
	b.WriteString("# like a poisoning warning. First install is still pinned by trust on first use.\n")
	fmt.Fprintf(&b, "name    = %s\n", quote(p.name))
	fmt.Fprintf(&b, "desc    = %s\n", quote(tomlSafe(p.info.Desc, descMax)))
	if s := tomlSafe(p.info.License, 64); s != "" {
		fmt.Fprintf(&b, "license = %s\n", quote(s))
	}
	if s := tomlSafe(p.info.Homepage, 200); s != "" {
		fmt.Fprintf(&b, "homepage = %s\n", quote(s))
	}

	b.WriteString("\n[bin]\n")
	if p.bare {
		b.WriteString("# This repo ships bare binaries, not archives: install lands the file at the path on the\n")
		b.WriteString("# right, so this one line holds for every platform.\n")
	} else if p.binProbed {
		b.WriteString("# The path on the right was read out of a real archive that was unpacked and inspected.\n")
	} else {
		b.WriteString("# The path on the right is a GUESS: it is wrong if the archive has subdirectories (bin/ etc).\n")
		b.WriteString("# A single wrapping top-level directory does not count -- that layer is stripped on extract.\n")
		b.WriteString("# Re-run with --probe to settle it, or look for yourself with `tar tzf`.\n")
	}
	fmt.Fprintf(&b, "%s = %s\n", tomlKey(p.binName), quote(p.binPath))

	b.WriteString("\n[artifact]\n")
	for _, a := range p.res.Assets {
		fmt.Fprintf(&b, "%s = %s\n", tomlKey(a.Platform), quote(a.Template))
	}

	data := []byte(b.String())
	// Even what we generate ourselves goes through the real parser, and through the
	// INDEX-side entry point specifically -- index.validEntry uses the same one, and it
	// throws out a whole entry when it fails. Blowing up here means the error can name
	// the repo; left until Load, all you get is "skipped N invalid records".
	if _, err := toyfile.ParseFrom(data, toyfile.OriginIndex); err != nil {
		return nil, fmt.Errorf("the generated toy.toml does not parse: %w", err)
	}
	return data, nil
}

// tomlSafe strips control characters and truncates by rune. These fields are all remote
// content printed verbatim to a terminal -- a "\r\x1b[32m✓ signed" can overwrite the
// trust-level line that was just printed (rule 8). toyfile.checkPrintable catches
// anything that slips past; stripping here is so a perfectly good entry does not get
// killed outright over a stray \t in its description.
func tomlSafe(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		if n >= max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// quote writes a toml basic string. DO NOT USE strconv.Quote: Go writes non-printable
// bytes as \xNN, and toml has no \x escape at all -- the output would be a syntactically
// invalid file. Control characters are already gone via tomlSafe, so only two characters
// are left to escape.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// tomlKey: a bare key may only be [A-Za-z0-9_-]; everything else gets quoted (platform
// keys contain a slash, so they must be).
func tomlKey(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return quote(s)
		}
	}
	if s == "" {
		return quote(s)
	}
	return s
}

func head(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("... (%d more)", len(s)-n))
}
