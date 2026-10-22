package main

// gen: read a list of owner/repo lines, produce an inline entry for each, merge them
// into the index JSON.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

type genConfig struct {
	repos  []string
	out    string
	update bool
	probe  bool
	prov   provider
	host   host.Host // the release download URL for --probe, and the raw URL of upstream's toy.toml
	lists  lists     // index/licenses.txt and index/delisted.txt
}

// genResult is how one repo ended up. Three ways: an entry, a deliberate skip, a failure.
type genResult struct {
	id      string
	entry   index.Entry
	pack    pack
	skip    string   // non-empty = deliberately skipped (already in the index, would downgrade, ...)
	pointer bool     // upstream ships its own toy.toml; the entry only says where to look
	bins    []string // for a pointer entry: the command names upstream's toy.toml puts on PATH
	err     error
	written bool
}

func runGen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // printUsage owns the usage text; do not let the flag package lay out its own
	repos := fs.String("repos", "", "")
	out := fs.String("out", filepath.Join("index", "toy-index.json"), "")
	update := fs.Bool("update", false, "")
	probe := fs.Bool("probe", false, "")
	token := fs.String("token", "", "")
	licenses := fs.String("licenses", filepath.Join("index", "licenses.txt"), "")
	delisted := fs.String("delisted", filepath.Join("index", "delisted.txt"), "")
	declined := fs.String("declined", filepath.Join("index", "declined.txt"), "")
	api := fs.String("api", "", "") // host API root, for tests and self-hosted instances only
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*repos) == "" {
		return errUsage
	}

	list, err := readRepoList(*repos)
	if err != nil {
		return err
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	gh, err := host.Lookup("github", nil)
	if err != nil {
		return err
	}
	ls, err := loadLists(*licenses, *delisted, *declined)
	if err != nil {
		return err
	}
	return gen(ctx, genConfig{
		repos: list, out: *out, update: *update, probe: *probe,
		prov: newGitHub(*api, tok), host: gh, lists: ls,
	})
}

func gen(ctx context.Context, cfg genConfig) error {
	idx, err := loadIndex(cfg.out)
	if err != nil {
		return err
	}

	results := make([]genResult, 0, len(cfg.repos))
	changed := 0
	for _, line := range cfg.repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := genOne(ctx, cfg, idx, line)
		if r.err == nil && r.skip == "" {
			idx.Upsert(r.entry)
			r.written = true
			changed++
		}
		results = append(results, r)
	}

	if changed > 0 {
		if err := writeIndex(cfg.out, idx); err != nil {
			return err
		}
	}
	report(cfg, results, changed)

	// One repo failing does not affect the others, but a whole batch producing nothing
	// must not exit 0 -- in CI that would pass silently.
	if changed == 0 && anyFailed(results) {
		return errors.New("no entries produced (every line above says why)")
	}
	return nil
}

