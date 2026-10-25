package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// resolved is everything you get from taking a ref all the way to "ready to start
// downloading". install, info and update all share it — do not write a second one.
type resolved struct {
	Ref      ref.Ref
	HostName string
	Host     host.Host
	Owner    string
	Repo     string
	Tag      string   // the tag as written, usually with a v
	Version  semver.V //
	// PrevTag is the release before Tag, set only when Tag was picked rather than
	// named with --version. It is what to fall back to when Tag's files are not up yet.
	PrevTag string
	// Tags is every tag git ls-remote listed, set only when Tag was picked. A forge's
	// latest-release page is only believed when it names one of them.
	Tags    []string
	Toyfile *toyfile.Toyfile
	Raw     []byte       // the verbatim bytes of toy.toml — what the author's signature covers (see autSignMessage)
	Entry   *index.Entry // set only when this came through an index, nil otherwise
	// IndexName is which index this record came from. A signature is vouched for by
	// that index and nothing else (whoever can edit that JSON can swap the public
	// key), so the trust line has to be able to name which index is vouching.
	IndexName string
	Trust     string // store.TrustSigned / TrustHost / TrustIndexed / TrustUnverified
	Signer    string // public key fingerprint; empty when unsigned
}

// rsvTagsTimeout caps `git ls-remote`. The main process context is only cancelled on
// Ctrl-C, and a host that never answers would leave it hanging forever (host.Tags
// documents that a timeout is mandatory).
const rsvTagsTimeout = 60 * time.Second

// rsvMaxTagsShown caps how many tags "there is no such version; these exist" lists.
const rsvMaxTagsShown = 20

// openStore opens ~/.toy and reads the config. It is the first line of every
// subcommand.
func openStore() (*store.Store, store.Config, error) {
	st, err := store.Open("")
	if err != nil {
		return nil, store.Config{}, err
	}
	// LoadConfig falls back to the built-in defaults when the file is missing, but
	// those defaults are invisible — the first time a user runs anything they should
	// end up with a config.toml on disk they can actually edit (adding an index, a
	// mirror or a self-hosted host all means editing it). Failing to write it is not
	// fatal: warn and carry on.
	if _, err := os.Stat(filepath.Join(st.Root, "config.toml")); errors.Is(err, os.ErrNotExist) {
		if err := st.SaveConfig(store.DefaultConfig()); err != nil {
			ui.Warn("cannot write the default config: %v (falling back to the built-in defaults for this run)", err)
		}
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, store.Config{}, err
	}
	return st, cfg, nil
}

// loadIndexes loads every configured index in order (through fetch, so file:// and
// https:// both work, and the result is cached under ~/.toy/index/). One index
// failing only warns: an index being down must not make the packages in a different
// index unfindable.
func loadIndexes(ctx context.Context, st *store.Store, cfg store.Config) ([]*index.Index, error) {
	var out []*index.Index
	failed := 0
	for _, spec := range cfg.Indexes {
		url := strings.TrimSpace(spec.URL)
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = url
		}
		if url == "" {
			ui.Warn("an index in the config has no url (name = %q), skipping it", spec.Name)
			failed++
			continue
		}

		cache := rsvIndexCache(st, name)
		data, err := fetch.GetBytes(ctx, url, fetch.Options{Name: name})
		switch {
		case err == nil:
			rsvWriteCache(cache, data, name)
		case ctx.Err() != nil:
			// Ctrl-C: stop trying the remaining indexes one by one, and do not
			// dress a cached copy up as success.
			return nil, ctx.Err()
		default:
			cached, cerr := os.ReadFile(cache)
			if cerr != nil {
				ui.Warn("index %s cannot be fetched (%v) and there is no local cache — its packages will not turn up this time", name, err)
				failed++
				continue
			}
			ui.Warn("index %s cannot be fetched (%v), using the copy cached %s ago", name, err, rsvCacheAge(cache))
			data = cached
		}

		idx, skipped, err := index.Load(data, name)
		if err != nil {
			ui.Warn("index %s is unusable: %v", name, err)
			failed++
			continue
		}
		// Skipping silently is a lie: when a user cannot find a package, they
		// need to know it is the record in the index that is broken.
		if skipped > 0 {
			ui.Warn("index %s has %d invalid record(s), skipped (id is not owner/repo, source does not parse, "+
				"source points back at an index, a plain URL that is not https://, or a duplicate id)", name, skipped)
		}
		out = append(out, idx)
	}
	// If not a single one worked, we cannot act like "that package was not found" —
	// that statement would be false.
	if len(out) == 0 && failed > 0 {
		return nil, fmt.Errorf("none of the %d configured indexes could be used — reasons above. "+
			"`toy install github:owner/repo` does not go through an index and still works", failed)
	}
	return out, nil
}

