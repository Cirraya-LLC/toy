// Package toyfile parses and validates the toy.toml an author writes. Spec §3.
package toyfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	ErrNoArtifact   = errors.New("toyfile: this repo publishes no binaries")
	ErrBothForms    = errors.New("toyfile: [artifact] and [script] are mutually exclusive")
	ErrNoPlatform   = errors.New("toyfile: no build for this platform")
	ErrUnsafeTarget = errors.New("toyfile: unsafe bin target path")
)

type Toyfile struct {
	Name     string            `toml:"name"`
	Desc     string            `toml:"desc"`
	License  string            `toml:"license"`
	Homepage string            `toml:"homepage"`
	Bin      map[string]string `toml:"bin"`      // command name on PATH -> path inside the package
	Artifact map[string]string `toml:"artifact"` // "linux/amd64" -> release asset name template
	SHA256   map[string]string `toml:"sha256"`   // "linux/amd64" -> hex; optional, but declaring one makes it mandatory
	Script   map[string]string `toml:"script"`   // single-file form: command name on PATH -> asset name template

	// Origin records whether these bytes came from the author's own repo or from an index
	// entry (the inline toyfile of spec §5). It is NEVER part of the on-disk format: it says
	// where the bytes came from, which is not something an author gets to declare -- if it
	// were, an inline toyfile could claim to be upstream, and the trust level is graded off
	// exactly this (iron law 8). toml:"-" also means a real `origin =` line in a toy.toml
	// lands in Undecoded and gets rejected.
	Origin string `toml:"-"`
}

// Where a toyfile came from. Origin is only ever one of these two.
const (
	OriginUpstream = "upstream" // the toy.toml in the repo, written by the author
	OriginIndex    = "index"    // inlined in an index entry, written by whoever submitted it
)

// Parse parses and validates. Validation MUST reject:
//   - [artifact] and [script] both present, or neither
//   - an empty name, or a name containing a path separator
//   - a bin/script key containing '/', or a value that is absolute or contains ".." (iron laws 5 and 7)
//   - a sha256 value that is not 64 hex characters
//
// Errors are written for humans and say what to change.
func Parse(data []byte) (*Toyfile, error) { return ParseFrom(data, OriginUpstream) }

