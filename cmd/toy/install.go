package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/platform"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// insNoVersion stands in for "this package has no such thing as a version" (the
// plain-URL route). It becomes a segment of the pkg directory, so it has to be a
// word that works as a directory name — never the empty string.
const insNoVersion = "unversioned"

// runInstall is the main flow, spec §2.
//
// Two flags, two different things. Do NOT merge them back together:
//
//	--reinstall  this version is already installed, install it again. Everyday
//	             operation, waives no check at all.
//	--force      accept what a safety check reports (checksum / author key /
//	             shadowing / trust downgrade / source). When it really waives
//	             something, print each waiver.
//
// parse ref -> (look the source up in an index) -> git ls-remote for tags -> pick
// the highest semver -> fetch toy.toml over the raw URL -> pick the asset for this
// os/arch -> download (with mirror fallback) -> check sha256 -> TOFU check ->
// unpack or chmod +x -> symlink (refusing to shadow) -> write the manifest ->
// print the trust level.
func runInstall(ctx context.Context, args []string) error {
	fs := newFlagSet("install")
	// These two flags say two different things; do not merge them back into one
	// (see the comment above insPerform). --reinstall is an everyday operation,
	// --force means "I checked it myself, let the safety check through".
	reinstall := fs.Bool("reinstall", false, "install this same version again (waives no safety check)")
	force := fs.Bool("force", false, "accept what a safety check reports: checksum changed / author key changed / command name shadowing / trust downgrade / source changed")
	version := fs.String("version", "", "install this version instead of the highest tag")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}

	r, err := ref.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(*version); v != "" {
		// The @version in the ref and --version mean the same thing. If both are
		// given and they disagree, installing either one is a guess — make the
		// user decide instead.
		if r.Version != "" && r.Version != v {
			return fmt.Errorf("%s says @%s but --version says %s. Those disagree, "+
				"and guessing which one you meant is not something toy will do — drop one and try again", r.Raw, r.Version, v)
		}
		if r.Kind == ref.KindURL {
			return fmt.Errorf("--version means nothing for a plain URL: %s points straight at one toy.toml, "+
				"so there are no tags to choose from. Use `toy install github:owner/repo@%s` instead", r.URL, v)
		}
		// The version gets pasted into the download URL and into the pkg directory
		// name, so run it through the same rules a ref goes through — no shortcut.
		// Borrowing ref.Parse here rather than copying its allowlist a second time.
		probe, perr := ref.Parse("owner/repo@" + v)
		if perr != nil {
			return fmt.Errorf("--version %q will not do: %w", v, perr)
		}
		r.Version = probe.Version
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	res, err := resolve(ctx, st, cfg, r)
	if err != nil {
		return err
	}

	old, err := insExisting(st, res)
	if err != nil {
		return err
	}
	// Already at this version: do not download it again. --reinstall means "I know,
	// do it anyway". This shortcut is off the table when old.ID differs from what we
	// just resolved — that means the source changed, and a matching tag is no proof
	// it is the same thing.
	if old != nil && old.ID == insPackageID(res) && !*reinstall && res.Tag != "" && old.Tag == res.Tag {
		hint := "add --reinstall to install it again"
		if *force {
			// --force no longer triggers a reinstall as a side effect. Returning
			// silently on a bare --force would leave the user believing whatever
			// they meant to waive had already taken effect.
			hint = "--force only waives what a safety check reports; it does not itself trigger a reinstall — add --reinstall for that"
		}
		ui.Info("%s is already at %s, nothing to do (%s)", old.Name, old.Tag, hint)
		return nil
	}
	// Going backwards is legal: `toy install foo@v1.0.0` asking for v1.0.0 is the
	// user saying so, and toy does not block it. But it must not happen quietly —
	// upstream deleting the newest tag (account taken over, release pulled, a
	// self-hosted host swapped out) takes exactly this path, and the only output
	// would be "installed t 1.0.0". Iron law 8: say it plainly and let the user
	// decide whether this downgrade is the one they wanted.
	if old != nil && insIsOlder(*old, res) {
		ui.Warn("this is a downgrade: %s currently has %s, and %s is older. "+
			"If you did not mean to go back a version, check whether upstream pulled the newer tag", old.Name, old.Tag, res.Tag)
	}
	if err := insPerformOrPrevious(ctx, st, cfg, r, res, old, *force); !errors.Is(err, insErrUpToDate) {
		return err
	}
	return nil
}

// runUninstall reads the manifest and removes the symlinks and the pkg directory.
// It does NOT touch the cache: that is content-addressed, and it doubles as the
// directory `toy serve` mirrors.
func runUninstall(ctx context.Context, args []string) error {
	fs := newFlagSet("uninstall")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	// When several packages match, FindByName has already listed the candidates.
	// Pass that through as-is rather than wrapping it in another layer.
	m, err := st.FindByName(fs.Arg(0))
	if err != nil {
		return err
	}

	// Symlinks first: if this fails while the manifest is still there, the user just
	// runs it again. Delete the manifest first and nothing knows which symlinks
	// were ever supposed to go.
	for _, l := range m.Links {
		if err := st.Unlink(l); err != nil {
			return err
		}
	}
	if err := insRemovePkgDir(st, m.PkgDir); err != nil {
		return err
	}
	if err := st.RemoveManifest(m.ID); err != nil {
		return err
	}
	ui.Info("removed %s %s (the cache stays — it is content-addressed, and it is what `toy serve` mirrors)",
		m.Name, m.Version)
	return nil
}

// runUpdate re-resolves the newest tag for each installed package, compares semver,
// and only runs the install flow when there is something newer. With no argument it
// does all of them, reporting each one; a failure on one does not stop the rest.
func runUpdate(ctx context.Context, args []string) error {
	fs := newFlagSet("update")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errUsage
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	var todo []store.Manifest
	if fs.NArg() == 1 {
		m, err := st.FindByName(fs.Arg(0))
		if err != nil {
			return err
		}
		todo = []store.Manifest{m}
	} else if todo, err = st.ListManifests(); err != nil {
		return err
	}
	if len(todo) == 0 {
		ui.Info("nothing is installed, so there is nothing to upgrade")
		return nil
	}

	updated, current, failed := 0, 0, 0
	for _, m := range todo {
		switch err := insUpdateOne(ctx, st, cfg, m); {
		case err == nil:
			updated++
		case errors.Is(err, insErrUpToDate):
			current++
		case errors.Is(err, context.Canceled):
			return err // Ctrl-C means Ctrl-C; do not go fetch the next package
		default:
			// One package failing must not hold up the others — they are
			// independent of each other by design (iron law 3).
			ui.Error("%s: %v", m.ID, err)
			failed++
		}
	}
	ui.Info("%d upgraded, %d already current, %d failed", updated, current, failed)
	if failed > 0 {
		return fmt.Errorf("%d package(s) could not be upgraded — reasons are above", failed)
	}
	return nil
}