// genOne handles one repo. Any failure lands on this one line (in the return value) and
// never interrupts the batch.
func genOne(ctx context.Context, cfg genConfig, idx *index.Index, line string) genResult {
	res := genResult{id: line}

	r, err := ref.Parse(line)
	// Shape validation borrows ref instead of keeping another allowlist here: these two
	// segments end up joined into ~/.toy/pkg/<host>/<owner>/<name>/, ref already stops
	// ".." and over-long segments there, and index.validID uses the same thing -- a second
	// copy just drifts apart over time.
	switch {
	case err != nil:
		res.err = fmt.Errorf("cannot parse: %w", err)
		return res
	case r.Version != "":
		res.err = errors.New("drop the @version: an index entry names a repo, and which version gets installed is decided by git tags (spec §5)")
		return res
	case r.Kind != ref.KindIndex && !(r.Kind == ref.KindHost && r.Host == "github"):
		res.err = errors.New("only owner/repo or github:owner/repo is accepted here")
		return res
	}
	res.id = r.ID()

	// A maintainer who asked to be delisted stays delisted, whatever list this line came
	// from -- an automated batch putting them back would break the one promise the
	// delist form makes.
	if cfg.lists.delisted[foldRepo(res.id)] {
		res.skip = "delisted at its maintainer's request (index/delisted.txt); never listed again"
		return res
	}
	if cfg.lists.declined[foldRepo(res.id)] {
		res.skip = "a reviewer decided it does not belong in the index (index/declined.txt)"
		return res
	}

	// Decide on entries already in the index first, saving an API call.
	if old, ok := idx.Find(res.id); ok {
		switch {
		case old.Key != "" || old.Sig != "":
			// Regenerating necessarily replaces the toyfile source, and the signature signs
			// exactly that source -- overwriting quietly makes a "✓" disappear. Spec §5
			// says a vanished signature must stop and let a person decide, so do not decide
			// for them here. --update does not unlock this either.
			res.skip = "the entry in the index is signed; leaving it alone (regenerating would invalidate the signature)"
			return res
		case old.Toyfile == "":
			// A pointer entry means upstream ships its own toy.toml, so the client installs
			// it at the host trust level. Switching to inline is a TrustHost -> TrustIndexed
			// downgrade (§5.1 rule 6), not a refresh.
			res.skip = "the entry in the index is a pointer (upstream ships its own toy.toml); inlining it would downgrade trust, so leaving it alone"
			return res
		case !cfg.update:
			res.skip = "already in the index, skipping (pass --update to overwrite)"
			return res
		}
	}

	info, err := cfg.prov.Fetch(ctx, r.Owner, r.Repo)
	if err != nil {
		res.err = err
		return res
	}
	if info.License, err = decideLicence(res.id, info.License, info.LicState, cfg.lists.reviewed); err != nil {
		res.err = err
		return res
	}
	// The client picks versions with `git ls-remote --tags` + semver (spec §2), so a
	// release tag has to be something semver can find a version in. Since the
	// prefixed-tag fallback landed that includes jqlang/jq's `jq-1.8.2` shape, but
	// not a tag with no version in it at all. Checking here rather than letting it
	// through is the same judgement as unstableBinPath: an entry nobody can install
	// does not belong in the index, and `toy install` failing at step one gives no
	// hint that the tag SHAPE was the problem.
	if _, verr := semver.Max([]string{info.Tag}, true, r.Repo); verr != nil {
		res.err = fmt.Errorf("the latest release is tagged %q, and semver can find no version in it "+
			"(it has to look like v1.2.3, or carry one after a package name like jq-1.8.2). "+
			"The client picks versions with `git ls-remote --tags` + semver, so it would not recognise a "+
			"single version here -- listing this repo produces an entry nobody can install", tomlSafe(info.Tag, 64))
		return res
	}

	// Upstream ships its own toy.toml: the right entry only says where to look. The
	// client lets upstream win anyway (§5.1 rule 1), so an inlined copy would never be
	// read; writing one anyway would only file the package a trust tier lower and leave
	// a second copy of the packaging to rot.
	switch tf, err := upstreamToyfile(ctx, cfg.host, r.Owner, r.Repo, info.Tag); {
	case err != nil:
		res.err = err
		return res
	case tf != nil:
		res.pointer = true
		for name := range tf.Bin {
			res.bins = append(res.bins, name)
		}
		for name := range tf.Script {
			res.bins = append(res.bins, name)
		}
		sort.Strings(res.bins)
		res.entry = index.Entry{
			ID:     res.id,
			Source: "github:" + r.Owner + "/" + r.Repo,
			Desc:   tomlSafe(info.Desc, descMax),
			Stars:  info.Stars,
		}
		return res
	}

	p, err := makePack(r.Owner, r.Repo, info)
	if err != nil {
		res.err = err
		return res
	}

	if cfg.probe {
		switch hit, cands, err := probeBin(ctx, cfg.host, p); {
		case err != nil:
			// A failed probe is not a failure: the entry still comes out, [bin] is just
			// still a guess, and the report says so.
			p.notes = append(p.notes, "probe failed: "+err.Error())
		case hit != "":
			if why := unstableBinPath(hit, p.res.Version); why != "" {
				// When the probed path cannot be trusted, FAIL THE WHOLE ENTRY and produce
				// nothing: a record that installs today and breaks the moment upstream cuts a
				// release is the same class of mistake as leaving [sha256] in. The error
				// quotes the probed path verbatim, so whoever writes the entry by hand has the
				// answer right in front of them. A single wrapping top-level directory is
				// already stripped on extract, so what reaches here is the kind that cannot be
				// stripped: more than one top-level entry, or a path that itself spells out a
				// version or a platform.
				res.err = fmt.Errorf("the probed in-package path is %q: %s. "+
					"Write this entry by hand -- `toy-index add --toyfile <file> owner/repo` -- or get the author to add their own toy.toml", hit, why)
				return res
			}
			p.binPath, p.binProbed = hit, true
			// The BASENAME of the probed file is the command name the author actually ships.
			// The default comes from the asset-name prefix (ripgrep-15.2.0-....tar.gz ->
			// ripgrep) while the file inside the package is called rg -- taking the default
			// would put a name on PATH that appears in no documentation anywhere (nobody has
			// ever written `ripgrep --version`), while the right of [bin] is correct, so it
			// installs and runs and simply nobody can name it. Once probed, the probe wins.
			if base := path.Base(hit); base != p.binName && cleanName(base) {
				p.notes = append(p.notes, fmt.Sprintf(
					"command name taken from the probed file: %q (the asset-name prefix was %q)", base, p.binName))
				p.binName = base
			}
		default:
			p.notes = append(p.notes, "the archive holds several executables, so [bin] cannot be settled "+
				"automatically -- pick one: "+strings.Join(head(cands, 8), ", "))
		}
	}

	if w := assetname.PlatformWord(p.binName); w != "" {
		res.err = fmt.Errorf("the command name %q carries the platform word %q: the asset names fooled the "+
			"inference, and under that name it would only ever be right on one kind of machine. "+
			"Write this entry by hand -- `toy-index add --toyfile <file> owner/repo` -- or get the author to add their own toy.toml", p.binName, w)
		return res
	}

	data, err := renderToyfile(p)
	if err != nil {
		res.err = err
		return res
	}
	res.pack = p
	res.entry = index.Entry{
		ID:      res.id,
		Source:  "github:" + r.Owner + "/" + r.Repo,
		Desc:    tomlSafe(info.Desc, descMax),
		Stars:   info.Stars,
		Toyfile: string(data),
	}
	return res
}