// ParseFrom is Parse plus a provenance tag. The validation rules are IDENTICAL -- not one
// of them is relaxed. An inline toyfile is remote content (anyone can fork the index,
// anyone can open a PR), so it needs these checks MORE than the author's own file, not
// less. Especially "a bin target may not be absolute or contain .." and "an artifact
// value may not be a URL": those are precisely what bounds the blast radius of a
// malicious index record.
func ParseFrom(data []byte, origin string) (*Toyfile, error) {
	var t Toyfile
	md, err := toml.Decode(string(data), &t)
	if err != nil {
		return nil, fmt.Errorf("toyfile: toy.toml is not valid TOML: %w", err)
	}
	// Unknown keys are rejected, never silently ignored: typing [sha265] instead of [sha256]
	// means "the author believes they pinned a hash, the client never verified anything" --
	// exactly what iron law 4 exists to prevent. It also catches install/postinstall/hook
	// keys: toy runs no code from a package (iron law 1), and swallowing those would let an
	// author believe they took effect.
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("toyfile: toy.toml has keys toy does not recognize: %s. "+
			"Only name / desc / license / homepage and the [bin] [artifact] [sha256] [script] sections are valid. "+
			"An unknown key would do nothing -- and a [sha256] with one letter wrong means no checksum verification "+
			"at all, so it is rejected outright rather than ignored. Also: toy never runs code from a package, "+
			"so install / build / hook keys do not exist in toy.toml",
			describeKeys(un))
	}

	if err := checkName(t.Name); err != nil {
		return nil, err
	}
	for what, val := range map[string]string{"desc": t.Desc, "license": t.License, "homepage": t.Homepage} {
		if err := checkPrintable(what, val); err != nil {
			return nil, err
		}
	}

	hasArtifact, hasScript := len(t.Artifact) > 0, len(t.Script) > 0
	switch {
	case hasArtifact && hasScript:
		return nil, fmt.Errorf("%w: %q declares both sections. Shipping release binaries? Keep only [artifact]. "+
			"Shipping a single-file script? Keep only [script]", ErrBothForms, t.Name)
	case !hasArtifact && !hasScript:
		return nil, fmt.Errorf("%w: the toy.toml for %q has neither [artifact] nor [script], so there is nothing "+
			"to install -- toy never builds from source. If the project publishes release assets, add [artifact] "+
			"(key is GOOS/GOARCH, value is the asset name, {version} and {tag} are available as placeholders); "+
			"if it is a single-file script, use [script] instead. Either way that is about 8 lines of toml, "+
			"and you do not need anyone's approval to add it", ErrNoArtifact, t.Name)
	}

	// bin: command name on PATH -> relative path inside the extracted package.
	for name, target := range t.Bin {
		if err := checkBinName("bin", name); err != nil {
			return nil, err
		}
		if err := checkRelPath("bin", name, target); err != nil {
			return nil, err
		}
	}

	// A [script] key also becomes a command name on PATH; the value is the asset to download.
	for name, asset := range t.Script {
		if err := checkBinName("script", name); err != nil {
			return nil, err
		}
		if err := checkRelPath("script", name, asset); err != nil {
			return nil, err
		}
	}

	if hasArtifact && len(t.Bin) == 0 {
		return nil, fmt.Errorf("toyfile: %q has [artifact] but no [bin], so installing it puts no command on "+
			"your PATH. Add a [bin] section, e.g. %s = \"%s\" -- the right-hand side is the path inside the "+
			"extracted package", t.Name, t.Name, t.Name)
	}

	for plat, asset := range t.Artifact {
		if err := checkPlatformKey("artifact", plat); err != nil {
			return nil, err
		}
		// The asset name is appended verbatim to the release download URL. With ".." or an
		// absolute path, URL normalization lands on a different repo entirely -- while the
		// client still prints "source github.com/<author>" straight out of toy.toml, which is
		// now a lie (iron law 8). An empty value is the same story: the URL ends in a slash,
		// an HTML page comes back, and with no sha256 it gets pinned by TOFU as the binary.
		if err := checkRelPath("artifact", plat, asset); err != nil {
			return nil, err
		}
	}

	for plat, sum := range t.SHA256 {
		if err := checkPlatformKey("sha256", plat); err != nil {
			return nil, err
		}
		if !isHex64(sum) {
			return nil, fmt.Errorf("toyfile: the [sha256] value for %q is %q, which is not 64 lowercase hex "+
				"characters. Paste the digest sha256sum prints, verbatim -- no filename, no uppercase", plat, sum)
		}
		// One wrong letter in the key silently skips verification -- and iron law 4 says a
		// declared sha256 MUST be enforced. Better to refuse outright than to install a
		// binary nobody checked.
		if _, ok := t.Artifact[plat]; !ok {
			if hasScript {
				return nil, fmt.Errorf("toyfile: %q is a [script] package and has no per-platform assets, so the "+
					"[sha256] entry for %q would never be used. Single-file scripts pin their checksum on first "+
					"install (trust on first use), so just delete the whole [sha256] section", t.Name, plat)
			}
			return nil, fmt.Errorf("toyfile: [sha256] has %q but [artifact] does not. The keys must match "+
				"exactly, or this checksum is never used -- the same as not writing it at all", plat)
		}
	}

	t.Origin = origin
	return &t, nil
}

// IsScript reports the single-file script form: download, chmod +x, symlink -- no
// extraction, no build.
func (t *Toyfile) IsScript() bool { return len(t.Script) > 0 }

// Asset picks the asset name for goos/goarch, expands the {version} / {tag} placeholders,
// and returns the sha256 declared for that platform ("" if there is none). If the
// platform is missing it returns ErrNoPlatform and lists the platforms the toyfile does
// declare.
//
// It tries exactly one key -- no fallback. For the musl / Rosetta / arm-generation
// fallbacks use AssetFor, fed by internal/platform.Candidates.
func (t *Toyfile) Asset(goos, goarch, version, tag string) (asset, sha256 string, err error) {
	a, sum, _, err := t.AssetFor([]string{goos + "/" + goarch}, version, tag)
	return a, sum, err
}