// insErrUpToDate is not a failure; it is insUpdateOne's third outcome.
var insErrUpToDate = errors.New("already up to date")

func insUpdateOne(ctx context.Context, st *store.Store, cfg store.Config, m store.Manifest) error {
	// Upgrade the way it was installed: something installed through an index has to
	// go through the index again. Otherwise the author's signature — which lives
	// only in the index entry — is lost on the very first upgrade, and the manifest
	// faithfully records the trust level one notch lower.
	id := m.ID
	if m.IndexID != "" {
		id = m.IndexID
	}
	r, err := ref.Parse(id)
	if err != nil {
		return fmt.Errorf("the id %q in the manifest does not parse, so its version cannot be looked up again: %w", id, err)
	}
	if r.Kind == ref.KindURL {
		// A plain URL points at one fixed toy.toml; there are no tags to compare.
		// The only way to move is to install that URL again. Say so plainly rather
		// than pretending a check happened.
		ui.Info("skipping %s: it came from a fixed URL, so there is no version to compare. "+
			"To pick up new content, run `toy install %s` again", m.Name, m.ID)
		return insErrUpToDate
	}

	res, err := resolve(ctx, st, cfg, r)
	if err != nil && m.IndexID != "" && m.IndexID != m.ID {
		// Gone from the index does not mean uninstallable — the source is the truth
		// and delisting is harmless (spec §5). But say it out loud: the author's
		// public key and signature go with the entry, so the trust level drops.
		ui.Warn("%s is no longer in any index (%v), upgrading straight from %s instead; "+
			"with the index entry gone there is no author signature left to verify", m.Name, err, m.ID)
		if r, err = ref.Parse(m.ID); err != nil {
			return fmt.Errorf("the id %q in the manifest does not parse, so its version cannot be looked up again: %w", m.ID, err)
		}
		res, err = resolve(ctx, st, cfg, r)
	}
	if err != nil {
		return err
	}
	if !insIsNewer(m, res) {
		ui.Info("%s %s is already current", m.Name, m.Tag)
		return insErrUpToDate
	}
	ui.Step("%s %s → %s", m.Name, m.Tag, res.Tag)
	old := m
	return insPerformOrPrevious(ctx, st, cfg, r, res, &old, false)
}

// insIsNewer reports whether the remote tag is newer than the installed one.
func insIsNewer(m store.Manifest, res *resolved) bool {
	newV, nerr := semver.Parse(res.Tag)
	oldV, oerr := semver.Parse(m.Tag)
	if nerr != nil || oerr != nil {
		// If either side is not semver there is no ordering to speak of. All we can
		// go on is "the tag changed, so there is something new" — treating an
		// unparseable tag as 0.0.0 would walk a perfectly healthy package backwards
		// on every update.
		return res.Tag != "" && res.Tag != m.Tag
	}
	return semver.Compare(newV, oldV) > 0
}

// insIsOlder reports whether the tag about to be installed is older than the one
// already installed.
//
// This is NOT the mirror image of insIsNewer. If either side is not valid semver
// there is no ordering, and saying "this is a downgrade" would be a guess — a wrong
// guess here is worse than staying quiet, so this returns false. insIsNewer, in the
// same situation, goes with "the tag changed, so there is something new".
func insIsOlder(m store.Manifest, res *resolved) bool {
	newV, nerr := semver.Parse(res.Tag)
	oldV, oerr := semver.Parse(m.Tag)
	if nerr != nil || oerr != nil {
		return false
	}
	return semver.Compare(newV, oldV) < 0
}

// insItem is one file this install has to download. The [artifact] shape has a
// single item (possibly an archive); the [script] shape has one per entry.
type insItem struct {
	name   string // command name (script) or asset name (artifact), for display
	asset  string // asset name / in-repo path with placeholders expanded
	url    string
	want   string // sha256 declared in toy.toml; empty means TOFU
	path   string // where the download landed
	sum    string // the sha256 actually computed
	source string // the URL that actually answered (may be a mirror)
}

// insPerform is the stretch install and update share: download -> TOFU -> write to
// disk -> symlink -> write the manifest -> pin -> print the trust level. A non-nil
// old means this is replacing a version that is already installed.
//
// force means exactly one thing: "a safety check reported a change, I checked it
// myself, let it through". "Install the same version again" is --reinstall and never
// reaches here — that is an everyday operation, and nobody should have to pick up a
// hammer that also waives "this version's contents changed" and "the author swapped
// keys", which are the entire reason TOFU exists (spec §4, iron laws 4 and 6).
//
// Whatever force actually waives MUST be printed, one line each (iron law 8): a
// silent waiver is the same as having no check.
// insPerform holds a per-package lock for the whole download-and-place sequence.
// Two concurrent installs of the same package used to collide in the staging
// directory: both extract into <version>.new, one renames it into place, and the
// other chmods files that are no longer there. The result on disk was fine, but
// the loser exited non-zero saying `chmod ...: no such file or directory`, which
// reads like a broken filesystem rather than "somebody else is installing this".
// Under the lock the second run waits and then takes the ordinary "already at
// this version" path. Different packages never contend.
func insPerform(ctx context.Context, st *store.Store, cfg store.Config, res *resolved, old *store.Manifest, force bool) error {
	return st.WithPackageLock(insPackageID(res), func() error {
		// Read the state again now that the lock is held. The caller's check ran
		// outside it, so four concurrent runs all saw "not installed", queued up,
		// and the three losers went on to install a second time -- landing on
		// "a link with that name already exists", which is true and unhelpful.
		// A lock only means something when the decision and the action are inside it.
		fresh, ferr := insExisting(st, res)
		appeared := ferr == nil && fresh != nil && fresh.ID == insPackageID(res) &&
			// The caller looked and saw nothing, or saw a different version. Either
			// way, what is on disk now arrived while this run was waiting.
			(old == nil || old.Tag != fresh.Tag) &&
			// And it is the same thing we were about to install. A bare URL carries
			// no tag at all, which is exactly the case the first version of this
			// check missed.
			(res.Tag == "" || fresh.Tag == res.Tag)
		if appeared {
			at := fresh.Tag
			if at == "" {
				at = fresh.Version
			}
			ui.Info("%s was installed at %s by another run while this one waited, nothing to do", fresh.Name, at)
			return nil
		}
		return insPerformLocked(ctx, st, cfg, res, old, force)
	})
}

