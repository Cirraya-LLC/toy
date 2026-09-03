// Package ref parses the package references users type on the command line.
// All four paths from spec §2 come through here.
package ref

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type Kind int

const (
	KindIndex Kind = iota // "Derrity/tinyserve"        — look it up in the index
	KindHost              // "github:Derrity/tinyserve" — read that host directly
	KindURL               // "https://…/toy.toml"       — no safety net at all
)

var ErrBadRef = errors.New("ref: cannot parse this package reference")

type Ref struct {
	Kind    Kind
	Host    string // KindHost only, e.g. "github"
	Owner   string
	Repo    string
	URL     string // KindURL only: the direct address of a toy.toml
	Version string // set only when the user wrote @v1.2.3; empty means latest
	Raw     string // the original input, for error messages
}

// owner / repo / version all get spliced into URLs and into paths under ~/.toy
// (spec §6), so they need a length cap too — otherwise one ref can produce a
// path nothing can handle.
const maxSegment = 100

// The URL form accepts only these three schemes (spec §5: an index source is
// file:// or https://; §2: a bare URL points straight at a toy.toml).
var allowedSchemes = map[string]bool{"http": true, "https": true, "file": true}

// Parse accepts these four shapes:
//
//	Derrity/tinyserve            KindIndex
//	Derrity/tinyserve@v1.2.3     KindIndex with a pinned version
//	github:Derrity/tinyserve     KindHost
//	https://…/toy.toml           KindURL
//
// owner/repo allow only [A-Za-z0-9._-], no empty segments, no more than one
// slash.
func Parse(s string) (Ref, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("%w: empty package reference; write it as owner/repo or github:owner/repo", ErrBadRef)
	}
	// Recognize the URL form first: the slashes and colons inside it follow
	// completely different rules from the two forms below.
	if strings.Contains(s, "://") {
		return parseURL(s, raw)
	}

	// @version only means something for the owner/repo form; a URL already
	// points at a specific file.
	body, version := s, ""
	if b, v, ok := strings.Cut(s, "@"); ok {
		body, version = b, v
		if err := validVersion(v); err != nil {
			return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
		}
	}

	r := Ref{Kind: KindIndex, Version: version, Raw: raw}

	// The host prefix. Note that a Windows drive letter like "C:foo" must not
	// be read as a host — hence: at least two characters, must start with a
	// letter.
	if h, rest, ok := strings.Cut(body, ":"); ok {
		if !validHostName(h) {
			return Ref{}, fmt.Errorf("%w: %q: %q is not a valid host prefix (a host name starts with a letter, "+
				"is at least two characters long, and contains only letters, digits, . and -, as in github:owner/repo)", ErrBadRef, s, h)
		}
		r.Kind = KindHost
		r.Host = strings.ToLower(h)
		body = rest
	}

	owner, repo, ok := strings.Cut(body, "/")
	if !ok {
		return Ref{}, fmt.Errorf("%w: %q: missing the slash; write it as owner/repo (for example Derrity/tinyserve)", ErrBadRef, s)
	}
	if strings.Contains(repo, "/") {
		return Ref{}, fmt.Errorf("%w: %q: only one slash is allowed — owner/repo is the whole reference (subdirectories are not supported)", ErrBadRef, s)
	}
	if err := validSegment("owner", owner); err != nil {
		return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
	}
	if err := validSegment("repo", repo); err != nil {
		return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
	}
	r.Owner, r.Repo = owner, repo
	return r, nil
}