// AssetFor walks the candidates in order and takes the first hit, returning the asset
// name, the declared sha256, and the candidate key that actually matched. Callers need
// that last one to tell the user a fallback happened: installing a linux/amd64 build on
// Alpine, or a darwin/amd64 build on Apple Silicon, has to be said out loud (iron law 8).
func (t *Toyfile) AssetFor(candidates []string, version, tag string) (asset, sha256, matched string, err error) {
	if t.IsScript() {
		return "", "", "", fmt.Errorf("toyfile: %q is a single-file script package ([script]) and has no "+
			"per-platform assets. Callers should check IsScript() first and use each Script entry directly", t.Name)
	}

	for _, plat := range candidates {
		tmpl, ok := t.Artifact[plat]
		if !ok {
			continue
		}
		// {version} / {tag} come from the remote (the tag git ls-remote returned, or the
		// caller's --version). Only after expansion is this the string that actually goes
		// into the download URL, so check it again here, at the one exit.
		expanded := Expand(tmpl, version, tag)
		if err := checkRelPath("artifact", plat, expanded); err != nil {
			return "", "", "", err
		}
		return expanded, t.SHA256[plat], plat, nil
	}

	// List both sides: the user needs to know what we tried, the author needs to know which
	// key to add. Suggest the two-segment base key, never the first candidate -- on Linux the
	// first candidate is linux/amd64/gnu, which we synthesized ourselves, and nobody ships an
	// asset called "gnu". Point the user at that and the issue they open upstream asks for
	// the wrong thing.
	want := "the current platform"
	if len(candidates) > 0 {
		want = baseKey(candidates[0])
	}
	return "", "", "", fmt.Errorf("%w: tried %s, but this toy.toml only declares %s. "+
		"To get it, open an issue upstream asking for a %s asset in the release plus one more line in toy.toml",
		ErrNoPlatform, describePlatforms(candidates), describePlatforms(t.Platforms()), want)
}

// baseKey trims a platform key to its first two segments (goos/goarch):
// linux/amd64/musl -> linux/amd64. A key that is already two segments comes back as is.
func baseKey(plat string) string {
	if i := strings.Index(plat, "/"); i >= 0 {
		if j := strings.Index(plat[i+1:], "/"); j >= 0 {
			return plat[:i+1+j]
		}
	}
	return plat
}

// Platforms returns the declared platforms in a stable order, for error messages.
func (t *Toyfile) Platforms() []string {
	out := make([]string, 0, len(t.Artifact))
	for plat := range t.Artifact {
		out = append(out, plat)
	}
	sort.Strings(out)
	return out
}

// Expand fills in the two placeholders in an asset name: {version} without the v prefix,
// {tag} verbatim. The version may arrive with or without a leading v -- it is stripped
// here, once, so every caller does not have to.
func Expand(tmpl, version, tag string) string {
	return strings.NewReplacer(
		"{version}", strings.TrimPrefix(version, "v"),
		"{tag}", tag,
	).Replace(tmpl)
}

func describePlatforms(plats []string) string {
	if len(plats) == 0 {
		return "none"
	}
	return strings.Join(plats, ", ")
}

// checkName: name becomes a directory name under ~/.toy/pkg, so it cannot be a path.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New(`toyfile: toy.toml is missing name. Add a line name = "your-tool-name" -- ` +
			`it becomes the directory name under ~/.toy once installed`)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: name %q must not contain a path separator; it is a name, not a path", name)
	case name == "." || name == ".." || strings.Contains(name, ".."):
		return fmt.Errorf(`toyfile: name %q must not contain ".."; it is used as a directory name`, name)
	}
	return checkPrintable("name", name)
}

// checkBinName: this key becomes a symlink name in ~/.toy/bin, so it cannot be a path either.
func checkBinName(section, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("toyfile: [%s] has an entry with an empty name. "+
			"The left-hand side is the command name on your PATH", section)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: the name %q in [%s] must not contain a path separator -- "+
			"the left-hand side is the command name on PATH, the right-hand side is the path", name, section)
	case name == "." || name == "..":
		return fmt.Errorf("toyfile: the name %q in [%s] is not a usable command name", name, section)
	case strings.ContainsAny(name, " \t"):
		return fmt.Errorf("toyfile: the name %q in [%s] contains whitespace. It is symlinked into ~/.toy/bin "+
			"as a command name, and a name with spaces is unusable from a shell", name, section)
	}
	return checkPrintable(fmt.Sprintf("the name %q in [%s]", name, section), name)
}