func insPerformLocked(ctx context.Context, st *store.Store, cfg store.Config, res *resolved, old *store.Manifest, force bool) error {
	if insTrustDowngraded(old, res) {
		if !force {
			return fmt.Errorf("this install of %s trusts less than the last one did: it was %q, now it is %q. "+
				"Maybe the sig was dropped from the index entry, maybe upstream's toy.toml is gone and the index's "+
				"inlined one took over, maybe you installed it by a different route this time. The warning above "+
				"says which. Check it yourself, then add --force to go ahead", old.Name, insTrustWord(old.Trust), insTrustWord(res.Trust))
		}
		ui.Warn("--force waived: %s dropped a trust level (%s -> %s)",
			old.Name, insTrustWord(old.Trust), insTrustWord(res.Trust))
	}
	// The other direction is good news: the author has since added their own
	// toy.toml, so the packaging instructions no longer come from the index
	// (rule 6). Do not block it, but do mention it — last time "which asset,
	// what the command is called" came from the index and this time it comes from
	// the author, and that is a real change to this package.
	if old != nil && old.Trust == store.TrustIndexed && res.Trust != store.TrustIndexed {
		ui.Info("upstream now ships its own toy.toml for %s: the packaging instructions no longer come from the index, this version installs the way the author wrote it", old.Name)
	}
	if insSourceChanged(old, res) {
		if !force {
			return fmt.Errorf("%s was installed from %s last time; this time it resolves to %s. "+
				"An index entry is only a pointer — editing one source line is enough to swap an installed "+
				"command for the contents of a different repository. Make sure this source is the one you want, "+
				"then add --force to go ahead", old.Name, old.ID, insPackageID(res))
		}
		ui.Warn("--force waived: the source of %s changed from %s to %s", old.Name, old.ID, insPackageID(res))
	}
	tf := res.Toyfile
	items, err := insItems(res)
	if err != nil {
		return err
	}
	ring, err := sign.OpenRing(st.KnownDir())
	if err != nil {
		return err
	}

	// Finish every download and every TOFU check before touching the package
	// directory on disk: if this run refuses partway through, the version already
	// installed has to be left exactly as it was.
	type pin struct{ id, kind, value string }
	var pins []pin
	trustID := insTrustID(res)
	for i := range items {
		it := &items[i]
		dl, err := fetch.Get(ctx, it.url, fetch.Options{
			SHA256:   it.want,
			Mirrors:  cfg.Mirrors,
			CacheDir: st.CacheDir(),
			Sink:     ui.NewProgress(),
			Name:     it.name,
		})
		if err != nil {
			if errors.Is(err, fetch.ErrChecksum) {
				// Iron law 4: a mismatch is always refused. fetch has already put
				// the expected and actual values into the error.
				return fmt.Errorf("checksum mismatch on %s: it does not match the sha256 declared in toy.toml, so nothing was installed: %w", it.asset, err)
			}
			return err
		}
		it.path, it.sum, it.source = dl.Path, dl.SHA256, dl.Source
		// When the original URL is down and the bytes came from a mirror, say so
		// (iron law 8). The content is address-by-hash, so a mirror does not need to
		// be trusted (spec §8) — but "does not need to be trusted" is not "does not
		// need to be known", and the user is entitled to know where these bytes
		// came from. fetch only tries mirrors when there is a sha256 to compare
		// against, so by the time we get here the comparison has happened.
		if dl.Source != it.url && !dl.FromCache {
			// The last segment of a mirror URL is the sha256 itself; no point
			// repeating it.
			ui.Info("%s: the original URL did not answer, so these bytes came from the mirror %s (sha256 checked, it matches what toy.toml declares)",
				it.name, dl.Source)
		}

		// The asset name goes into the TOFU id unconditionally. It IS the platform
		// discriminator — asset names already differ per platform
		// (linux_amd64.tar.gz vs linux_amd64_musl.tar.gz), and the same ref@version
		// hands a musl machine and a glibc machine two completely different sets of
		// bytes. Leave it out and moving to another machine (or sharing $HOME) gets
		// reported as "somebody swapped the contents" — a frightening false alarm,
		// in exactly the place where crying wolf costs the most. When two platforms
		// do point at the same asset name they should share one pin, and this gets
		// that right too.
		id := trustID + "/" + it.name
		if err := insTOFU(ring, id, "sha256", it.sum, "the sha256 of "+it.name, force); err != nil {
			return err
		}
		pins = append(pins, pin{id, "sha256", it.sum})
	}
	// If the index says this package has an author public key, pin the key as well
	// (spec §4: both the public key and the artifact hash are pinned on first
	// install). A different key is a different "author", and that is precisely what
	// this is here to stop.
	if res.Entry != nil && res.Signer != "" {
		key := strings.TrimSpace(res.Entry.Key)
		if err := insTOFU(ring, insKeyID(res), "key", key, "the author public key", force); err != nil {
			return err
		}
		pins = append(pins, pin{insKeyID(res), "key", key})
	}

	// Only touch the version currently in service once unpacking AND symlinking have
	// both succeeded: unpack into a sibling directory, then move the old one aside
	// and swing the new one in; if symlinking fails, move the old one back. Doing a
	// RemoveAll in place and unpacking over it turns a poisoned or corrupt upstream
	// plus one perfectly ordinary --force reinstall into a primitive for "take away
	// the command the user has" — empty package directory, dangling symlink, while
	// the manifest is still there and `toy list` still lists it.
	pkgDir := st.PkgDir(insHostSeg(res), insOwnerSeg(res), insNameSeg(res, tf), insDirVersion(res))
	staged := pkgDir + ".new"
	if err := insRemovePkgDir(st, staged); err != nil {
		return err
	}
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return fmt.Errorf("cannot create the package directory %s: %w", staged, err)
	}
	for _, it := range items {
		if err := insUnpack(tf, it, staged); err != nil {
			os.RemoveAll(staged) // half a package is harder to diagnose than none
			return err
		}
	}
	backup, err := insSwapIn(st, staged, pkgDir)
	if err != nil {
		return err
	}

	links, err := insLink(st, tf, pkgDir, old, force)
	if err != nil {
		insRestore(st, backup, pkgDir)
		return err
	}
	_ = insRemovePkgDir(st, backup)

	m := store.Manifest{
		ID: insPackageID(res), IndexID: insIndexID(res), Name: tf.Name,
		Version: insVersionLabel(res), Tag: res.Tag,
		Host: res.HostName, Owner: res.Owner, Repo: res.Repo,
		Trust: res.Trust, Signer: res.Signer,
		SHA256: items[0].sum, Source: items[0].source,
		PkgDir: pkgDir, Links: links,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.SaveManifest(m); err != nil {
		return err
	}
	// When the source changed, the new manifest has a different id, and leaving the
	// old one behind creates a ghost record pointing at a deleted directory: the same
	// package shows up twice in `toy list`, `toy uninstall <name>` reports "matched 2
	// installed packages", and doctor's suggested fix reinstalls the repository that
	// was moved away from.
	if old != nil && old.ID != m.ID {
		if err := st.RemoveManifest(old.ID); err != nil && !errors.Is(err, store.ErrNotInstalled) {
			ui.Warn("cannot delete the old manifest %s: %v (`toy list` will show this package twice)", old.ID, err)
		}
	}
	// Pin only after the install succeeded. Pinning on failure would let the next
	// install treat a bad value as "what was pinned on first install".
	for _, p := range pins {
		if err := ring.Pin(p.id, p.kind, p.value); err != nil {
			ui.Warn("cannot pin %s for %s: %v (the next install will treat it as a first install)", p.kind, p.id, err)
		}
	}
	insCleanOld(st, old, pkgDir, links)

	ui.Info("installed %s %s -> %s", tf.Name, m.Version, strings.Join(insBaseNames(links), ", "))
	if !st.InPath() {
		ui.Warn("%s is not on your PATH yet, so the command you just installed will not be found. Add it to PATH — `toy doctor` prints the exact line", st.BinDir())
	}
	ui.Trust(res.Trust, insTrustDetail(res))
	return nil
}