func parseURL(s, raw string) (Ref, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %q: not a valid URL: %v", ErrBadRef, s, err)
	}
	if !allowedSchemes[u.Scheme] {
		return Ref{}, fmt.Errorf("%w: %q: only http://, https:// and file:// addresses are accepted, not %q", ErrBadRef, s, u.Scheme)
	}
	if u.Scheme == "file" {
		if u.Path == "" {
			return Ref{}, fmt.Errorf("%w: %q: a local file goes as file:///absolute/path/toy.toml (three slashes)", ErrBadRef, s)
		}
		// A host name after file:// is a trap: whoever reads this only uses
		// u.Path, so file://evil.com/etc/passwd reads the LOCAL /etc/passwd
		// while that line looks like a remote address in logs, in the install
		// manifest and in the trust level (spec §4 demands we say what we
		// actually did). RFC 8089 allows only an empty host or localhost;
		// reject anything else instead of quietly dropping it.
		if h := u.Hostname(); h != "" && !strings.EqualFold(h, "localhost") {
			return Ref{}, fmt.Errorf("%w: %q: file:// must not carry the host name %q — it is ignored, and what "+
				"actually gets read is the local file %q. Write local files as file:///absolute/path/toy.toml (three slashes)", ErrBadRef, s, h, u.Path)
		}
	} else if u.Hostname() == "" {
		// Hostname(), not Host(): the Host of "https://:8080/x" is ":8080" —
		// non-empty, but there is no host name in it.
		return Ref{}, fmt.Errorf("%w: %q: missing the host name; write it as https://host/path/toy.toml", ErrBadRef, s)
	}
	// Host names are case-insensitive, so lowercase them: the URL becomes the
	// key of the download cache and of the TOFU record (spec §4, §6). Two
	// addresses differing only in case must be one key, or Example.COM can slip
	// past the hash pinned for example.com.
	u.Host = strings.ToLower(u.Host)
	// Store u.String() rather than the input: scheme and host are normalized,
	// and the result is guaranteed to parse again.
	return Ref{Kind: KindURL, URL: u.String(), Raw: raw}, nil
}

// validSegment is the character allowlist for owner/repo. These values get
// spliced into download URLs and into filesystem paths like
// ~/.toy/pkg/<host>/<owner>/<name>/, so be strict: ".." and empty segments are
// path traversal, and slashes or backslashes conjure up an extra directory
// level.
func validSegment(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", what)
	case s == "." || s == "..":
		return fmt.Errorf("%s cannot be %q", what, s)
	case len(s) > maxSegment:
		return fmt.Errorf("%s is too long (%d characters max)", what, maxSegment)
	}
	for _, c := range s {
		if !isSegmentChar(c) {
			return fmt.Errorf("%s %q contains the character %q, which is not allowed (letters, digits and . _ - only)", what, s, string(c))
		}
	}
	return nil
}

// validVersion: a version also becomes a directory level under pkg, so the
// rules match owner/repo, plus one extra character — semver's build metadata
// separator '+'.
func validVersion(s string) error {
	switch {
	case s == "":
		return errors.New("write a version after the @, as in @v1.2.3")
	case s == "." || s == "..":
		return fmt.Errorf("a version cannot be %q", s)
	case len(s) > maxSegment:
		return fmt.Errorf("version is too long (%d characters max)", maxSegment)
	}
	for _, c := range s {
		if !isSegmentChar(c) && c != '+' {
			return fmt.Errorf("version %q contains the character %q, which is not allowed (letters, digits and . _ - + only)", s, string(c))
		}
	}
	return nil
}

func isSegmentChar(c rune) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// validHostName validates a name from the host table (github / gitlab /
// codeberg / …), not a domain name. The two-character minimum exists so that a
// Windows drive letter like "C:foo" cannot land here.
func validHostName(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9' || c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ID returns the canonical "owner/repo", which the index uses as its key.
func (r Ref) ID() string {
	if r.Owner == "" || r.Repo == "" {
		return ""
	}
	return r.Owner + "/" + r.Repo
}

// String returns the canonical form, which is guaranteed to parse again.
func (r Ref) String() string {
	if r.Kind == KindURL {
		return r.URL
	}
	s := r.ID()
	if r.Kind == KindHost && r.Host != "" {
		s = r.Host + ":" + s
	}
	if r.Version != "" {
		s += "@" + r.Version
	}
	return s
}
