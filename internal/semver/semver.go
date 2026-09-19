// Package semver is a good-enough semantic version comparator.
// Spec §10: no third-party libraries — this is three integers plus an optional
// pre-release tag, compared.
package semver

import (
	"cmp"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// maxLen matches maxSegment in internal/ref: a version becomes a directory
// level under ~/.toy/pkg, and a single path component is capped at 255 bytes on
// most filesystems. Tags come from git ls-remote, so upstream can make them as
// long as it likes — this gate has to be here.
const maxLen = 100

// V is a parsed version. An empty Pre means a stable release.
type V struct {
	Major, Minor, Patch int
	Pre                 string // without the leading '-'
	Raw                 string // the original tag, possibly with a 'v' prefix
	// Prefix is a package-name prefix that was stripped off Raw, "jq-" for the
	// tag "jq-1.8.2". Empty for an ordinary tag. Only Max's fallback pass ever
	// sets it; Parse stays strict.
	Prefix string
}

// Parse accepts "v1.2.3" / "1.2.3" / "1.2.3-rc.1".
// An invalid tag returns an error — callers should skip it, never treat it as
// 0.0.0.
func Parse(s string) (V, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return V{}, errors.New("version is empty")
	}
	if len(raw) > maxLen {
		return V{}, fmt.Errorf("version is too long (%d characters, %d max)", len(raw), maxLen)
	}
	body := strings.TrimPrefix(raw, "v")
	// Build metadata (everything after '+') takes no part in comparison, but its
	// CHARACTERS MUST STILL BE VALIDATED: Raw goes verbatim into the directory
	// name under ~/.toy/pkg (spec §6) and into the download URL that Number()
	// builds (§2.4), and "v1.2.3+a/b" is a tag git will genuinely let you push.
	// Not validating it means letting upstream inject slashes into our paths and
	// URLs. Validate, then discard.
	if i := strings.IndexByte(body, '+'); i >= 0 {
		if err := checkIdents(body[i+1:]); err != nil {
			return V{}, fmt.Errorf("build metadata in %q is invalid: %w", raw, err)
		}
		body = body[:i]
	}
	var pre string
	if i := strings.IndexByte(body, '-'); i >= 0 {
		// A hyphen implies a pre-release tag, so validate inside this branch:
		// "1.0.0-" leaves pre empty, and it must never slip through on the
		// theory that "empty tag == stable release".
		pre, body = body[i+1:], body[:i]
		if err := checkIdents(pre); err != nil {
			return V{}, fmt.Errorf("pre-release tag in %q is invalid: %w", raw, err)
		}
	}

	nums := strings.Split(body, ".")
	if len(nums) != 3 {
		return V{}, fmt.Errorf("%q is not semver: a version is three numbers, major.minor.patch, as in v1.2.3", raw)
	}
	v := V{Pre: pre, Raw: raw}
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := parseNum(nums[i])
		if err != nil {
			return V{}, fmt.Errorf("%q is not semver: %w", raw, err)
		}
		*dst = n
	}
	return v, nil
}

// parseNum accepts digits only, so "-2", "x" and "" are all rejected.
func parseNum(s string) (int, error) {
	if s == "" {
		return 0, errors.New("one of the three number parts is empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q is not a number", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is too large a number", s)
	}
	return n, nil
}

// checkIdents validates dot-separated identifiers: every part non-empty, and
// letters, digits and hyphens only. Pre-release tags and build metadata share
// one grammar in semver, so they share this one check.
func checkIdents(s string) error {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return errors.New("one of the dot-separated parts is empty (no doubled or trailing dots)")
		}
		for _, c := range id {
			ok := c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				return fmt.Errorf("%q contains the illegal character %q; only letters, digits and hyphens are allowed", id, c)
			}
		}
	}
	return nil
}