// insTrustDowngraded reports whether this install trusts one notch less than the
// last one did.
//
// Verifying the signature is not enough on its own: dropping the sig from an index
// entry quietly turns "✓ signed" into "· from …", and nobody notices a check mark
// that stopped being there. TOFU records "what it was last time", and so does this —
// a downgrade stops and makes the user type --force (spec §4, iron law 8).
//
// It compares store.TrustRank rather than testing "is it TrustSigned": the latter
// breaks nowhere at all when a fourth level is added, it just silently files the new
// level on the wrong side. Host level dropping to "packaging instructions from the
// index" — upstream's toy.toml is gone and the index's inlined one took over — is
// exactly such a downgrade, and like a vanished signature it means somebody else
// made a decision on the author's behalf.
func insTrustDowngraded(old *store.Manifest, res *resolved) bool {
	return old != nil && store.TrustRank(res.Trust) < store.TrustRank(old.Trust)
}

// insTrustWord is how a trust level reads inside a sentence. ui.Trust prints the
// full line; this only needs a short phrase that fits into "it was X, now it is Y".
func insTrustWord(level string) string {
	switch level {
	case store.TrustSigned:
		return "signed"
	case store.TrustHost:
		return "host account"
	case store.TrustIndexed:
		return "packaging instructions from the index"
	case store.TrustUnverified:
		return "unverified"
	case "":
		return "no trust level recorded"
	default:
		return fmt.Sprintf("unrecognized level %q", level)
	}
}

// insItems works out what this install has to download, per shape.
func insItems(res *resolved) ([]insItem, error) {
	tf := res.Toyfile
	version := insVersion(res)

	if tf.IsScript() {
		// A [script] value is a path inside the repository ("gitwip.sh" in the
		// example), fetched through the raw template rather than as a release asset
		// — the whole point of a single-file script is not having to cut a release.
		out := make([]insItem, 0, len(tf.Script))
		for _, name := range slices.Sorted(maps.Keys(tf.Script)) {
			asset := toyfile.Expand(tf.Script[name], version, res.Tag)
			// The expanded string is what actually goes into the URL, and {tag}
			// comes from upstream. Check it again.
			if err := insSafeAsset(name, asset); err != nil {
				return nil, err
			}
			u, err := insAssetURL(res, asset, true)
			if err != nil {
				return nil, err
			}
			out = append(out, insItem{name: name, asset: asset, url: u})
		}
		return out, nil
	}

	cands := platform.Candidates()
	asset, want, matched, err := tf.AssetFor(cands, version, res.Tag)
	if err != nil {
		return nil, err
	}
	// Matching something other than the first candidate means what gets installed
	// may not be "the build made for this machine". Iron law 8: say so. Judging that
	// correctly needs to know which platforms the author declared — see
	// insFallbackNote.
	if note := insFallbackNote(cands, matched, tf.Platforms()); note != "" {
		ui.Warn("%s", note)
	}
	u, err := insAssetURL(res, asset, false)
	if err != nil {
		return nil, err
	}
	return []insItem{{name: asset, asset: asset, url: u, want: want}}, nil
}

// insFallbackNote returns a plain-English line when the match was not the first
// candidate. Matching the first one returns the empty string.
//
// Iron law 8: if what gets installed is not "the build made for this machine", the
// user has to know — neither of these failures looks like a fallback problem when it
// hits. Rosetta quietly runs an Intel build a good deal slower; a glibc build dropped
// into Alpine reports `not found` while the file is sitting right there.
//
// But "did not match the first candidate" is NOT the same as "did not get a build
// made for this machine": on linux toy ALWAYS synthesizes a libc suffix as the first
// candidate (gnu / musl), while the vast majority of projects publish only two-segment
// keys. That would add a bogus warning to every ordinary install and drown out the
// musl and Rosetta warnings that actually matter — a warning that fires every time is
// no warning at all. So `declared` (which platforms the author listed) is part of the
// test: a bare linux/amd64 is almost always the glibc build, and for a glibc machine
// it IS the right one — unless the author split by libc themselves, in which case
// what the generic entry holds is anybody's guess. A musl machine always gets told,
// the other way round: a bare linux/amd64 is almost never a musl build.
func insFallbackNote(cands []string, matched string, declared []string) string {
	if len(cands) == 0 || matched == "" || matched == cands[0] {
		return ""
	}
	want := cands[0]
	if want == matched+"/gnu" && !insSplitsBy(matched, declared) {
		return ""
	}
	switch {
	case strings.HasSuffix(want, "/gnu") && matched == strings.TrimSuffix(want, "/gnu")+"/musl":
		// A glibc machine falling back to the musl entry (the last candidate in
		// platform.candidatesFor). This is almost always good news — musl builds
		// are nearly always statically linked, so they run fine on a glibc machine,
		// and it is the only reason projects that ship musl only, like ripgrep and
		// zoxide, are installable at all. Say it anyway: this is not "the build made
		// for this machine" (iron law 8), and on the off chance that musl build is
		// dynamically linked, it will not start here.
		return "this package only ships a musl build for " + strings.TrimSuffix(want, "/gnu") + ", so that is the one toy uses. " +
			"musl builds are almost always statically linked, so it runs fine on this glibc machine. " +
			"If this one happens to be dynamically linked it will say \"file not found\" when you run it — " +
			"ask upstream for a glibc build if that happens"
	case want == "darwin/arm64" && matched == "darwin/amd64":
		return "this package has no Apple Silicon build, so the Intel one (darwin/amd64) is what toy uses. " +
			"It runs under Rosetta 2 translation — it works, just slower. Without Rosetta 2 installed macOS " +
			"will refuse to open it; `softwareupdate --install-rosetta` fixes that"
	case want == matched+"/gnu":
		// Reaching here means the author really did split by libc, they just have
		// no glibc entry.
		return "this package splits its builds by libc but has no " + want + " entry, so the generic " + matched +
			" build is the one toy uses — a generic build is usually the glibc one, the author just did not say so"
	case strings.HasSuffix(want, "/musl"):
		return "this machine uses musl (Alpine and friends), and this package only has a generic Linux build (" + matched + "). " +
			"If that build is dynamically linked against glibc it will not start here — and the error looks like the file " +
			"is missing (`not found` / `no such file or directory`) when what is really missing is the dynamic loader"
	default:
		return "there is no " + want + " build, so toy falls back to " + matched + " — usable, but not built for this machine"
	}
}