// upstreamToyfile reads the toy.toml upstream ships at tag, through the same raw-URL
// template the client uses. nil, nil means there is none.
//
// Only a definite 404 means "none". Anything else -- a 500, a timeout -- fails this repo
// instead of falling through to an inline entry: guessing "absent" on a bad network day
// is the §5.1 rule 3 mistake made at generation time, and the repo is retried next run.
func upstreamToyfile(ctx context.Context, h host.Host, owner, repo, tag string) (*toyfile.Toyfile, error) {
	if h.Raw == "" {
		return nil, nil // a host with no raw template (only tests build one)
	}
	data, err := fetch.GetBytes(ctx, h.RawURL(owner, repo, tag, "toy.toml"), fetch.Options{Name: "toy.toml"})
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("cannot tell whether upstream ships its own toy.toml at %s, so not guessing: %w", tomlSafe(tag, 64), err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("upstream ships a toy.toml at %s that does not parse, so the client would refuse it too: %w", tomlSafe(tag, 64), err)
	}
	return tf, nil
}

func loadIndex(path string) (*index.Index, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		ui.Info("%s does not exist yet; a new one will be created", path)
		return &index.Index{Version: index.Version}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read index %s: %w", path, err)
	}
	idx, skipped, err := index.Load(data, path)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		// Load drops bad records, and we are about to write the whole file back out --
		// saying nothing would mean quietly deleting somebody else's entries.
		ui.Warn("%s holds %d invalid records; they were dropped, and writing the file back removes them for good", path, skipped)
	}
	return idx, nil
}

func writeIndex(path string, idx *index.Index) error {
	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("cannot write index %s: %w", path, err)
	}
	return nil
}