// Compare returns -1 / 0 / 1. A pre-release sorts before the stable release of
// the same number (1.0.0-rc < 1.0.0).
func Compare(a, b V) int {
	// cmp.Compare rather than subtraction: V's fields are exported, so a
	// hand-built negative value would overflow the subtraction and invert the
	// ordering.
	if c := cmp.Compare(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Patch, b.Patch); c != 0 {
		return c
	}
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1 // a stable release beats the pre-release of the same number
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre compares pre-release tags part by part: all-digit parts compare
// numerically (rc.2 < rc.10), numeric parts rank below parts containing
// letters, and more parts wins (rc.1 < rc.1.1).
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

func compareIdent(a, b string) int {
	an, bn := allDigits(a), allDigits(b)
	switch {
	case an && bn:
		return compareNumeric(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// compareNumeric compares two digit strings by value without strconv, so no
// number is ever long enough to overflow.
func compareNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Max picks the highest stable version out of a pile of tags, and errors when
// none of them parse. Pre-releases are ignored unless allowPre — installing
// something should not hand you an rc by default.
// Max picks the highest release among a repository's tags.
//
// repo is the repository name ("gping"), and it is the discriminator that makes
// prefixed tags safe. Pass "" when it is not known; the prefix rules below then
// fall through to the conservative single-prefix case.
func Max(tags []string, allowPre bool, repo string) (V, error) {
	// Candidates are the bare semver tags PLUS any prefixed tag whose prefix is the
	// repository's own name. That pairing is the whole rule, and both halves come
	// from a real repository:
	//
	//   ripgrep tags its releases bare (15.2.0) and its sub-crates with prefixes
	//   (wincolor-0.1.6). Let a sub-crate compete and one sitting on a higher
	//   version silently becomes the release we install, from its tag's URL.
	//
	//   gping migrated the other way: its releases are gping-v1.21.0 today, and the
	//   bare v1.0.0..v1.2.1 tags are 2021 leftovers. "Bare always wins" installed a
	//   three-year-old version from an asset name that no longer exists.
	//
	// A prefix equal to the repository name is the project talking about itself.
	// Any other prefix is a different project sharing the repository.
	var cands []V
	var skippedPre int
	for _, t := range tags {
		v, err := Parse(t)
		if err != nil {
			pv, perr := parsePrefixed(t)
			if perr != nil || !PrefixNamesRepo(pv.Prefix, repo) {
				continue // non-semver tags are everywhere; skip rather than treat as 0.0.0
			}
			v = pv
		}
		if v.Pre != "" && !allowPre {
			skippedPre++
			continue
		}
		cands = append(cands, v)
	}
	if best, ok := highest(cands); ok {
		return best, nil
	}

	// Nothing bare and nothing named after the repository. A repository whose tags
	// all carry one other prefix is still perfectly installable, so accept that --
	// but only one prefix: across two projects there is no highest version, and
	// picking one installs whichever sub-project happens to be furthest ahead.
	byPrefix := map[string][]V{}
	for _, t := range tags {
		v, err := parsePrefixed(t)
		if err != nil {
			continue
		}
		if v.Pre != "" && !allowPre {
			skippedPre++
			continue
		}
		byPrefix[v.Prefix] = append(byPrefix[v.Prefix], v)
	}
	switch len(byPrefix) {
	case 1:
		for _, vs := range byPrefix {
			if best, ok := highest(vs); ok {
				return best, nil
			}
		}
	case 0:
		// fall through to the errors below
	default:
		names := make([]string, 0, len(byPrefix))
		for k := range byPrefix {
			names = append(names, strings.TrimSuffix(k, "-"))
		}
		sort.Strings(names)
		return V{}, fmt.Errorf("no plain semver tag here, and the prefixed ones belong to %d different projects (%s) -- picking a highest across them would install whichever sub-project happens to be furthest ahead. Pass --version with the exact tag you want",
			len(names), strings.Join(names, ", "))
	}

	if skippedPre > 0 {
		return V{}, fmt.Errorf("found %d pre-release version(s) but no stable release; pass --version with a specific tag to install a pre-release", skippedPre)
	}
	return V{}, fmt.Errorf("none of these %d tags is a semver version (they should look like v1.2.3, or carry one after the package name like jq-1.8.2); this repository may not have tagged a release yet", len(tags))
}

func highest(vs []V) (V, bool) {
	var best V
	found := false
	for _, v := range vs {
		if !found || Compare(v, best) > 0 {
			best, found = v, true
		}
	}
	return best, found
}

// PrefixNamesRepo reports whether a tag prefix names this repository. Compared with
// '-', '_' and '.' ignored and case folded, because gping-v1.21.0, gping_1.21.0
// and GPing-1.21.0 are all the project talking about itself.
func PrefixNamesRepo(prefix, repo string) bool {
	if repo == "" {
		return false
	}
	return foldName(prefix) == foldName(repo)
}

func foldName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if c == '-' || c == '_' || c == '.' {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// parsePrefixed accepts "<name>-<semver>", the shape jq uses. The prefix has to
// look like a package name and nothing else: Raw ends up in the download URL and
// Number() feeds the directory under ~/.toy/pkg, so a prefix carrying a slash or
// a dot-dot is upstream choosing our paths for us.
func parsePrefixed(s string) (V, error) {
	raw := strings.TrimSpace(s)
	i := strings.LastIndexByte(raw, '-')
	if i <= 0 || i == len(raw)-1 {
		return V{}, fmt.Errorf("%q has no package-name prefix", raw)
	}
	// The last '-' may belong to the version itself ("mytool-1.2.3-rc.1"), so walk
	// left until what follows parses.
	for i > 0 {
		prefix, rest := raw[:i+1], raw[i+1:]
		if v, err := Parse(rest); err == nil && validPrefix(prefix) {
			v.Raw, v.Prefix = raw, prefix
			return v, nil
		}
		i = strings.LastIndexByte(raw[:i], '-')
	}
	return V{}, fmt.Errorf("%q is not <name>-<semver>", raw)
}

// validPrefix keeps the prefix to characters that are safe in a path and a URL.
func validPrefix(p string) bool {
	if p == "" || len(p) > maxLen || strings.Contains(p, "..") {
		return false
	}
	for _, c := range p {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// Number returns the version without the 'v' prefix, for the {version}
// placeholder in toy.toml.
func (v V) Number() string {
	// Prefer stripping 'v' off the original tag: the author's release asset
	// names are derived from that tag, so rebuilding the string from parts can
	// fail to match (a tag carrying build metadata, for instance).
	if v.Raw != "" {
		return strings.TrimPrefix(strings.TrimPrefix(v.Raw, v.Prefix), "v")
	}
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