// insSplitsBy reports whether the author split their builds along a finer axis under
// base (linux/amd64/musl beneath linux/amd64, say). Only a split means the generic
// entry is no longer "the default one".
func insSplitsBy(base string, declared []string) bool {
	for _, p := range declared {
		if strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

// insAssetURL builds the download URL for one asset.
func insAssetURL(res *resolved, asset string, script bool) (string, error) {
	if res.Ref.Kind == ref.KindURL {
		// The plain-URL route has no host, so an asset can only be resolved
		// relative to the URL of the toy.toml itself — there is nothing else that
		// says where it lives.
		base, err := url.Parse(res.Ref.URL)
		if err != nil {
			return "", fmt.Errorf("%s does not parse, so the asset URL cannot be built: %w", res.Ref.URL, err)
		}
		rel, err := url.Parse(asset)
		if err != nil {
			return "", fmt.Errorf("the asset name %q cannot be read as a relative URL: %w", asset, err)
		}
		return base.ResolveReference(rel).String(), nil
	}
	var u, what string
	if script {
		u, what = res.Host.RawURL(res.Owner, res.Repo, res.Tag, asset), "raw"
	} else {
		u, what = res.Host.ReleaseURL(res.Owner, res.Repo, res.Tag, asset), "release"
	}
	if u == "" {
		return "", fmt.Errorf("host %q has no %s template, so the download URL cannot be built. "+
			"Add %s = \"…\" under [hosts.%s] in ~/.toy/config.toml",
			res.HostName, what, what, res.HostName)
	}
	return u, nil
}

// insUnpack puts one downloaded file into the package directory.
// Iron laws 1 and 2: unpacking and chmod, nothing else. No step here ever executes
// anything from the package.
func insUnpack(tf *toyfile.Toyfile, it insItem, pkgDir string) error {
	switch {
	case tf.IsScript():
		// Single-file script: download + chmod 0755 + symlink. Nothing unpacked,
		// nothing built (spec §3).
		return insCopy(it.path, filepath.Join(pkgDir, it.name), 0o755)
	case archive.IsArchive(it.asset):
		return insExtract(it.path, it.asset, pkgDir)
	case archive.SingleFileCompression(it.asset) != "":
		// One binary, squeezed, with no container around it -- restic ships
		// restic_0.19.1_linux_amd64.bz2 and nothing else for linux/amd64. It lands
		// at the [bin] path for the same reason a bare binary does, below.
		dst, err := insSingleTarget(tf, it, pkgDir)
		if err != nil {
			return err
		}
		return archive.Decompress(it.path, it.asset, dst)
	default:
		// Bare binary: land it at the path [bin] names, not under the asset name.
		//
		// Asset names VARY BY PLATFORM (yq_linux_amd64 / yq_darwin_arm64 /
		// yq_linux_arm64 …), while [bin] is a single line that does not vary by
		// platform and expands no placeholders. Write it out under the asset name
		// and every platform except the one [bin] happens to spell correctly gets a
		// symlink to a file that does not exist — and it blows up only after the
		// download and the checksum both succeeded. That is how the mikefarah/yq
		// entry in the index was broken on 5 of its 6 platforms: when it was added
		// it had only been verified on linux/amd64.
		//
		// Do this only when [bin] has exactly one entry: with several, picking one
		// is a guess, so keep the old behaviour and let store.Link fail loudly.
		full, err := insSingleTarget(tf, it, pkgDir)
		if err != nil {
			return err
		}
		return insCopy(it.path, full, 0o755)
	}
}

// insExtract unpacks into pkgDir.
//
// archive detects the format FROM THE FILE NAME SUFFIX, and a cached file is named
// after its own content hash with no suffix at all (spec §6: the file name IS the
// sha256, which is what makes the cache a mirror directory for free) — hand it a
// cache path directly and the answer is always "unrecognized archive format". So
// stage a symlink under the original asset name in a temp directory first.
func insExtract(src, asset, pkgDir string) error {
	tmp, err := os.MkdirTemp("", "toy-extract-")
	if err != nil {
		return fmt.Errorf("cannot create a temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	named := filepath.Join(tmp, filepath.Base(asset))
	if err := os.Symlink(src, named); err != nil {
		return fmt.Errorf("cannot stage the archive for extraction: %w", err)
	}
	if _, err := archive.Extract(named, pkgDir); err != nil {
		return err
	}
	return nil
}

// insCopy copies a downloaded file into the package directory.
// Not a hard link: cache files are 0644 and `toy serve` hands them out as a mirror,
// and sharing one inode would mean the chmod +x here also modifies the cached copy.
func insCopy(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("cannot read the downloaded file %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("writing %s failed: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("writing %s failed: %w", dst, err)
	}
	// The perm passed to O_CREATE gets trimmed by umask: 0755 under umask 022, but
	// 0700 under umask 077 — and this file is the one the user has to be able to run.
	return os.Chmod(dst, perm)
}

// insLink symlinks every command, rolling back everything it created if one fails.
// Half a set of commands left on PATH is worse than none: you find out one is
// missing only when you go to run it.
func insLink(st *store.Store, tf *toyfile.Toyfile, pkgDir string, old *store.Manifest, force bool) ([]string, error) {
	targets := map[string]string{}
	if tf.IsScript() {
		for name := range tf.Script {
			targets[name] = name // scripts land in the package dir under the command name
		}
	} else {
		maps.Copy(targets, tf.Bin)
	}

	// On an upgrade or a reinstall, a symlink of the same name is one toy created
	// last time — replacing it needs no further confirmation from the user. Every
	// other name still goes through the shadowing check (iron law 5).
	owned := map[string]bool{}
	if old != nil {
		for _, l := range old.Links {
			owned[filepath.Base(l)] = true
		}
	}

	var links []string
	for _, name := range slices.Sorted(maps.Keys(targets)) {
		target := filepath.Join(pkgDir, filepath.FromSlash(targets[name]))
		// zip usually carries no execute bit, and 0644 binaries turn up in tars
		// too. Fix it before linking, or the install produces a command that
		// exists but cannot run.
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 == 0 {
			os.Chmod(target, fi.Mode().Perm()|0o755)
		}
		// Try once with nothing waived: that way whatever --force overrode can be
		// reported in store's own wording, instead of reimplementing the shadowing
		// check here (iron law 8). owned[name] is a link toy created last time —
		// replacing that one is not a safety waiver.
		lp, err := st.Link(name, target, owned[name])
		if err != nil && force && (errors.Is(err, store.ErrShadow) || errors.Is(err, store.ErrLinkExists)) {
			ui.Warn("--force waived: %v", err)
			lp, err = st.Link(name, target, true)
		}
		if err != nil {
			for _, done := range links {
				if uerr := st.Unlink(done); uerr != nil {
					ui.Warn("cannot remove %s while rolling back: %v", done, uerr)
				}
			}
			// Only an actually-created link can have displaced the previous
			// version's. If none were created, say nothing — otherwise a clean
			// refusal reads like it broke the installed package.
			if old != nil && len(links) > 0 {
				ui.Warn("the symlinks from the previous version of %s may already have been replaced; running `toy install` or `toy update` again restores them", old.Name)
			}
			return nil, err
		}
		links = append(links, lp)
	}
	return links, nil
}

// insCleanOld tidies up the version that was replaced: commands the new version no
// longer provides, and the old package directory. All best-effort — the new version
// is already installed and written to the manifest, and failing to tidy up must not
// fail the install.
func insCleanOld(st *store.Store, old *store.Manifest, pkgDir string, links []string) {
	if old == nil {
		return
	}
	for _, l := range old.Links {
		if slices.Contains(links, l) {
			continue
		}
		if err := st.Unlink(l); err != nil {
			ui.Warn("cannot remove %s left behind by the old version: %v", l, err)
		}
	}
	if old.PkgDir == "" || old.PkgDir == pkgDir {
		return
	}
	if err := insRemovePkgDir(st, old.PkgDir); err != nil {
		ui.Warn("cannot clean up the old version's package directory: %v", err)
	}
}

// insTOFU runs one trust-on-first-use check (spec §4; the model is copied from SSH's
// known_hosts). A change stops the install unless --force — and both the pinned
// value and the new one have to be printed, or the user has nothing to judge by.
func insTOFU(ring *sign.Ring, id, kind, value, what string, force bool) error {
	trust, old, err := ring.Check(id, kind, value)
	if err != nil {
		return fmt.Errorf("cannot read the trust-on-first-use record (%s/%s): %w", kind, id, err)
	}
	if trust != sign.TrustChanged {
		return nil
	}
	if !force {
		// One line, no breaks: ui escapes a newline to \x0a — it has to, because
		// anyone who can inject a newline can overwrite the trust line (iron law
		// 8) — so a \n here would only turn into noise.
		return fmt.Errorf("%w: %s for %s does not match what was pinned on first install — pinned %s, "+
			"got %s this time. The author may have re-cut the same version, or somebody may have swapped the "+
			"contents. Check it yourself, then add --force to overwrite the pin", sign.ErrChanged, what, id, old, value)
	}
	ui.Warn("--force waived: %s for %s changed (pinned %s -> now %s)", what, id, old, value)
	return nil
}

// insExisting reads the manifest for the version of this package already installed.
// Not installed returns nil, which is not an error.
//
// It looks up the package id first, then falls back to the index entry's id: the
// moment an index entry's source changes, insPackageID changes with it, and looking
// up only by that makes "what was installed last time" disappear from view — so the
// source change is not caught and the old manifest is never cleaned up.
func insExisting(st *store.Store, res *resolved) (*store.Manifest, error) {
	m, err := st.LoadManifest(insPackageID(res))
	if err == nil {
		return &m, nil
	}
	if !errors.Is(err, store.ErrNotInstalled) {
		return nil, err
	}
	idxID := insIndexID(res)
	if idxID == "" {
		return nil, nil
	}
	ms, err := st.ListManifests()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.IndexID == idxID {
			return &m, nil
		}
	}
	return nil, nil
}

// insSourceChanged reports whether what is being installed now comes from a
// different source.
//
// An index entry is one pointer line inside a JSON file anyone can open a PR
// against. Change the source from github:alice/tool to github:evil/tool and
// `toy update` happily re-resolves against the new pointer while not one of the
// three gates fires: with neither side signed, insTrustDowngraded is always false;
// the TOFU id carries the source, so it becomes a brand-new record; and the trust
// line reads the same as ever. So a changed source is treated like a trust
// downgrade — an upgrade must not quietly change who the user is trusting.
func insSourceChanged(old *store.Manifest, res *resolved) bool {
	return old != nil && old.ID != "" && old.ID != insPackageID(res)
}

// insSwapIn swings the unpacked staged directory into place as pkgDir and returns
// the old directory it moved aside (empty string when there was none).
func insSwapIn(st *store.Store, staged, pkgDir string) (string, error) {
	backup := ""
	if _, err := os.Stat(pkgDir); err == nil {
		backup = pkgDir + ".old"
		if err := insRemovePkgDir(st, backup); err != nil {
			os.RemoveAll(staged)
			return "", err
		}
		if err := os.Rename(pkgDir, backup); err != nil {
			os.RemoveAll(staged)
			return "", fmt.Errorf("cannot move the old package directory %s out of the way: %w", pkgDir, err)
		}
	}
	if err := os.Rename(staged, pkgDir); err != nil {
		insRestore(st, backup, pkgDir)
		os.RemoveAll(staged)
		return "", fmt.Errorf("cannot move the new package into %s: %w", pkgDir, err)
	}
	return backup, nil
}

// insRestore puts back the version insSwapIn moved aside. Best-effort: this path
// already has an error to report, and a second one would only bury the real cause.
func insRestore(st *store.Store, backup, pkgDir string) {
	if backup == "" {
		return
	}
	_ = insRemovePkgDir(st, pkgDir)
	if err := os.Rename(backup, pkgDir); err != nil {
		ui.Warn("cannot put the previous version back at %s: %v (running `toy install` again restores it)", pkgDir, err)
	}
}

// insRemovePkgDir deletes a package directory, after confirming it really is inside
// ~/.toy. A manifest is a file on disk, and a tampered pkg_dir must never become
// authorization to "delete /" (store.Unlink guards symlinks the same way; this is
// the matching guard).
func insRemovePkgDir(st *store.Store, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("%q cannot be turned into an absolute path: %w", dir, err)
	}
	root := filepath.Clean(st.Root)
	if abs != root && !strings.HasPrefix(filepath.Clean(abs), root+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to delete %s — it is not inside %s, and toy only deletes what it installed itself", abs, root)
	}
	if abs == root {
		return fmt.Errorf("refusing to delete %s — that is the whole toy directory, not one package", abs)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("cannot delete the package directory %s: %w", abs, err)
	}
	return nil
}

// insPackageID is this package's identity in installed/ and in the keyring. It does
// not change across versions.
func insPackageID(res *resolved) string {
	if res.Ref.Kind == ref.KindURL {
		return res.Ref.URL
	}
	return res.HostName + ":" + res.Owner + "/" + res.Repo
}

// insIndexID is this package's id in the index, or the empty string when no index
// was involved.
func insIndexID(res *resolved) string {
	if res.Entry == nil {
		return ""
	}
	return res.Entry.ID
}

// insKeyID is the id under the "key" dimension of the TOFU keyring.
//
// It has to be the index entry's id: an author public key only ever appears in an
// index entry, and insPackageID is derived from that record's source — so editing
// one source line amounts to a new identity, and "the author's key changed" never
// reaches Check at all, while spec §4 requires a changed key to stop the install.
func insKeyID(res *resolved) string {
	if id := insIndexID(res); id != "" {
		return id
	}
	return insPackageID(res)
}

// insNameSeg is <this segment> in pkg/<host>/<owner>/<this segment>/<version>.
//
// It cannot just be the name from toy.toml: that is whatever the author felt like
// writing, and it is perfectly legal for tool-rs and tool-go under the same owner to
// both say name = "tool" — at which point two packages share one install directory.
// Installing the second silently deletes the first one's files (while the first one's
// manifest and symlinks are still there), and uninstalling the second takes the
// first one's directory with it. A repository name is unique within an owner by
// construction; a plain URL has no repository name, so only a hash of the URL itself
// can tell two of them apart.
func insNameSeg(res *resolved, tf *toyfile.Toyfile) string {
	if res.Ref.Kind != ref.KindURL && res.Repo != "" {
		return res.Repo
	}
	sum := sha256.Sum256([]byte(insPackageID(res)))
	return tf.Name + "-" + hex.EncodeToString(sum[:])[:8]
}

// insTrustID is the id under the sha256 dimension of the TOFU keyring.
//
// Iron law 6: once the hash of a given ref@version is recorded it never points
// anywhere else — so it has to carry the tag that was resolved. Write only
// owner/repo and an ordinary upgrade to the next version gets reported as "the
// checksum changed", and a warning that shows up daily is one nobody takes seriously.
func insTrustID(res *resolved) string {
	id := insPackageID(res)
	if res.Tag == "" {
		return id
	}
	return id + "@" + res.Tag
}

// insVersion is the version that fills the {version} placeholder. The plain-URL
// route has no version, so it returns the empty string — inventing one would be a
// lie (iron law 8), and resolve already guarantees that route carries no
// placeholders.
func insVersion(res *resolved) string {
	if res.Tag == "" {
		return ""
	}
	return res.Version.Number()
}

// insVersionLabel is the version shown in the manifest and on screen.
func insVersionLabel(res *resolved) string {
	if v := insVersion(res); v != "" {
		return v
	}
	return insNoVersion
}

func insDirVersion(res *resolved) string { return insVersionLabel(res) }

// insHostSeg / insOwnerSeg are the <host>/<owner> segments of pkg/<host>/<owner>/….
// A plain URL has neither a host nor an owner, so url/<domain> stands in — otherwise
// every package installed from a plain URL piles into the same directory.
func insHostSeg(res *resolved) string {
	if res.Ref.Kind == ref.KindURL {
		return "url"
	}
	return res.HostName
}

func insOwnerSeg(res *resolved) string {
	if res.Ref.Kind != ref.KindURL {
		return res.Owner
	}
	if u, err := url.Parse(res.Ref.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return "unknown"
}

// insTrustDetail is the detail that follows the trust level on the line printed
// after an install (spec §4).
func insTrustDetail(res *resolved) string {
	switch res.Trust {
	case store.TrustSigned:
		// NEVER print the repository owner after the check mark. The signed message
		// is id + source + the verbatim toy.toml, all three publicly readable, and
		// not one byte of it binds this public key to that owner — anyone can sign
		// somebody else's unclaimed record with their own key, and "Derrity (key …)"
		// reads as "Derrity signed this" (iron law 8). All this check mark can say
		// is: the key registered in the index signed this version of toy.toml.
		detail := "key " + res.Signer
		if res.IndexName != "" {
			detail += " · registered in index " + res.IndexName
		}
		if who := qryHostLabel(res); who != "" && who != "/" {
			detail += " (this is not a claim about " + who + " themselves)"
		}
		return detail
	case store.TrustHost:
		// What we want is "github.com/Derrity": what the user trusts is that
		// account on that host.
		return qryHostLabel(res)
	case store.TrustIndexed:
		// The ui.Trust line already spells out which half is not the author's.
		// Add the two things it does not know: which index supplied these
		// packaging instructions, and whose release the bytes come from.
		var parts []string
		if res.IndexName != "" {
			parts = append(parts, "supplied by index "+res.IndexName)
		}
		if who := qryHostLabel(res); who != "" && who != "/" {
			parts = append(parts, "release from "+who)
		}
		if res.Signer != "" {
			// A signature on the inline level is still not "the author signed
			// it" (rule 2), and the wording has to nail that down.
			parts = append(parts, "key "+res.Signer+" (proves only that this index record was not altered)")
		}
		return strings.Join(parts, " · ")
	default:
		return ""
	}
}

// insSafeAsset blocks the dangerous paths that only appear after placeholders are
// expanded. {tag} comes from upstream, and a tag named "../../x" can aim the
// download URL somewhere else entirely while the post-install line still faithfully
// carries the author's name (iron laws 7 and 8). The [artifact] route is already
// checked by toyfile.Asset; this one covers [script].
func insSafeAsset(name, asset string) error {
	switch {
	case strings.TrimSpace(asset) == "":
		return fmt.Errorf("[script] %q expands to nothing, so there is nothing to install", name)
	case strings.HasPrefix(asset, "/"), filepath.IsAbs(asset):
		return fmt.Errorf("[script] %q expands to the absolute path %q; only paths relative to the repository are allowed", name, asset)
	case strings.ContainsAny(asset, "?#"):
		return fmt.Errorf("[script] %q expands to %q, which contains ? or # — pasted into a URL that fetches a different file", name, asset)
	}
	for _, seg := range strings.Split(filepath.ToSlash(asset), "/") {
		if seg == ".." {
			return fmt.Errorf(`[script] %q expands to %q, which has a ".." segment and would point outside the repository`, name, asset)
		}
	}
	return nil
}

func insBaseNames(links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, filepath.Base(l))
	}
	return out
}

// insParseFlags parses a subcommand's flags. The flag package's own usage layout
// duplicates main's, so its output is discarded and only the specific error survives;
// main prints the usage line (errUsage).
func insParseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h used to land on main's one-line syntax string and say nothing at
			// all about what each option means. And install's --reinstall and
			// --force are exactly the pair that has to be spelled out here: one is
			// an everyday operation, the other means "I checked it myself, let the
			// safety check through". main prints the syntax line right after this,
			// so all that is added here is the options. A subcommand with no
			// options at all (update) must not print an empty "options" heading.

			n := 0
			fs.VisitAll(func(*flag.Flag) { n++ })
			if n > 0 {
				fmt.Fprintf(ui.Output(), "options for toy %s (they go before the positional arguments):\n", fs.Name())
				fs.SetOutput(ui.Output())
				fs.PrintDefaults()
			}
		} else {
			ui.Error("%v", err)
		}
		return errUsage
	}
	return nil
}