// rsvIndexCache is where one index is cached under ~/.toy/index/.
//
// The name is whatever the user wrote in config.toml and may contain slashes and
// colons, so it has to be escaped — and escaping is many-to-one ("a/b" and "a:b"
// both become "a_b"), so two indexes sharing one cache file would hand back the
// other index's contents while offline. Same approach as store.safeSeg: if anything
// was changed, append a hash of the original string.
func rsvIndexCache(st *store.Store, name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if strings.Trim(slug, ".") == "" {
		slug = "_"
	}
	if slug != name {
		sum := sha256.Sum256([]byte(name))
		slug += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return filepath.Join(st.IndexDir(), slug+".json")
}

// rsvWriteCache writes an index cache: temp file in the same directory, then rename.
// A Ctrl-C or a full disk leaving half a JSON behind would mean the next offline
// fallback reads a broken cache that never repairs itself.
func rsvWriteCache(path string, data []byte, name string) {
	f, err := os.CreateTemp(filepath.Dir(path), ".idx-*")
	if err == nil {
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), path)
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}
	if err != nil {
		// Failing to cache only affects the next offline run; this one carries on.
		ui.Warn("index %s could not be cached: %v", name, err)
	}
}

func rsvCacheAge(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "an unknown amount of time"
	}
	return time.Since(fi.ModTime()).Round(time.Minute).String()
}

// resolve carries out steps 1-3 of spec §2:
//
//	KindIndex -> look the source up in the indexes, then continue as KindHost; if it
//	             is not there, fail and suggest installing via github: directly
//	KindHost  -> git ls-remote for tags, pick the highest semver (or r.Version when
//	             it is set), then fetch that tag's toy.toml through the raw template
//	KindURL   -> fetch that toy.toml directly; there is no way to know a version
//
// It also settles the Trust level (spec §4). It downloads no artifact and touches no
// pkg directory on disk.
func resolve(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref) (*resolved, error) {
	switch r.Kind {
	case ref.KindIndex:
		return rsvViaIndex(ctx, st, cfg, r)
	case ref.KindHost:
		return rsvViaHost(ctx, cfg, r, "")
	case ref.KindURL:
		return rsvViaURL(ctx, r)
	default:
		return nil, fmt.Errorf("unrecognized package reference: %q", r.Raw)
	}
}

