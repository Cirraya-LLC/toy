package main

// --probe: download one asset and unpack it, turning the right of [bin] from a guess
// into a fact.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// probeMaxBytes: we are downloading a stranger's archive, and only to glance at the
// directory layout inside. A few-hundred-MB asset is probably not a CLI tool and is not
// worth waiting for.
const probeMaxBytes = 256 << 20

// probePlatforms are the platforms we probe, ordered by "most likely to exist". Which
// one we pick does not change the answer: every platform's package in a given release
// has the same layout (goreleaser and cargo-dist both produce them all from one config).
var probePlatforms = []string{"linux/amd64", "linux/amd64/gnu", "linux/amd64/musl", "linux/arm64"}

// probeBin returns the real in-package relative path.
//
// The second return value is the candidate list for "found more than one executable and
// cannot settle it" -- in that case the first return value is empty and WE NEVER GUESS
// ONE: guessing wrong shows up as a user whose new command on PATH is a shell-completion
// script, while the report says "probed".
//
// Extraction always goes through internal/archive (rule 7: path traversal, symlink
// escape and zip bombs are all stopped in there). We are downloading a stranger's
// archive here, which is exactly what those checks are for.
func probeBin(ctx context.Context, h host.Host, p pack) (string, []string, error) {
	plat, tmpl := probePick(p.res.Artifact)
	if tmpl == "" {
		return "", nil, errors.New("no linux/amd64 or linux/arm64 build to probe, skipping")
	}
	asset := toyfile.Expand(tmpl, p.res.Version, p.info.Tag)
	url := h.ReleaseURL(p.owner, p.repo, p.info.Tag, asset)

	got, err := fetch.Get(ctx, url, fetch.Options{Name: asset, MaxBytes: probeMaxBytes})
	if err != nil {
		return "", nil, fmt.Errorf("downloading %s (%s) failed: %w", asset, plat, err)
	}
	defer os.Remove(got.Path)

	dir, err := os.MkdirTemp("", "toy-index-probe-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)

	// archive detects the format from the FILENAME SUFFIX, and the temp file fetch leaves
	// behind has no suffix, so make a symlink carrying the real asset name first (same as
	// insExtract in cmd/toy/install.go).
	link := filepath.Join(dir, filepath.Base(asset))
	if err := os.Symlink(got.Path, link); err != nil {
		return "", nil, err
	}
	out := filepath.Join(dir, "x")
	if err := os.Mkdir(out, 0o755); err != nil {
		return "", nil, err
	}
	files, err := archive.Extract(link, out)
	if err != nil {
		return "", nil, fmt.Errorf("unpacking %s failed: %w", asset, err)
	}

	hit, cands := pickExecutable(out, files, p.name)
	if hit == "" && len(cands) == 0 {
		return "", nil, fmt.Errorf("%s holds no executable at all (%d members)", asset, len(files))
	}
	return hit, cands, nil
}

func probePick(artifact map[string]string) (plat, tmpl string) {
	for _, k := range probePlatforms {
		if v, ok := artifact[k]; ok {
			return k, v
		}
	}
	return "", ""
}

// pickExecutable picks the actual command out of the extracted files.
//
// One executable means it is that one. More than one (completion scripts, man-page
// generators, a bundled second tool) first looks for one whose filename is exactly the
// command name we want; still undecided, it hands the whole candidate list over for a
// human to look at -- guessing would produce a wrong entry that LOOKS like it was
// verified.
func pickExecutable(root string, files []string, want string) (string, []string) {
	var execs []string
	for _, rel := range files {
		fi, err := os.Lstat(filepath.Join(root, rel))
		// Lstat rather than Stat: the permission bits of whatever a symlink points at are
		// not the symlink's own.
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		execs = append(execs, filepath.ToSlash(rel))
	}
	sort.Strings(execs)
	switch len(execs) {
	case 0:
		return "", nil
	case 1:
		return execs[0], nil
	}
	var named []string
	for _, e := range execs {
		if base := e[strings.LastIndex(e, "/")+1:]; base == want {
			named = append(named, e)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return "", execs
}

// unstableBinPath decides whether a probed in-package path may be written into [bin].
//
// The right of [bin] is used VERBATIM: the install side does not expand {version}, and a
// toy.toml has a single [bin] line shared by every platform (see insLink in
// cmd/toy/install.go). So when a package wraps everything in something like
// `tool_1.2.3_linux_amd64/`, the probed path pins both the version and the platform --
// writing that into an inline entry produces a record that installs today and breaks the
// moment upstream cuts a release, the same class of mistake as leaving [sha256] in.
//
// The platform test borrows assetname.Classify (which only yields a Platform when it
// recognises both os and arch) rather than keeping another linux/darwin/amd64 word list
// here -- a second copy is a second thing that slowly drifts.
func unstableBinPath(path, version string) string {
	if version != "" && strings.Contains(path, version) {
		return "it spells out the version " + version + ", so the next upstream release points at a directory that does not exist"
	}
	for _, seg := range strings.Split(path, "/") {
		if p := assetname.Classify(seg).Platform; p != "" {
			return "it spells out a platform name (" + seg + " -> " + p + "), and [bin] is a single line shared by every platform"
		}
	}
	return ""
}