// insStaleEntryNote explains a download failure that is very likely OUR fault
// rather than the package's.
//
// An inline entry (spec §5.1) is packaging instructions the index wrote on behalf
// of a repository that has no toy.toml of its own. It carries no version, so it
// follows whatever tag upstream published last — which means it keeps working
// across releases right up until the author changes how they name their assets.
// At that moment the asset name in the entry stops existing and every user gets
// a bare 404.
//
// Without this line the user reads that as "the package is broken" and goes to
// bother the author, who did nothing wrong and cannot fix it either. We wrote
// that entry; the report belongs to us. This is the operational cost of §5.1,
// and it is only fair to make it cheap to report.
//
// Only for entries that actually came from an index. An author's own toy.toml
// returning 404 is genuinely their release being broken, and blaming our index
// there would be its own kind of lie (iron law 8).
func insStaleEntryNote(res *resolved, err error) string {
	if res == nil || res.Toyfile == nil || res.Toyfile.Origin != toyfile.OriginIndex {
		return ""
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ""
	}
	where := "the index"
	if res.IndexName != "" {
		where = "index " + res.IndexName
	}
	return "that asset no longer exists upstream. The packaging instructions for this package " +
		"come from " + where + ", not from the author, and they are probably out of date — " +
		"the author most likely renamed their release assets. This is ours to fix, not theirs: " +
		"please report it at " + issueURL + " and we will regenerate the entry."
}