// rsvViaIndex looks the source up in the indexes and then follows whichever route
// that source names. An index is only a directory; the source is the truth (spec §5).
func rsvViaIndex(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref) (*resolved, error) {
	idxs, err := loadIndexes(ctx, st, cfg)
	if err != nil {
		return nil, err
	}
	id := r.ID()
	// With no index configured at all — a legal state, see store.DefaultIndexURL —
	// saying "not found in the indexes" would be false: we did not look anywhere
	// (iron law 8).
	if len(cfg.Indexes) == 0 {
		return nil, fmt.Errorf("no index is configured, so %s cannot be looked up. "+
			"Add the public one with `toy index add default %s` (a config.toml written by toy 0.1.0 has none). "+
			"And if you know where the repository is, `toy install github:%s` never needed an index", id, store.DefaultIndexURL, id)
	}
	for _, idx := range idxs {
		e, ok := idx.Find(id)
		if !ok {
			continue
		}
		src, err := ref.Parse(e.Source)
		if err != nil {
			return nil, fmt.Errorf("the source %q of %s in index %s does not parse: %w", e.Source, e.ID, idx.Name, err)
		}
		src.Version = r.Version // the @version the user typed wins

		var res *resolved
		switch src.Kind {
		case ref.KindHost:
			// An inlined toyfile is only ever passed down the host route: there
			// the download URL is built from our own host template plus the tag,
			// so all the index can change is "which asset gets installed and what
			// the command is called" (rule 5 — this is what makes the whole design
			// hold). On the plain-URL route the index already decides where the
			// toy.toml lives and assets are resolved relative to it, so letting the
			// index swap the file's contents too would leave the blast radius with
			// no edge at all. That route does not take inline entries.
			res, err = rsvViaHost(ctx, cfg, src, e.Toyfile)
		case ref.KindURL:
			// A plain URL offers no versions to pick from, so the version the user
			// named cannot be installed. Quietly installing the latest would hand
			// them something they did not ask for; say so instead.
			if r.Version != "" {
				return nil, fmt.Errorf("%s in index %s points at a fixed URL (%s), which offers no versions to choose from, "+
					"so @%s cannot be installed. Find its repository and use github:owner/repo@%s instead",
					e.ID, idx.Name, e.Source, r.Version, r.Version)
			}
			res, err = rsvViaURL(ctx, src)
		default:
			// index.Load already skips records like this as invalid; reaching
			// here means it changed underneath us.
			return nil, fmt.Errorf("the source of %s in index %s is %q, which is another index lookup — that is a loop",
				e.ID, idx.Name, e.Source)
		}
		if err != nil {
			return nil, err
		}

		res.Entry, res.IndexName = &e, idx.Name
		rsvApplySignature(idx.Name, e, res)
		return res, nil
	}

	// Not in the index does not mean uninstallable — the github: route is the truth
	// from end to end, which is the entire point of this architecture (spec §5:
	// delisting is harmless).
	return nil, fmt.Errorf("no index has %s. That does not mean it cannot be installed: an index is only a "+
		"directory and the host is the source of truth — if you know where it lives, write "+
		"`toy install github:%s` (use a gitlab: / codeberg: prefix for other hosts). "+
		"`toy search %s` may turn up something with a similar name", id, id, r.Repo)
}