// checkRelPath: the right-hand side must be a relative path inside the package, with no
// way out of it (iron laws 5 and 7).
func checkRelPath(section, name, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("%w: the path for %q in [%s] is empty. Write the relative path inside the "+
			"extracted package", ErrUnsafeTarget, name, section)
	case strings.HasPrefix(target, "/") || filepath.IsAbs(target):
		return fmt.Errorf("%w: %q in [%s] points at the absolute path %q. Only a path inside the package "+
			"belongs here -- an absolute path means something else entirely on someone else's machine",
			ErrUnsafeTarget, name, section, target)
	}
	for _, seg := range strings.Split(filepath.ToSlash(target), "/") {
		if seg == ".." {
			return fmt.Errorf(`%w: the path %q for %q in [%s] contains a ".." segment, which escapes the `+
				`package directory`, ErrUnsafeTarget, target, name, section)
		}
	}
	// [artifact] and [script] values are appended verbatim to the download URL
	// (host.ReleaseURL / RawURL), and ? and # are the query and fragment separators there:
	// "toy.tar.gz?x" actually requests "toy.tar.gz", "a#b" requests "a" -- a different file,
	// fetched silently, while the client keeps printing the name the author wrote in
	// toy.toml (iron law 8). fetch has always preferred erroring over guessing for file://;
	// this closes the same gap here.
	//
	// An absolute URL with a scheme trips none of the three checks above (no leading slash,
	// no ".." segment, no ?#), but it is just as much not "a relative path inside the
	// package". Spec §3 is blunt about it: [artifact] / [script] hold a release asset name
	// and NEVER a URL. Let one through and, on the bare-URL install path,
	// base.ResolveReference jumps straight to another machine -- or to file:///etc/shadow,
	// or 169.254.169.254 -- while the client still prints the source the author's toy.toml
	// claims. That line becomes a lie (iron law 8).
	if i := strings.Index(target, "://"); i > 0 && isURLScheme(target[:i]) {
		return fmt.Errorf("%w: the value %q for %q in [%s] is a URL. Only the release asset name "+
			"(or a path inside the package) belongs here -- toy builds the address itself from the host, "+
			"and a hardcoded URL would point somewhere other than the source toy.toml claims",
			ErrUnsafeTarget, target, name, section)
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		return fmt.Errorf("%w: the path %q for %q in [%s] contains %q. It is appended to the download URL, "+
			"where ? and # cut off everything after them, so a different file comes back. If the asset name "+
			"really contains that character, rename it in the release",
			ErrUnsafeTarget, target, name, section, string(target[i]))
	}
	return checkPrintable(fmt.Sprintf("the path for %q in [%s]", name, section), target)
}

// isURLScheme reports whether s is an RFC 3986 scheme: a letter followed by letters,
// digits, '+', '-' or '.'. Held strictly to the spec so an ordinary filename like
// "a:b://c" is not mistaken for a URL and rejected.
func isURLScheme(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// checkPlatformKey: the key must be GOOS/GOARCH, or GOOS/GOARCH/libc with a libc segment.
// A malformed key does not fail -- it just never matches, forever. So reject it on the
// spot: otherwise the author believes they shipped a Linux build, the user is told "no
// build for your platform", and nothing in between ever says a word.
func checkPlatformKey(section, plat string) error {
	// runtime.GOOS/GOARCH are always lowercase with no whitespace. "Linux/AMD64" or
	// " linux/amd64 " does not fail, it just never matches. Check this first, because the
	// segment comparison below is byte for byte.
	if plat != strings.ToLower(plat) || strings.ContainsAny(plat, " \t") {
		return fmt.Errorf("toyfile: the key %q in [%s] must be all lowercase with no whitespace -- it has to "+
			"equal Go's GOOS/GOARCH byte for byte to ever match, e.g. \"linux/amd64\"", plat, section)
	}
	parts := strings.Split(plat, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("toyfile: the key %q in [%s] is not in GOOS/GOARCH form -- "+
			`e.g. "linux/amd64", "darwin/arm64", or with a libc segment, "linux/amd64/musl"`, plat, section)
	}
	// No allowlist for the arch segment: every new GOARCH Go adds would turn an allowlist
	// into a bogus rejection, and the generation names GOARCH cannot express -- armv6,
	// armv7 -- have to be writable here in the first place.
	if len(parts) == 3 && parts[2] != "musl" && parts[2] != "gnu" {
		return fmt.Errorf("toyfile: the key %q in [%s] has an unrecognized third segment %q -- that segment "+
			"is the libc and can only be \"musl\" (Alpine and friends) or \"gnu\" (glibc). Anything else "+
			"will never match, so the line does nothing", plat, section, parts[2])
	}
	return nil
}

// checkPrintable: name/desc/license/homepage, command names and paths all get printed to
// the terminal verbatim, and the line right next to them on install is the trust level
// (spec §4). A \r plus an ANSI sequence erases a line already printed and rewrites it,
// forging "✓ signed · Derrity". Iron law 8 requires the trust level to be printed
// honestly, so control characters die here at parse time: there are several places that
// print, and only one that parses.
func checkPrintable(what, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("toyfile: %s contains the control character %q. These fields are printed to your "+
				"terminal verbatim, and newlines or escape sequences can overwrite the trust level line, "+
				"so none of them are accepted", what, r)
		}
	}
	return nil
}

func describeKeys(keys []toml.Key) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