// insSingleTarget is where a one-file artifact lands: the path on the right of
// [bin] when there is exactly one of them, and the asset's own name otherwise.
// Shared by the bare-binary and the single-compressed-file cases, which have the
// same problem for the same reason -- see the comment in insUnpack.
func insSingleTarget(tf *toyfile.Toyfile, it insItem, pkgDir string) (string, error) {
	dst := filepath.Base(it.asset)
	if len(tf.Bin) == 1 {
		for _, target := range tf.Bin {
			dst = target // toyfile already guarantees no absolute path and no ..
		}
	}
	full := filepath.Join(pkgDir, dst)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("cannot create the directory %s: %w", filepath.Dir(full), err)
	}
	return full, nil
}

// insPerformOrPrevious installs res, and when that fails because res's release file
// is not there, installs the release before it instead.
//
// A tag routinely goes up before its release files do: the author pushes the tag and
// CI spends the next hour building and uploading. rclone's v1.75.2 was tagged at 09:45
// UTC and its first file arrived at 10:50, and for that whole window `toy install
// rclone` failed for everyone -- with a note blaming a renamed asset and asking them to
// report it. toy picks versions from tags and never asks a forge's API whether a
// release is published (spec §2), so it cannot see this coming; it can only recover,
// and say plainly that it did.
//
// Only when the version was picked rather than asked for (res.PrevTag is empty after
// --version), and only on a definite 404: a 500 or a timeout is upstream having a bad
// day, and moving someone to an older release over it would be wrong.
func insPerformOrPrevious(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref, res *resolved, old *store.Manifest, force bool) error {
	err := insPerform(ctx, st, cfg, res, old, force)
	if err == nil || res.PrevTag == "" || !errors.Is(err, os.ErrNotExist) {
		insWarnStale(res, err)
		return err
	}
	// The wording is read by scripts/index-check.sh to tell "still publishing" from
	// "renamed"; keep "<name> <tag> is tagged, but its release file" intact.
	ui.Warn("%s %s is tagged, but its release file is not there. Usually upstream is still publishing it -- "+
		"tags often go up well before the files do -- though it can also mean the file was renamed. "+
		"Installing %s, the release before it; `toy update` moves to %s once its files are up",
		res.Repo, res.Tag, res.PrevTag, res.Tag)
	r.Version = res.PrevTag
	prev, perr := resolve(ctx, st, cfg, r)
	if perr != nil {
		return fmt.Errorf("%w (and the release before it, %s, did not resolve either: %v)", err, res.PrevTag, perr)
	}
	if old != nil && old.ID == insPackageID(prev) && old.Tag == prev.Tag {
		ui.Info("%s is already at %s, nothing to do", old.Name, old.Tag)
		return insErrUpToDate
	}
	err = insPerform(ctx, st, cfg, prev, old, force)
	insWarnStale(prev, err)
	return err
}

// insWarnStale prints the "this index entry is probably out of date" note when err is
// the kind of failure it describes. It runs once the fallback has had its chance: a
// missing file for a release that is merely still being published is not stale.
func insWarnStale(res *resolved, err error) {
	if note := insStaleEntryNote(res, err); note != "" {
		ui.Warn("%s", note)
	}
}