// rsvViaHost walks the main path of spec §2: ls-remote for tags -> pick a version ->
// fetch toy.toml over raw.
//
// inline is the verbatim toyfile from the index entry (empty when there is none). It
// is only ever used when upstream definitively has no toy.toml, and using it drops
// trust to store.TrustIndexed.
func rsvViaHost(ctx context.Context, cfg store.Config, r ref.Ref, inline string) (*resolved, error) {
	h, err := host.Lookup(r.Host, cfg.Hosts)
	if err != nil {
		return nil, err
	}
	hostName := h.Name
	if hostName == "" {
		hostName = strings.ToLower(r.Host)
	}

	// Version discovery is `git ls-remote --tags`: one command, identical across
	// every git host, no API, no token, no rate limit (spec §2).
	clone := h.CloneURL(r.Owner, r.Repo)
	tagCtx, cancel := context.WithTimeout(ctx, rsvTagsTimeout)
	defer cancel()
	tags, err := host.Tags(tagCtx, clone)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("%s has no tags at all, and tags are how toy discovers versions. "+
			"If this is your repository: push a v1.0.0 tag, cut a release, add a toy.toml, and it is installable", clone)
	}
	v, err := rsvPickVersion(tags, r.Version, r.Repo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.String(), err)
	}
	// When the version was picked rather than asked for, remember the one before it:
	// a tag routinely goes up before its release files do (insPerformOrPrevious).
	prevTag := ""
	var picked []string
	if r.Version == "" {
		picked = tags
		if p, err := semver.Max(rsvWithout(tags, v.Raw), false, r.Repo); err == nil {
			prevTag = p.Raw
		}
	}

	rawURL := h.RawURL(r.Owner, r.Repo, v.Raw, "toy.toml")
	if rawURL == "" {
		return nil, fmt.Errorf("host %q has no raw template, so toy.toml cannot be fetched. "+
			"Add raw = \"…/{owner}/{repo}/…/{ref}/{path}\" under [hosts.%s] in ~/.toy/config.toml",
			hostName, hostName)
	}
	where := fmt.Sprintf("%s:%s/%s@%s", hostName, r.Owner, r.Repo, v.Raw)
	// The trust anchor is the host's account system and nothing else (spec §4).
	trust := store.TrustHost
	tf, raw, err := rsvGetToyfile(ctx, rawURL, where)
	if err != nil {
		// Upstream always wins (rule 1): the toyfile inlined in an index entry
		// only steps in when upstream DEFINITIVELY has none of its own. The day
		// the author adds one, this gives way unconditionally — otherwise a
		// malicious index entry could override the author's own declaration, and
		// that is hijacking.
		if inline == "" || !rsvIsNotFound(err) {
			return nil, err
		}
		if tf, raw, err = rsvInlineToyfile(inline); err != nil {
			return nil, fmt.Errorf("%s has no toy.toml in its repository, and the toyfile inlined by the index is unusable too: %w", where, err)
		}
		trust = store.TrustIndexed
		ui.Warn("%s has no toy.toml in its repository, so the one inlined in the index entry was used: "+
			"which asset gets installed and what the command is called come from the index, not from the author. "+
			"The bytes still come from the author's own release — the download URL is built from the host template, "+
			"and the index has no say in it", where)
	}

	return &resolved{
		Ref:      r,
		HostName: hostName,
		Host:     h,
		Owner:    r.Owner,
		Repo:     r.Repo,
		Tag:      v.Raw,
		Version:  v,
		PrevTag:  prevTag,
		Tags:     picked,
		Toyfile:  tf,
		Raw:      raw,
		Trust:    trust,
	}, nil
}

// rsvWithout is tags minus one of them.
func rsvWithout(tags []string, drop string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != drop {
			out = append(out, t)
		}
	}
	return out
}

// rsvViaURL is the plain-URL route: nothing is vouched for, and there is no such
// thing as a version.
func rsvViaURL(ctx context.Context, r ref.Ref) (*resolved, error) {
	tf, raw, err := rsvGetToyfile(ctx, r.URL, r.URL)
	if err != nil {
		return nil, err
	}
	// There is no tag on this route, so {version} / {tag} cannot be expanded.
	// Forcing them to the empty string would build a download URL that looks
	// entirely plausible while pointing at a different file (or at a 404 page), and
	// with no hash declared those bytes get pinned by TOFU exactly as they are.
	// Better to say it now.
	for _, m := range []map[string]string{tf.Artifact, tf.Script} {
		for k, val := range m {
			if strings.Contains(val, "{version}") || strings.Contains(val, "{tag}") {
				return nil, fmt.Errorf("this package cannot be installed from a plain URL: the asset for %s is %q, "+
					"which uses the {version}/{tag} placeholders, and the URL of a toy.toml carries no version. "+
					"Use `toy install github:owner/repo` instead (that route takes the version from a git tag), "+
					"or ask the author to write the asset name out in full", k, val)
			}
		}
	}
	return &resolved{
		Ref:     r,
		Toyfile: tf,
		Raw:     raw,
		// Empty Tag, zero Version: there is no way to know, and inventing one
		// would be a lie (iron law 8).
		Trust: store.TrustUnverified,
	}, nil
}

// rsvGetToyfile fetches and parses one toy.toml. where is a human-readable
// description of the location.
func rsvGetToyfile(ctx context.Context, url, where string) (*toyfile.Toyfile, []byte, error) {
	data, err := fetch.GetBytes(ctx, url, fetch.Options{Name: "toy.toml"})
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// Almost always a 404: the repository has not adopted toy yet. Never guess
		// asset names, never build (iron law 2).
		return nil, nil, fmt.Errorf("no toy.toml at %s: %w. "+
			"This repository most likely has not adopted toy yet — toy installs release binaries and "+
			"single-file scripts, it never guesses asset names, and it never builds from source. "+
			"If this is your repository: add an 8-line toy.toml at the root (name / [bin] / [artifact]) "+
			"and you are done — nobody has to approve it",
			where, err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("there is a problem with the toy.toml at %s: %w", where, err)
	}
	// Keep the verbatim bytes: what the author signed is "the file the user actually
	// downloads", and re-serializing it means verifying against a different piece of
	// text (see the comment on autSignMessage).
	return tf, data, nil
}

