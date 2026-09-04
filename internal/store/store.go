// Package store owns everything under ~/.toy: the directory layout, config.toml,
// install manifests, and the symlinks. Spec §6.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/Cirraya-LLC/toy/internal/host"
)

var (
	ErrShadow       = errors.New("store: that command name would shadow a program already on PATH")
	ErrNotInstalled = errors.New("store: package is not installed")
	ErrLinkExists   = errors.New("store: a link with that name already exists")
)

// Trust levels. These get printed to the user verbatim (iron law 8).
const (
	TrustSigned = "signed" // index entry + author signature
	TrustHost   = "host"   // the host's own account system
	// TrustIndexed means "the bytes come from the author's release, but the
	// packaging instructions come from the index": the upstream repo has no
	// toy.toml, so which asset to install and what to call the command is what
	// the index entry's inlined toyfile says. It ranks below TrustHost (that
	// declaration was not written by the author) and above TrustUnverified (the
	// bytes still come from the author's own release; the URL is built from the
	// host template, not from anything the index says).
	//
	// AN INLINED TOYFILE CAN NEVER REACH TrustSigned: the signer is whoever
	// submitted the index record, not the author, so that checkmark would claim
	// more than it can (§14.3, and rule 2 on index.Entry.Toyfile).
	TrustIndexed    = "index-toyfile"
	TrustUnverified = "unverified" // bare URL
)

// TrustRank puts the trust levels on one line, higher is more trusted.
// Downgrade checks and display both compare through this. Never hardcode
// "equals TrustSigned means high" anywhere again: when a level gets added, not
// one of those sites errors out, they just silently file the new level on the
// wrong side of the line. That is exactly how TrustIndexed got added.
//
// An unrecognized level ranks lowest. Iron law 8 forbids silent success, so
// anything we cannot explain does not get to count as the safe end.
func TrustRank(level string) int {
	switch level {
	case TrustSigned:
		return 4
	case TrustHost:
		return 3
	case TrustIndexed:
		return 2
	case TrustUnverified:
		return 1
	default:
		return 0
	}
}

// DefaultIndexURL is the address of the built-in index. THIS IS THE ONE
// CONSTANT: once the index is actually hosted somewhere, filling in the address
// here is the entire change, nothing else moves (spec §5: an index source "is
// just a URL"; file:// and https:// take the same code path).
//
// It is empty right now, meaning NO INDEX IS PRECONFIGURED. The v1 index is the
// local file in this repo (index/toy-index.json) and it has not been uploaded to
// any static host yet. Shipping an address that 404s would hand every new user a
// red cross on their first `toy doctor` / `toy search`, and that is not their
// problem (iron law 8: never report our own state as the user's fault). Having no
// index is a legal state: `toy install github:owner/repo` never touches an index.
const DefaultIndexURL = ""

const defaultIndexName = "default"

// Top-level directories under ~/.toy. Spec §6.
const (
	dirBin       = "bin"
	dirPkg       = "pkg"
	dirCache     = "cache"
	dirInstalled = "installed"
	dirKeys      = "keys"
	dirKnown     = "known"
	dirIndex     = "index"
	configName   = "config.toml"
)

// Config is ~/.toy/config.toml.
type Config struct {
	Indexes []IndexRef           `toml:"index"`
	Mirrors []string             `toml:"mirrors"`
	Hosts   map[string]host.Host `toml:"hosts"` // override or add a host; three lines supports one more
}

type IndexRef struct {
	Name string `toml:"name"`
	URL  string `toml:"url"` // file:// or https://; file:// is enough for v1, one line to change later
}

// DefaultConfig returns the built-in defaults: the one index when
// DefaultIndexURL is set, and no index at all when it is empty.
func DefaultConfig() Config {
	c := Config{Indexes: []IndexRef{}, Mirrors: []string{}, Hosts: map[string]host.Host{}}
	if DefaultIndexURL != "" {
		c.Indexes = append(c.Indexes, IndexRef{Name: defaultIndexName, URL: DefaultIndexURL})
	}
	return c
}

type Store struct{ Root string }