func anyFailed(rs []genResult) bool {
	for _, r := range rs {
		if r.err != nil {
			return true
		}
	}
	return false
}

// report prints a summary for a human to read over.
//
// The table is laid out with tabwriter into an in-memory buffer and then handed to ui a
// line at a time -- descriptions, asset names and server text inside errors are all
// remote content, and printing them directly lets a "\r\x1b[32m✓" overwrite the lines
// above it (§14.4). ui escapes control characters into visible \xNN.
func report(cfg genConfig, rs []genResult, changed int) {
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rs {
		switch {
		case r.err != nil:
			fmt.Fprintf(tw, "✗\t%s\t%v\n", r.id, r.err)
		case r.skip != "":
			fmt.Fprintf(tw, "-\t%s\t%s\n", r.id, r.skip)
		case r.pointer:
			fmt.Fprintf(tw, "✓\t%s\tpointer entry: upstream ships its own toy.toml (bin %s)\n", r.id, strings.Join(r.bins, ", "))
		default:
			how := "guessed"
			if r.pack.binProbed {
				how = "probed"
			}
			fmt.Fprintf(tw, "✓\t%s\t%d platforms, bin %s -> %s (%s)\n",
				r.id, len(r.pack.res.Assets), r.pack.name, r.pack.binPath, how)
		}
	}
	tw.Flush()
	ui.Info("")
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		ui.Info("%s", l)
	}

	for _, r := range rs {
		for _, n := range r.pack.notes {
			ui.Warn("%s: %s", r.id, n)
		}
	}

	// §14.9 rule 2: the names on the left of [bin] are the one thing in this feature that
	// still needs a human to look. reservedLinkNames blocks git and the shadow check
	// blocks commands that already exist, but a name not yet on PATH can be claimed.
	//
	// This does NOT keep its own copy of the reserved list: that list lives in
	// internal/store, and a second copy is one more place that can fail silently (the
	// §14.1 lesson has exactly this shape). The client refuses at install time regardless;
	// all this does is put the names in front of a person.
	names := reportBinNames(rs)
	if len(names) > 0 {
		ui.Info("")
		ui.Info("These entries will put the following command names on PATH -- check every one before committing:")
		var nb strings.Builder
		ntw := tabwriter.NewWriter(&nb, 0, 0, 2, ' ', 0)
		for _, n := range names {
			fmt.Fprintf(ntw, "    %s\n", n)
		}
		ntw.Flush()
		for _, l := range strings.Split(strings.TrimRight(nb.String(), "\n"), "\n") {
			ui.Info("%s", l)
		}
	}

	ui.Info("")
	if changed > 0 {
		pointers := 0
		for _, r := range rs {
			if r.written && r.pointer {
				pointers++
			}
		}
		ui.Info("Wrote %d entries to %s (%d inline, %d pointing at upstream's own toy.toml).", changed, cfg.out, changed-pointers, pointers)
		if changed > pointers {
			ui.Info("Whoever installs an inline entry sees the downgraded trust level (spec §5.1), and inline entries")
			ui.Info("carry no [sha256] -- they follow upstream's newest tag, so pinning one version's bytes would turn")
			ui.Info("the next release into a checksum mismatch.")
		}
	} else {
		ui.Info("Nothing changed; %s was left untouched.", cfg.out)
	}
	if q := cfg.prov.Quota(); q != "" {
		ui.Info("%s", q)
	}
}

// reportBinNames is that last column of the report: which command names this batch of
// entries will put on PATH.
//
// It must print binName (the one that actually lands on PATH), not name (the package
// directory name) -- on a repo like ripgrep the two differ. This column is the only
// checkpoint §14.9 rule 2 leaves for a person.
func reportBinNames(rs []genResult) []string {
	var names []string
	for _, r := range rs {
		switch {
		case !r.written:
		case r.pointer:
			for _, b := range r.bins {
				names = append(names, fmt.Sprintf("%s\t← %s (its own toy.toml)", b, r.id))
			}
		default:
			names = append(names, fmt.Sprintf("%s\t← %s", r.pack.binName, r.id))
		}
	}
	return names
}