// rsvInlineToyfile parses the toyfile inlined in an index entry.
//
// Validation goes down exactly the same path as an upstream one (toyfile.ParseFrom
// only adds an origin marker to Parse), with nothing relaxed: bin targets may not be
// absolute, may not contain "..", artifact values may not be URLs, platform keys have
// to be valid (rule 4). "It came from our own index" is no reason to relax anything —
// an index is precisely the place anyone can fork and anyone can open a PR against.
func rsvInlineToyfile(inline string) (*toyfile.Toyfile, []byte, error) {
	raw := []byte(inline)
	tf, err := toyfile.ParseFrom(raw, toyfile.OriginIndex)
	if err != nil {
		return nil, nil, err
	}
	return tf, raw, nil
}

// rsvIsNotFound decides "upstream's toy.toml definitively does not exist", as
// opposed to "we did not get it this time".
//
// That distinction IS the security boundary of the inline toyfile (rule 1): a 404
// means the author really has not adopted toy, while a timeout, a 5xx or a DNS
// failure only means we did not get an answer this time — treating the latter as
// "does not exist" lets one network hiccup quietly move the user from the author's
// declaration to the index's, with nothing anywhere saying so.
//
// Both routes collapse into one test: on file:// os.Open already gives fs.ErrNotExist,
// and on https fetch wraps the same sentinel when the status code really is 404.
//
// NEVER change this back to matching on error text. The reason phrase in resp.Status
// is sent verbatim by the server, so one upstream answering `HTTP/1.1 500 HTTP 404
// Not Found` is enough to make strings.Contains read a 500 as "does not exist" —
// and then a server-side outage moves the user from the author's declaration to the
// index's, which is exactly what rule 3 is there to stop, with nothing anywhere
// saying so.
func rsvIsNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// rsvPickVersion picks the version: the tag the user named if they named one,
// otherwise the highest stable release.
// repo is the repository name; semver.Max needs it to tell a project's own
// prefixed tags (gping-v1.21.0) from a sub-project sharing the repository
// (wincolor-0.1.6).
func rsvPickVersion(tags []string, want, repo string) (semver.V, error) {
	if want == "" {
		return semver.Max(tags, false, repo)
	}
	num := strings.TrimPrefix(want, "v")
	for _, t := range tags {
		// "v1.2.3" and "1.2.3" name the same tag; accept either spelling.
		if t == want || strings.TrimPrefix(t, "v") == num {
			return rsvPinned(t, repo), nil
		}
	}
	// "1.8.1" for jq, whose tags are jq-1.8.1: the number without the repository's
	// own prefix names the same release, the way "1.2.3" names "v1.2.3". Only a
	// prefix that IS the repository name -- in ripgrep, wincolor-0.1.6 is another
	// project, and --version 0.1.6 must not find it.
	for _, t := range tags {
		if v, err := semver.Max([]string{t}, true, repo); err == nil && v.Prefix != "" &&
			semver.PrefixNamesRepo(v.Prefix, repo) && v.Number() == num {
			return v, nil
		}
	}
	return semver.V{}, fmt.Errorf("there is no version %q. The ones that exist are: %s", want, rsvShowTags(tags))
}