// Open opens (and creates if needed) the store. An empty root means ~/.toy,
// overridden by the TOY_HOME environment variable, which the tests rely on.
func Open(root string) (*Store, error) {
	if root == "" {
		root = os.Getenv("TOY_HOME")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("store: no home directory, and TOY_HOME is not set — "+
				"set TOY_HOME=/some/dir and retry: %w", err)
		}
		root = filepath.Join(home, ".toy")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: cannot resolve %q to an absolute path: %w", root, err)
	}
	s := &Store{Root: abs}

	// keys/ holds private keys, so it is owner-only. Everything else is 0755.
	for _, d := range []string{dirBin, dirPkg, dirCache, dirInstalled, dirKnown, dirIndex} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
			return nil, fmt.Errorf("store: cannot create directory %s: %w", filepath.Join(abs, d), err)
		}
	}
	if err := os.MkdirAll(s.KeysDir(), 0o700); err != nil {
		return nil, fmt.Errorf("store: cannot create directory %s: %w", s.KeysDir(), err)
	}
	// MkdirAll is a no-op on a directory that already exists, so a keys/ that was
	// created 0755 at some point stays wide open, with private keys in it. If
	// tightening fails, say so; do not look the other way (iron law 8).
	if fi, err := os.Stat(s.KeysDir()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(s.KeysDir(), 0o700); err != nil {
			return nil, fmt.Errorf("store: %s is mode %o, so other users can read the private keys "+
				"in it, and tightening it failed: %w", s.KeysDir(), fi.Mode().Perm(), err)
		}
	}
	return s, nil
}

func (s *Store) BinDir() string   { return filepath.Join(s.Root, dirBin) }
func (s *Store) CacheDir() string { return filepath.Join(s.Root, dirCache) }
func (s *Store) KeysDir() string  { return filepath.Join(s.Root, dirKeys) }
func (s *Store) KnownDir() string { return filepath.Join(s.Root, dirKnown) }
func (s *Store) IndexDir() string { return filepath.Join(s.Root, dirIndex) }

// PkgDir is pkg/<host>/<owner>/<name>/<version>/
//
// Every segment goes through safeSeg. These values come straight from toy.toml
// and from user input, so a single "../.." would move the extraction target
// outside ~/.toy (the same class of hole as iron law 7's path traversal), and two
// different segments escaping to the same name would make two packages share one
// install directory.
func (s *Store) PkgDir(hostName, owner, name, version string) string {
	return filepath.Join(s.Root, dirPkg,
		safeSeg(hostName), safeSeg(owner), safeSeg(name), safeSeg(version))
}

func (s *Store) configPath() string { return filepath.Join(s.Root, configName) }

// LoadConfig reads config.toml. A missing file means a brand-new store, so the
// built-in defaults come back.
func (s *Store) LoadConfig() (Config, error) {
	data, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("store: cannot read %s: %w", s.configPath(), err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("store: %s is not valid TOML: %w", s.configPath(), err)
	}
	if c.Mirrors == nil {
		c.Mirrors = []string{}
	}
	if c.Hosts == nil {
		c.Hosts = map[string]host.Host{}
	}
	// Host.Name is `toml:"-"`: the name lives in the table header ([hosts.github]),
	// not inside the table, so it decodes as empty. Fill it back in here. Lookup
	// needs it to name the host in its errors.
	for name, h := range c.Hosts {
		h.Name = name
		c.Hosts[name] = h
	}
	return c, nil
}

func (s *Store) SaveConfig(c Config) error {
	data, err := encodeTOML("# toy config: indexes, mirrors, and git host overrides.", c)
	if err != nil {
		return fmt.Errorf("store: cannot serialize config: %w", err)
	}
	if err := writeFileAtomic(s.configPath(), data, 0o644); err != nil {
		return fmt.Errorf("store: cannot write %s: %w", s.configPath(), err)
	}
	return nil
}

// encodeTOML encodes v as TOML with one human-readable comment on top.
func encodeTOML(header string, v any) ([]byte, error) {
	body, err := toml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(header+"\n\n"), body...), nil
}