// rsvShowTags turns a tag list into something a person can read: the parseable ones
// newest first, everything else (release-2024 and friends) after them.
func rsvShowTags(tags []string) string {
	sorted := slices.Clone(tags)
	slices.SortStableFunc(sorted, func(a, b string) int {
		va, ea := semver.Parse(a)
		vb, eb := semver.Parse(b)
		switch {
		case ea == nil && eb == nil:
			return semver.Compare(vb, va) // newest first
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(sorted) > rsvMaxTagsShown {
		return strings.Join(sorted[:rsvMaxTagsShown], ", ") +
			fmt.Sprintf("… (%d in total)", len(sorted))
	}
	return strings.Join(sorted, ", ")
}

// rsvApplySignature decides whether this index record counts as "signed" at all
// (spec §4, iron law 8).
//
// A key on its own means nothing: an index is a JSON file anyone can fork and anyone
// can open a PR against, and copying somebody's public key into an entry costs
// nothing. WHAT GETS VERIFIED IS THE SIGNATURE: the author signed id + source + the
// verbatim bytes of this toy.toml (autSignMessage), so the signature pins who,
// where it points, and what gets installed all at once — swap any one of them and it
// no longer verifies.
//
// If it does not verify, drop to the level below (host account / unverified) and say
// why. Never print the check mark: a client that shows one on sight of a key is
// handing the word "signed" free of charge to anyone who can edit an index, and the
// client is the only security boundary there is.
func rsvApplySignature(idxName string, e index.Entry, res *resolved) {
	key := strings.TrimSpace(e.Key)
	if key == "" {
		return // no name claimed, so there should be no signature (signing is an optional upgrade, not an entry ticket)
	}
	fp := rsvFingerprint(key)
	switch {
	case fp == "":
		ui.Warn("the key on %s in index %s is not a valid ed25519 public key, treating it as unsigned", e.ID, idxName)
	case strings.TrimSpace(e.Sig) == "":
		ui.Warn("%s in index %s carries an author public key but no signature — a key that has signed nothing "+
			"proves nothing, so this counts as unsigned (one run of `toy publish` by the author adds the sig)", e.ID, idxName)
	default:
		if err := sign.Verify(key, autSignMessage(e.ID, e.Source, res.Raw), strings.TrimSpace(e.Sig)); err != nil {
			ui.Warn("the signature on %s in index %s does not verify against this version of toy.toml (%v), "+
				"treating it as unsigned — either the author edited toy.toml without re-running `toy publish`, "+
				"or somebody altered this index record", e.ID, idxName, err)
			return
		}
		// An inlined toyfile never reaches TrustSigned (rule 2): the signature
		// covers what THE PERSON WHO SUBMITTED THIS INDEX RECORD wrote, and the
		// signer is not the author. A check mark here is exactly the overclaim
		// §14.3 warns about — it proves only that this record was not altered,
		// not that the author endorses these packaging instructions. The
		// fingerprint is still kept: a swapped key has to be caught by TOFU
		// (insKeyID / spec §4).
		if res.Toyfile != nil && res.Toyfile.Origin == toyfile.OriginIndex {
			res.Signer = fp
			ui.Info("the signature on %s in index %s verifies (key %s) — that only shows this index record "+
				"was not altered, not that the author endorses these packaging instructions, so the trust "+
				"level stays at \"packaging instructions from the index\"", e.ID, idxName, fp)
			return
		}
		res.Trust, res.Signer = store.TrustSigned, fp
	}
}

// rsvFingerprint turns the hex public key from an index entry into the short
// fingerprint shown on screen. It returns the empty string when the key is not a
// valid ed25519 public key, and callers take that as "do not claim this is signed".

func rsvFingerprint(hexKey string) string {
	b, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return ""
	}
	return sign.Fingerprint(ed25519.PublicKey(b))
}

// rsvPinned parses a tag the user named with --version. It goes through the same
// parser as picking the newest, so a prefix is stripped the same way: returning a
// bare {Raw: "jq-1.8.1"} made {version} expand to "jq-1.8.1" and named the package
// directory after the whole tag, so a pinned install and an unpinned one of the
// same release disagreed on both. A tag carrying no version at all ("nightly") is
// still installable -- the user named it -- it just cannot be compared.
func rsvPinned(tag, repo string) semver.V {
	if v, err := semver.Max([]string{tag}, true, repo); err == nil {
		return v
	}
	return semver.V{Raw: tag}
}