// Manifest is the full record of one install. Without it there is no uninstall
// and no update.
type Manifest struct {
	ID string `toml:"id"` // "github:Derrity/tinyserve"
	// IndexID is this package's id in the index ("Derrity/tinyserve"), set only
	// when it was installed through an index. Without it `toy update` can only
	// take ID down the host path, so the author's signature disappears forever
	// after the first upgrade: a package manager that drops a trust level every
	// time you update (iron law 8).
	IndexID     string   `toml:"index_id"`
	Name        string   `toml:"name"`
	Version     string   `toml:"version"`
	Tag         string   `toml:"tag"`
	Host        string   `toml:"host"`
	Owner       string   `toml:"owner"`
	Repo        string   `toml:"repo"`
	Trust       string   `toml:"trust"`  // one of the four Trust* constants above
	Signer      string   `toml:"signer"` // public key fingerprint, empty when unsigned
	SHA256      string   `toml:"sha256"`
	Source      string   `toml:"source"` // the URL the download actually succeeded from
	PkgDir      string   `toml:"pkg_dir"`
	Links       []string `toml:"links"`        // absolute paths of the symlinks we created
	InstalledAt string   `toml:"installed_at"` // RFC3339
}

// manifestPath turns an id into one filename under installed/.
//
// An id looks like "github:Derrity/tinyserve", and using its : and / directly as
// a filename would write into some other directory. So: replace everything
// outside the whitelist with _, then append the first 8 hex of the id's sha256.
// The escaping is many-to-one (a/b and a:b collide); the hash suffix hands
// uniqueness back.
func (s *Store) manifestPath(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("store: package id is empty, cannot locate its install manifest")
	}
	sum := sha256.Sum256([]byte(id))
	file := safeSlug(id) + "-" + hex.EncodeToString(sum[:])[:8] + ".toml"
	return filepath.Join(s.Root, dirInstalled, file), nil
}

// safeSlug turns arbitrary text into something safe to use as a file or
// directory name. Anything outside the whitelist becomes _, so / and : and ..
// cannot escape the current directory.
//
// Uppercase is folded to lowercase too, because APFS on macOS is case-insensitive
// by default: "Foo" and "foo" are THE SAME DIRECTORY there. Without folding,
// Derrity/tinyserve and derrity/TinySERVE would get two pkg directories that look
// different and are actually one, two packages sharing one set of files, and
// uninstalling either would delete the other's executable. Folding is many-to-one
// just like escaping is; the caller hands uniqueness back by appending a hash of
// the original string (see safeSeg and manifestPath).
func safeSlug(s string) string {
	if s == "" {
		return "_"
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b = append(b, byte(r)+('a'-'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	// All-dots ("." and "..") mean something special to the filesystem. Replace it.
	if strings.Trim(string(b), ".") == "" {
		b = []byte(strings.Repeat("_", len(b)))
	}
	const maxName = 96 // room for the hash suffix and extension; all ASCII, so truncation cannot split a character
	if len(b) > maxName {
		b = b[:maxName]
	}
	return string(b)
}

// safeSeg is safeSlug plus "append a hash if anything changed".
//
// Escaping and case folding are both many-to-one: owners "a/b" and "a:b" both
// become "a_b", "Foo" and "foo" both become "foo", and two names longer than
// maxName are identical once truncated. Two different packages would then share
// one install directory: installing the second overwrites the first's files, and
// uninstalling the second deletes the first's executable. manifestPath has solved
// this all along (see the comment there); same trick here. A slug that matches the
// original byte for byte is kept as-is, so ordinary paths still read like
// pkg/github/derrity-1f0a3c7d/tinyserve/1.2.3.
func safeSeg(s string) string {
	slug := safeSlug(s)
	if slug == s {
		return slug
	}
	sum := sha256.Sum256([]byte(s))
	return slug + "-" + hex.EncodeToString(sum[:])[:8]
}

// redactURL replaces the password in a URL with xxxxx (via the stdlib's
// URL.Redacted).
//
// A user is perfectly free to type `toy install https://u:hunter2@example.com/toy.toml`.
// fetch needs the URL verbatim to authenticate, but a manifest is a file that sits
// on disk long-term and gets printed. A package manager that writes your password
// into ~/.toy/installed/*.toml on one install does not get to call itself a
// security boundary. Anything that is not a URL (unparseable, or simply has no
// password) comes back untouched.
func redactURL(s string) string {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return s // the vast majority of URLs carry no credentials, no need to parse
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); !ok {
		return s
	}
	return u.Redacted()
}

func (s *Store) SaveManifest(m Manifest) error {
	path, err := s.manifestPath(m.ID)
	if err != nil {
		return err
	}
	// Strip the password before it hits disk: the manifest is a file that lives on
	// disk and gets printed by `toy list`, while fetch uses the verbatim URL that
	// is still in memory and is unaffected.
	m.Source = redactURL(m.Source)
	data, err := encodeTOML("# toy install manifest, generated. Delete it and this package can no longer be uninstalled or updated.", m)
	if err != nil {
		return fmt.Errorf("store: cannot serialize the install manifest: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("store: cannot write install manifest %s: %w", path, err)
	}
	return nil
}

func (s *Store) LoadManifest(id string) (Manifest, error) {
	path, err := s.manifestPath(id)
	if err != nil {
		return Manifest{}, err
	}
	m, err := readManifest(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%w: %s. Run `toy list` to see what is installed", ErrNotInstalled, id)
	}
	return m, err
}

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("store: install manifest %s is corrupt: %w (deleting it recovers, but you will have to clean that package up by hand)", path, err)
	}
	return m, nil
}

// ListManifests returns every install manifest, sorted by id. A manifest we
// cannot read is an error, not a skip: skipping it silently makes that package
// impossible to uninstall.
func (s *Store) ListManifests() ([]Manifest, error) {
	dir := filepath.Join(s.Root, dirInstalled)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: cannot read %s: %w", dir, err)
	}
	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		m, err := readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) RemoveManifest(id string) error {
	path, err := s.manifestPath(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotInstalled, id)
	}
	if err != nil {
		return fmt.Errorf("store: cannot remove install manifest %s: %w", path, err)
	}
	return nil
}

// FindByName looks up an installed package by command name or package name, for
// `toy uninstall tinyserve`. On more than one hit it returns an error listing the
// candidates so the user can retype the full id.
func (s *Store) FindByName(name string) (Manifest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Manifest{}, fmt.Errorf("store: the name to look up is empty")
	}
	all, err := s.ListManifests()
	if err != nil {
		return Manifest{}, err
	}

	var hits []Manifest
	for _, m := range all {
		// An id is unique, so spelling it out leaves no ambiguity. Direct hit.
		if m.ID == name {
			return m, nil
		}
		if manifestMatches(m, name) {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Manifest{}, fmt.Errorf("%w: %q. Run `toy list` to see what is installed", ErrNotInstalled, name)
	default:
		ids := make([]string, 0, len(hits))
		for _, m := range hits {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return Manifest{}, fmt.Errorf("store: %q matches %d installed packages: %s. "+
			"Retry with one of those full ids", name, len(hits), strings.Join(ids, ", "))
	}
}

// manifestMatches: package name, repo name, or the basename of any symlink it created.
func manifestMatches(m Manifest, name string) bool {
	if m.Name == name || m.Repo == name {
		return true
	}
	for _, l := range m.Links {
		if filepath.Base(l) == name {
			return true
		}
	}
	return false
}

// Link creates a symlink in BinDir pointing at target.
// Iron law 5: if name already exists on PATH (and is not one of ours in BinDir),
// return ErrShadow unless force. This is what closes off bin = "sudo" and the
// rest of that privilege-escalation family.
func (s *Store) Link(name, target string, force bool) (string, error) {
	if err := validLinkName(name); err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("store: cannot resolve link target %q to an absolute path: %w", target, err)
	}
	// A relative target resolves against bin/, which is essentially never what the
	// author meant. While we are here, confirm it actually exists, so we do not
	// create a dangling link the user only discovers as "command not found".
	fi, err := os.Stat(absTarget)
	if err != nil {
		return "", fmt.Errorf("store: link target %s is unusable — the package may not contain that file; "+
			"check the relative path on the right of toy.toml's [bin]: %w", absTarget, err)
	}
	// Linking to a directory produces a command that exists but cannot run, which
	// is broken the same way a dangling link is.
	if fi.IsDir() {
		return "", fmt.Errorf("store: link target %s is a directory, not an executable — "+
			"toy.toml's [bin] must point at a file", absTarget)
	}

	binDir := s.BinDir()
	linkPath := filepath.Join(binDir, name)

	// The shadow check comes before the exists check: it is the security verdict,
	// the one the user has to waive by hand with --force.
	if !force {
		// When exec.LookPath finds something inside a relative PATH entry ("." or,
		// far more common, the empty entry in PATH), Go 1.19+ returns it together
		// with exec.ErrDot. Checking only err == nil lets that one relative entry
		// silently switch off the shadow check for the whole of PATH, and
		// PATH="…:" is written like that everywhere. It is still a program of the
		// same name on PATH, so it still counts as shadowing.
		if found, err := exec.LookPath(name); err == nil || errors.Is(err, exec.ErrDot) {
			if abs, aerr := filepath.Abs(found); aerr == nil {
				found = abs
			}
			if !sameDir(filepath.Dir(found), binDir) {
				return "", fmt.Errorf("%w: %q on PATH is already %s. Installing this would shadow it; "+
					"pass --force if you really mean to override it", ErrShadow, name, found)
			}
		}
		if _, err := os.Lstat(linkPath); err == nil {
			return "", fmt.Errorf("%w: %s already exists. Run `toy uninstall` on that package, or pass --force to replace it",
				ErrLinkExists, linkPath)
		}
	}

	// Replacing an existing symlink means this name changed owner. Another
	// package's manifest may still list it, and `toy uninstall that-package` will
	// act on that record as "delete my own link" — deleting the current package's
	// command instead. The new package's manifest is untouched, `toy list` keeps
	// claiming it is installed, and doctor cannot see it either (that is not a
	// dangling symlink, it is nothing at all). So drop the stale record before
	// creating the link. BEFORE, so that a failure here has changed nothing.
	if _, err := os.Lstat(linkPath); err == nil {
		if err := s.disownLink(linkPath, absTarget); err != nil {
			return "", err
		}
	}

	if err := symlinkAtomic(absTarget, linkPath); err != nil {
		return "", fmt.Errorf("store: cannot create symlink %s: %w", linkPath, err)
	}
	return linkPath, nil
}

// disownLink removes linkPath from every install manifest that no longer owns it.
// Ownership is decided by where the symlink points right now: whichever package's
// PkgDir the target falls inside is the package that owns the name.
func (s *Store) disownLink(linkPath, newTarget string) error {
	all, err := s.ListManifests()
	if err != nil {
		return err
	}
	for _, m := range all {
		if withinDir(m.PkgDir, newTarget) {
			continue // this package is reinstalling/upgrading itself, the record is current
		}
		kept := make([]string, 0, len(m.Links))
		for _, l := range m.Links {
			if !sameFile(l, linkPath) {
				kept = append(kept, l)
			}
		}
		if len(kept) == len(m.Links) {
			continue
		}
		m.Links = kept
		if err := s.SaveManifest(m); err != nil {
			return fmt.Errorf("store: %s changed owner, but %s's install manifest cannot be updated — "+
				"installing anyway means uninstalling %s would delete this link: %w", linkPath, m.ID, m.ID, err)
		}
	}
	return nil
}

// sameFile compares two paths for being the same filename. It does not
// dereference the symlink itself, which may well be dangling.
func sameFile(a, b string) bool {
	if absA, err := filepath.Abs(a); err == nil {
		a = absA
	}
	if absB, err := filepath.Abs(b); err == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// withinDir reports whether path is inside dir (or is dir). An empty dir is
// always false.
func withinDir(dir, path string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// reservedLinkNames are the programs toy itself execs BY NAME off PATH.
//
// The shadow check only blocks a program of the same name that ALREADY EXISTS on
// PATH (LookPath has to find it). So on a machine with no git installed — macOS
// without the Xcode CLT, a slim container, a CI base image, all common, and the
// bare-URL install path does not need git at all — a toy.toml with
// [bin] git = "payload" claims bin/git cleanly. And bin/ is exactly the directory
// doctor tells the user to put FIRST on PATH. Not one line ran at install time
// (iron law 1 holds, literally), but the very next `toy install github:…` /
// `toy update` / `toy publish` / `toy doctor` execs it as git. Execution was only
// deferred by one command.
//
// --force DOES NOT WAIVE THIS. It is not the same class of risk as
// bin = "sudo": staring at "pass --force if you really mean it", a user cannot
// work out the consequence, and no package has a legitimate reason to claim a
// name out of toy's own toolchain. Every program toy learns to exec adds a line
// here.
var reservedLinkNames = map[string]bool{"git": true}

func validLinkName(name string) error {
	// Fold to lowercase before comparing: APFS on macOS and Windows are both
	// case-insensitive, where bin/GIT simply is bin/git.
	if reservedLinkNames[strings.ToLower(name)] {
		return fmt.Errorf("store: command name %q is a program toy itself executes — letting it land in %s "+
			"means the next toy command runs a file out of this package. --force does not waive this one", name, dirBin)
	}
	switch {
	case name == "":
		return fmt.Errorf("store: command name is empty (in toy.toml's [bin], the key is the name that lands on PATH)")
	case name == "." || name == "..":
		return fmt.Errorf("store: %q is not a usable command name", name)
	case strings.ContainsRune(name, os.PathSeparator), strings.ContainsRune(name, '/'):
		return fmt.Errorf("store: command name %q cannot contain a slash — it is a name on PATH, not a path", name)
	case strings.TrimSpace(name) != name:
		// A name with leading or trailing whitespace cannot be typed in a shell,
		// and it looks exactly like an ordinary name: "sudo " both installs and
		// slips past the shadow check (LookPath("sudo ") finds nothing).
		return fmt.Errorf("store: command name %q has leading or trailing whitespace — nobody can type that name", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("store: command name %q contains control characters — that is not a name anyone can type", name)
		}
	}
	return nil
}

// symlinkAtomic creates a temp name first, then renames it into place.
// Not remove-then-symlink: that leaves a window where the command does not
// exist, and this name is quite possibly in use by another process (or another
// toy) right then.
func symlinkAtomic(target, linkPath string) error {
	dir := filepath.Dir(linkPath)
	var tmp string
	for i := 0; ; i++ {
		var buf [6]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return err
		}
		tmp = filepath.Join(dir, "."+filepath.Base(linkPath)+".tmp"+hex.EncodeToString(buf[:]))
		err := os.Symlink(target, tmp)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || i >= 8 {
			return err
		}
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Unlink deletes a symlink we created.
// It only ever deletes things inside BinDir: a manifest is a file on disk, and a
// corrupt manifest must not become authorization to "delete /usr/bin/sudo".
// Already gone counts as deleted, because uninstall has to be re-runnable.
func (s *Store) Unlink(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("store: cannot resolve %q to an absolute path: %w", path, err)
	}
	binDir := s.BinDir()
	if !sameDir(filepath.Dir(abs), binDir) {
		return fmt.Errorf("store: refusing to delete %s — it is not inside %s, and toy only deletes symlinks it created itself", abs, binDir)
	}
	err = os.Remove(abs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: cannot delete %s: %w", abs, err)
	}
	return nil
}

// InPath reports whether BinDir is already on the user's PATH. Used by toy doctor.
func (s *Store) InPath() bool { return onPath(s.BinDir(), os.Getenv("PATH")) }

func onPath(dir, pathEnv string) bool {
	want := canonDir(dir)
	for _, e := range filepath.SplitList(pathEnv) {
		if e == "" {
			e = "." // POSIX: an empty entry in PATH means the current directory
		}
		if canonDir(e) == want {
			return true
		}
	}
	return false
}

func sameDir(a, b string) bool { return canonDir(a) == canonDir(b) }

// canonDir normalizes a directory name as best it can: absolute first (PATH is
// allowed to contain relative entries), then resolve symlinks (/usr/bin is
// frequently a symlink to /bin). If that fails, fall back to string comparison.
func canonDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// writeFileAtomic writes a temp file in the same directory and renames it, so a
// power cut mid-write cannot leave half a manifest behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has succeeded
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	// rename only makes the metadata atomic, it does not guarantee the contents
	// reached disk. Without this fsync a power cut can leave a zero-length
	// manifest behind, which is precisely what this function claims to prevent.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CacheEntry is one file in the download cache. The name is the sha256 of the
// contents, which is what makes the cache directory a mirror as it stands
// (spec §8) -- toy serve does nothing but hand these files out by name.
type CacheEntry struct {
	SHA256 string
	Bytes  int64
	InUse  bool // referenced by the manifest of a package that is still installed
}

// CacheEntries lists the cache with each file marked as still in use or not.
//
// "In use" means an installed package's manifest records that sha256. Everything
// else is a version somebody upgraded past, or a package they uninstalled:
// uninstall deliberately leaves the bytes behind, because they are
// content-addressed and they are what toy serve mirrors. That is a good reason
// to keep some of them and no reason at all to keep every one forever with no
// way to see the total -- which is what this exists to fix.
func (s *Store) CacheEntries() ([]CacheEntry, error) {
	ms, err := s.ListManifests()
	if err != nil {
		return nil, err
	}
	used := make(map[string]bool, len(ms))
	for _, m := range ms {
		if m.SHA256 != "" {
			used[strings.ToLower(m.SHA256)] = true
		}
	}
	des, err := os.ReadDir(s.CacheDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: cannot read the cache directory %s: %w", s.CacheDir(), err)
	}
	out := make([]CacheEntry, 0, len(des))
	for _, de := range des {
		if de.IsDir() || !isSHA256Name(de.Name()) {
			continue // Not ours; leave anything we did not put here alone.
		}
		info, err := de.Info()
		if err != nil {
			continue // Vanished between ReadDir and Info; nothing to report.
		}
		out = append(out, CacheEntry{SHA256: de.Name(), Bytes: info.Size(), InUse: used[de.Name()]})
	}
	slices.SortFunc(out, func(a, b CacheEntry) int { return strings.Compare(a.SHA256, b.SHA256) })
	return out, nil
}

// PruneCache deletes cached downloads. With all set, everything goes; otherwise
// only the entries no installed package refers to.
//
// Only files whose name is a sha256 are ever touched. The cache directory is the
// one place where a stray name would be a user's own file, and deleting
// something we did not write is not a thing this command gets to do.
func (s *Store) PruneCache(all bool) (removed int, freed int64, err error) {
	es, err := s.CacheEntries()
	if err != nil {
		return 0, 0, err
	}
	for _, e := range es {
		if e.InUse && !all {
			continue
		}
		p := filepath.Join(s.CacheDir(), e.SHA256)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, freed, fmt.Errorf("store: cannot delete %s: %w", p, err)
		}
		removed++
		freed += e.Bytes
	}
	return removed, freed, nil
}

// isSHA256Name reports whether a file name is exactly 64 lowercase hex digits.
func isSHA256Name(n string) bool {
	if len(n) != 64 {
		return false
	}
	for _, c := range n {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// WithPackageLock runs fn holding an exclusive lock for one package id.
//
// Two `toy install` runs of the same package used to race in the staging
// directory: both extract into <version>.new, one renames it into place, and the
// other is left chmod-ing files that are no longer there. The state left behind
// was fine -- one winner, a working command -- but the loser exited non-zero with
// `chmod ...: no such file or directory`, which reads like a broken filesystem
// rather than "another install is doing this right now". A Makefile installing
// packages in parallel fails for no reason a user can act on.
//
// Under the lock the second run simply waits, then takes the ordinary "already at
// this version" path.
//
// The lock file lives beside the manifest and is never deleted: unlinking it
// would let a third process create and lock a fresh file at the same path while
// the first two still hold the old inode. flock is released by the kernel when a
// process dies, so a stale file locks nothing.
//
// Locking is per package. Two different packages install concurrently with no
// contention at all, which is the common case and the one worth keeping fast.
func (s *Store) WithPackageLock(id string, fn func() error) error {
	mp, err := s.manifestPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		return fmt.Errorf("store: cannot create %s: %w", filepath.Dir(mp), err)
	}
	lockPath := mp + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("store: cannot open the install lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("store: cannot lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
