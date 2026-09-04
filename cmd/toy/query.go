package main

// The discovery side: list / search / info / index / doctor.
//
// One rule governs all the formatting here: EVERYTHING MEANT FOR A HUMAN GOES
// THROUGH ui — that is, stderr, with control characters escaped on the way out. The
// desc in an index and the fields in a toy.toml are foreign text, and fmt.Fprintln
// straight to the terminal means one "\r\x1b[32m✓ signed" can paint over the trust
// line that was just printed. Iron law 8 says the client is the only security
// boundary there is, and a line somebody else can overwrite is not a boundary. So
// tables are laid out with tabwriter into an in-memory buffer first, then handed to
// ui.Info a line at a time. Only the machine-readable --json output goes to stdout
// (spec §9).

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/platform"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

const (
	// qrySearchLimit is how many results `toy search` shows by default.
	qrySearchLimit = 20
	// qryDescWidth is where the description column gets truncated (in runes). The
	// desc in an index has no length limit, and a few hundred words of it would
	// flatten the table.
	qryDescWidth = 72
	// qryIndexProbeTimeout caps how long doctor probes one index. The main process
	// context is only cancelled on Ctrl-C, so an index that never answers would leave
	// the whole check-up hanging.
	qryIndexProbeTimeout = 20 * time.Second
)

// ---------------------------------------------------------------- layout helpers

// qryFlags builds a FlagSet for one subcommand. Usage is printed by main throughout
// (errUsage), so the flag package's own usage output is switched off and only its
// error line survives.
func qryFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {}
	return fs
}

// qryCell replaces the control characters in a cell with visible \xNN.
//
// tabwriter uses \t and \n to divide cells, and desc is written by whoever wrote the
// index entry: one \t throws off every column after it on that row, and one \n
// conjures an extra row out of nothing (the second half of which reads exactly like
// a normal record). ui escapes as well in the end, but by then the columns have
// already been measured — this has to happen before tabwriter sees it. Runs of
// whitespace collapse to a single space too: a cell's contents have no business
// carrying their own column spacing, and that spacing is the raw material for
// painting a whole fake row out of one name.
func qryCell(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0 &&
		!strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	space := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			fmt.Fprintf(&b, `\x%02x`, r)
			space = false
			continue
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// qryCellWidth is the default cap on one table cell (in runes).
//
// name / desc / index name all come from toy.toml and from indexes, with no limit on
// either length or content. tabwriter sizes a column to its widest cell plus two, so
// one 300-column-wide name pushes every column to its right — including the trust
// level on EVERY ROW — off the screen, while that same cell has ample room for a
// hand-written "v3.2.1  ✓ signed 9f3c1a20  Derrity/cowsay". One malicious package
// would be enough to make the whole table's trust column disappear and forge its own
// row in its place (iron law 8).
const qryCellWidth = 24

// qryTrunc truncates by rune, appending an ellipsis when it cuts.
func qryTrunc(s string, n int) string {
	if n <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// qryTable prints rows as an aligned table. The first row is the header.
//
// tabwriter measures column widths in runes, while CJK characters occupy two cells in
// a terminal — so a column containing CJK text will be a cell or two out of alignment
// after it. Putting the freest column (description, source) last is enough; do not go
// implement an East Asian Width table over this.
//
// widths caps each column, defaulting to qryCellWidth, with 0 meaning no truncation.
// The last column is uncapped by default: there is nothing to its right to push off.
func qryTable(rows [][]string, widths ...int) {
	if len(rows) == 0 {
		return
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			limit := qryCellWidth
			if i == len(r)-1 {
				limit = 0
			}
			if i < len(widths) {
				limit = widths[i]
			}
			cells[i] = qryTrunc(qryCell(c), limit)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	// tabwriter only writes into an in-memory buffer, so Flush cannot fail in any
	// way worth handling.
	_ = w.Flush()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		ui.Info("%s", strings.TrimRight(line, " "))
	}
}

// qryTrustLabel is the trust-level cell in a table.
// Iron law 8: a level we do not recognize has to be said out loud, never smoothed
// over into whichever level happens to look safe.
func qryTrustLabel(trust, signer string) string {
	switch trust {
	case store.TrustSigned:
		if signer == "" {
			// Saying "signed" while unable to name the signer amounts to
			// vouching for a key nobody can identify.
			return "⚠ claims signed, no signer"
		}
		return "✓ signed " + signer
	case store.TrustHost:
		return "· host account"
	case store.TrustIndexed:
		// This column has to show that the packaging instructions came from the
		// index (rule 3): which asset gets installed and what the command is
		// called were not written by the author, and that belongs among the
		// things you see at a glance in this table.
		return "⚠ packaging from index"
	case store.TrustUnverified:
		return "⚠ unsigned"
	case "":
		return "⚠ no trust level in manifest"
	default:
		return fmt.Sprintf("⚠ unknown level %q", trust)
	}
}

// ---------------------------------------------------------------- installed index

// qryInstalledIndex flattens the install manifests into a lookup table, so "is this
// package installed" has an answer.
//
// An index entry's id is "Derrity/tinyserve" while its source is
// "github:Derrity/tinyserve", and a manifest's id is the latter — the two ends do not
// line up, so each manifest is registered under three keys. All lowercase: owner and
// repo are case-insensitive on GitHub and GitLab anyway.
func qryInstalledIndex(ms []store.Manifest) map[string]store.Manifest {
	out := make(map[string]store.Manifest, len(ms)*3)
	add := func(k string, m store.Manifest) {
		k = strings.ToLower(strings.TrimSpace(k))
		// First one wins: with the same owner/repo installed from two different
		// hosts, this loose key can only point at one of them. It is used solely
		// to display "installed v1.2.3", and the two host-qualified keys are more
		// specific and registered first, so this is the only genuinely ambiguous one.
		if k == "" || k == ":" {
			return
		}
		if _, ok := out[k]; !ok {
			out[k] = m
		}
	}
	for _, m := range ms {
		add(m.ID, m)
		if m.Owner != "" && m.Repo != "" {
			if m.Host != "" {
				add(m.Host+":"+m.Owner+"/"+m.Repo, m)
			}
			add(m.Owner+"/"+m.Repo, m)
		}
	}
	return out
}

// qryLookupInstalled tries the given keys in order, most specific first.
func qryLookupInstalled(idx map[string]store.Manifest, keys ...string) (store.Manifest, bool) {
	for _, k := range keys {
		if m, ok := idx[strings.ToLower(strings.TrimSpace(k))]; ok {
			return m, true
		}
	}
	return store.Manifest{}, false
}

// ---------------------------------------------------------------- toy list

func runList(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	ms, err := st.ListManifests()
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		ui.Info("Nothing installed yet.")
		ui.Info("`toy search <query>` looks through the indexes. And if you already know")
		ui.Info("where the repository is, `toy install github:owner/repo` skips them entirely.")
		return nil
	}

	// Trust goes in the first column: it is the one thing in this table that must
	// not be squeezed out, and in the first column there is no such thing as being
	// pushed off screen by the cell to its left (iron law 8).
	rows := [][]string{{"TRUST", "NAME", "VERSION", "SOURCE"}}
	for _, m := range ms {
		name := m.Name
		if name == "" {
			name = m.Repo
		}
		version := m.Version
		if version == "" {
			version = m.Tag
		}
		if version == "" {
			// The plain-URL route has no version to speak of (resolve is very
			// clear about that), and inventing one would be a lie.
			version = "—"
		}
		source := m.ID
		if source == "" {
			source = m.Source
		}
		rows = append(rows, []string{qryTrustLabel(m.Trust, m.Signer), name, version, source})
	}
	// The trust cell has to fit a full fingerprint, so its cap is wider than the rest.
	qryTable(rows, 48, qryCellWidth, 16, 0)
	ui.Info("")
	ui.Info("%d installed. `toy info <ref>` for details, `toy update` to upgrade.", len(ms))
	return nil
}

// ---------------------------------------------------------------- toy search

// qrySearchHit is one result in the --json output. Deliberately not index.Entry
// itself: a field added to the index later must not automatically appear in an
// output format we have promised.
type qrySearchHit struct {
	ID     string   `json:"id"`
	Source string   `json:"source"`
	Desc   string   `json:"desc,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Index  string   `json:"index"`
	Score  int      `json:"score"`
	// HasKey only says the index entry carries a well-formed ed25519 public key. IT
	// IS NOT "SIGNED": search does not have that version's toy.toml (getting it would
	// take one network request per result), so it cannot verify a single signature.
	// Copying somebody else's public key costs nothing, and the entry's sig may even
	// be empty — calling this signed would hand that word free of charge to anyone
	// who can edit an index (spec §5, iron law 8). The only route that really
	// verifies a signature is install / info.
	HasKey           bool   `json:"has_key"`
	KeyFingerprint   string `json:"key_fingerprint,omitempty"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
}

func runSearch(ctx context.Context, args []string) error {
	fs := qryFlags("search")
	limit := fs.Int("limit", qrySearchLimit, "how many results to show")
	asJSON := fs.Bool("json", false, "machine-readable output on stdout")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errUsage
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit has to be positive, got %d", *limit)
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	// No index configured at all is THE DEFAULT STATE, not a failure (see
	// store.DefaultIndexURL). Carrying on would print "no package in the indexes
	// matches X" — which is false, because we did not look anywhere (iron law 8).
	if len(cfg.Indexes) == 0 {
		qryNoIndexHelp()
		if *asJSON {
			return qrySearchJSON(query, nil)
		}
		return nil
	}
	idxs, err := loadIndexes(ctx, st, cfg)
	if err != nil {
		return err
	}
	// Whether something is installed is local information, and failing to read it
	// must not fail the whole search — that column is simply missing then.
	ms, err := st.ListManifests()
	if err != nil {
		ui.Warn("cannot read the install manifests (%v), so the \"installed\" column is missing this time", err)
	}
	installed := qryInstalledIndex(ms)

	results := index.Search(idxs, query, *limit)
	hits := make([]qrySearchHit, 0, len(results))
	for _, r := range results {
		h := qrySearchHit{
			ID:     r.Entry.ID,
			Source: r.Entry.Source,
			Desc:   r.Entry.Desc,
			Tags:   r.Entry.Tags,
			Index:  r.Index,
			Score:  r.Score,
		}
		// When the key in the index is not a valid ed25519 public key it must
		// never be rendered as a normal-looking fingerprint (iron law 8) —
		// rsvFingerprint returns the empty string for anything it cannot read.
		if fp := rsvFingerprint(r.Entry.Key); fp != "" {
			h.HasKey, h.KeyFingerprint = true, fp
		}
		if m, ok := qryLookupInstalled(installed, r.Entry.Source, r.Entry.ID); ok {
			h.Installed = true
			h.InstalledVersion = m.Version
		}
		hits = append(hits, h)
	}

	if *asJSON {
		return qrySearchJSON(query, hits)
	}

	if len(hits) == 0 {
		// Not found does not mean uninstallable: an index is only a directory and
		// the host is the truth (spec §5 — that is what "delisting is harmless"
		// means). A bare "not found" leaves people thinking the tool cannot be
		// installed, when it is most likely sitting on GitHub waiting for them.
		ui.Info("No package in the indexes matches %q.", query)
		ui.Info("")
		ui.Info("An index is only a directory; the host is the source of truth. If you know where it")
		ui.Info("lives, install it now — there is no need to wait for anyone to list it:")
		ui.Info("    toy install github:owner/repo")
		ui.Info("(Use a gitlab: / codeberg: prefix for other hosts, or `toy index add` someone else's index.)")
		return nil
	}

	rows := [][]string{{"PACKAGE", "STATUS", "INDEX", "DESCRIPTION"}}
	for _, h := range hits {
		state := "—"
		if h.Installed {
			state = "installed"
			if h.InstalledVersion != "" {
				state += " " + h.InstalledVersion
			}
		}
		rows = append(rows, []string{h.ID, state, h.Index, qryTrunc(h.Desc, qryDescWidth)})
	}
	qryTable(rows, 48, 20, qryCellWidth, 0)
	ui.Info("")
	if len(hits) == *limit {
		ui.Info("Showing %d (raise it with --limit). `toy info <package>` for details.", len(hits))
	} else {
		ui.Info("%s. `toy info <package>` for details, `toy install <package>` to install.", qryPlural(len(hits), "result", "results"))
	}
	return nil
}

// qrySearchJSON is the --json output. Spec §9: machine-readable goes to stdout,
// everything else to stderr. With no results it still emits a valid empty result —
// do not leave a script guessing whether empty stdout means a crash.
func qrySearchJSON(query string, hits []qrySearchHit) error {
	if hits == nil {
		hits = []qrySearchHit{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Query   string         `json:"query"`
		Count   int            `json:"count"`
		Results []qrySearchHit `json:"results"`
	}{query, len(hits), hits})
}

// qryNoIndexHelp explains what "no index configured" means and gives commands that
// can be copied as they stand.
//
// This is a legal state, not a fault: the built-in index is currently empty
// (store.DefaultIndexURL), and `toy install github:owner/repo` never goes through an
// index from end to end. So this only covers "how to add one" and "you can install
// without one", and prints not a single ✗.
func qryNoIndexHelp() {
	ui.Info("No index is configured, so there is nothing to search. This is the default, not a fault.")
	ui.Info("")
	ui.Info("Add one (the name is local to you, pick anything):")
	ui.Info("    toy index add local file:///absolute/path/toy-index.json   # a local file")
	ui.Info("    toy index add local ./index/toy-index.json                # a bare path works too, it becomes file://")
	ui.Info("    toy index add community https://example.com/index.json")
	ui.Info("")
	ui.Info("An index is only a directory; the host is the source of truth. If you know where the")
	ui.Info("repository is, you never needed an index in the first place:")
	ui.Info("    toy install github:owner/repo")
}

// ---------------------------------------------------------------- toy info

func runInfo(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	r, err := ref.Parse(args[0])
	if err != nil {
		// `toy list` and `toy uninstall` both accept the short installed name
		// (tinyserve) while info only accepts a ref — so the name you just read in
		// the list reports "missing slash" in this one subcommand, and the same
		// string means two different things inside the same CLI. For an installed
		// package we already hold its id; there is no reason to make the user spell
		// it out again. If it is not installed, hand back the original parse error:
		// at that point it really is a malformed ref, not an installed package.
		m, ferr := st.FindByName(args[0])
		if ferr != nil {
			return err
		}
		r2, perr := ref.Parse(m.ID)
		if perr != nil {
			return err
		}
		r = r2
	}
	res, err := resolve(ctx, st, cfg, r)
	if err != nil {
		return err
	}
	tf := res.Toyfile

	rows := [][]string{{"name", tf.Name}}
	add := func(label, value string) { rows = append(rows, []string{label, value}) }

	add("reference", res.Ref.String())
	if tf.Desc != "" {
		add("description", tf.Desc)
	}
	if res.Entry != nil && res.Entry.Desc != "" && res.Entry.Desc != tf.Desc {
		// The index's description and the author's own may differ, so show both —
		// and be explicit about which is which.
		add("index description", res.Entry.Desc)
	}
	add("license", qryOr(tf.License, "not declared by the author"))
	if tf.Homepage != "" {
		add("homepage", tf.Homepage)
	} else if web := res.Host.WebURL(res.Owner, res.Repo); web != "" {
		add("repository", web)
	}

	if res.Tag != "" {
		add("latest version", res.Tag)
	} else {
		// The plain-URL route has no tags to choose from at all, and inventing a
		// version number would be a lie (iron law 8).
		add("version", "unknown — a plain URL carries no version; you get whatever that file is right now")
	}

	// Comparing against one exact key would report "installable after all" cases
	// (Rosetta, musl falling back to a generic build) as "no matching build", so this
	// walks the candidate order exactly as install does.
	cands := platform.Candidates()
	// What gets reported to a human is the two-segment base, not cands[0] — on linux
	// that one is the linux/amd64/gnu toy synthesizes itself, and printing it as
	// "your platform" would send people looking for an asset that does not exist.
	cur, matched := platform.Base(), cands[0]
	if tf.IsScript() {
		add("kind", "single-file script: download + chmod +x, nothing unpacked, nothing built")
		add("platforms", "platform-independent (whether it runs is up to the script)")
	} else {
		add("kind", "release binary: download + unpack, not one line of script is run")
		plats := tf.Platforms()
		add("platforms", strings.Join(plats, "  "))
		// "Is this really a fallback" has exactly one test, and it is the same one
		// that decides the line install prints. info and install disagreeing about
		// the same package would be worse than both of them being wrong.
		switch hit := qryMatchPlatform(plats, cands); {
		case hit == "":
			add("this machine", cur+"  ✗ no matching build, cannot install")
			add("", "tried "+strings.Join(cands, ", ")+
				". Open an issue upstream asking the author to publish a "+cur+" asset and add one line for it in toy.toml")
		default:
			matched = hit // picking the sha256 below needs the key actually matched
			// Installable, but not the build made for this machine. Reuse install's
			// wording for the reason rather than writing a second version here: cur
			// and hit are the same string when the author split by libc, and a
			// "falling back to: linux/amd64" line would then be pointing at itself.
			if note := insFallbackNote(cands, hit, plats); note != "" {
				add("this machine", cur+"  ✓ installable, but not the build made for this machine")
				add("", note)
			} else {
				add("this machine", cur+"  ✓ has a matching build")
			}
		}
	}

	// The names that will appear on PATH after installing — the most concrete cell on
	// the screen you are supposed to read before installing.
	declared, what := tf.Bin, "path in the package"
	if tf.IsScript() {
		declared, what = tf.Script, "release asset"
	}
	label := "commands"
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		add(label, fmt.Sprintf("%s  →  %s: %s", name, what, declared[name]))
		label = ""
	}

	switch {
	case tf.IsScript():
		add("integrity", "scripts declare no per-platform sha256; the hash is pinned on first install (TOFU) and a later change is refused")
	case tf.SHA256[matched] != "":
		add("integrity", "this platform declares a sha256; it is checked on install and a mismatch is always refused")
	case len(tf.SHA256) > 0:
		add("integrity", fmt.Sprintf("the author gave a sha256 for %d platform(s), not this one — so the hash gets pinned on first install (TOFU)", len(tf.SHA256)))
	default:
		add("integrity", "the author declared no sha256; the hash is pinned on first install (TOFU) and a later change is refused")
	}

	ms, err := st.ListManifests()
	if err != nil {
		ui.Warn("cannot read the install manifests (%v), so this cannot say whether it is installed", err)
	}
	keys := []string{res.Ref.String(), res.Ref.ID()}
	if res.HostName != "" && res.Owner != "" && res.Repo != "" {
		keys = append([]string{res.HostName + ":" + res.Owner + "/" + res.Repo}, keys...)
	}
	if m, ok := qryLookupInstalled(qryInstalledIndex(ms), keys...); ok {
		add("installed", qryOr(m.Version, m.Tag)+" ("+qryTrustLabel(m.Trust, m.Signer)+")")
	} else {
		add("installed", "no")
	}

	qryTable(rows, 16, 0)
	ui.Info("")
	// The trust level goes through ui.Trust, so it is worded exactly like the line
	// printed after an install (spec §4, iron law 8).
	ui.Trust(res.Trust, insTrustDetail(res))
	return nil
}

// qryMatchPlatform returns the first candidate that is a declared platform key, or
// the empty string when none is. This MUST pick in the same order as
// toyfile.AssetFor — diverge and info says "installable" while install refuses, or
// the other way round.
func qryMatchPlatform(declared, candidates []string) string {
	for _, c := range candidates {
		if slices.Contains(declared, c) {
			return c
		}
	}
	return ""
}

func qryOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// qryHostLabel is the human-readable source, of the form "github.com/Derrity".
func qryHostLabel(res *resolved) string {
	if u, err := url.Parse(res.Host.WebURL(res.Owner, res.Repo)); err == nil && u.Hostname() != "" {
		return u.Hostname() + "/" + res.Owner
	}
	if res.HostName != "" {
		return res.HostName + "/" + res.Owner
	}
	return res.Owner
}

// ---------------------------------------------------------------- toy index

func runIndex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return errUsage
		}
		return qryIndexAdd(st, cfg, args[1], args[2])
	case "rm", "remove":
		if len(args) != 2 {
			return errUsage
		}
		return qryIndexRm(st, cfg, args[1])
	case "list", "ls":
		if len(args) != 1 {
			return errUsage
		}
		return qryIndexList(st, cfg)
	default:
		return fmt.Errorf("no such subcommand %q — there is only add / rm / list", args[0])
	}
}

func qryIndexAdd(st *store.Store, cfg store.Config, name, rawURL string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("an index needs a name: `toy index add <name> <url>`. The name is local to you, pick anything")
	}
	if qryCell(name) != name {
		return fmt.Errorf("the index name %q contains control characters — it gets printed to your terminal as-is, so pick another one", name)
	}
	u, err := qryIndexURL(rawURL)
	if err != nil {
		return err
	}
	for _, spec := range cfg.Indexes {
		if strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			return fmt.Errorf("there is already an index called %q (%s). Run `toy index rm %s` first, then add it again",
				spec.Name, spec.URL, name)
		}
		if spec.URL == u {
			// Allowed, but say so: the same URL under two names gets fetched
			// twice, and the second copy's results are deduplicated away, which
			// looks exactly like "I added it and nothing happened".
			ui.Warn("index %q already points at this URL; the same index under two names just gets fetched twice", spec.Name)
		}
	}
	cfg.Indexes = append(cfg.Indexes, store.IndexRef{Name: name, URL: u})
	if err := st.SaveConfig(cfg); err != nil {
		return err
	}
	ui.Info("added: %s -> %s", name, u)
	ui.Info("`toy doctor` checks that it is reachable; `toy search <query>` starts searching right away.")
	return nil
}

func qryIndexRm(st *store.Store, cfg store.Config, name string) error {
	name = strings.TrimSpace(name)
	for i, spec := range cfg.Indexes {
		if !strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			continue
		}
		cfg.Indexes = slices.Delete(cfg.Indexes, i, i+1)
		if err := st.SaveConfig(cfg); err != nil {
			return err
		}
		ui.Info("removed: %s -> %s", spec.Name, spec.URL)
		// The cache is left in place: it is only read as an offline fallback, and
		// by then this index is no longer in the config, so nothing reads it.
		// To clear it for real, delete the matching file under ~/.toy/index/.
		if len(cfg.Indexes) == 0 {
			ui.Warn("no indexes are left, so `toy search` will find nothing. " +
				"`toy install github:owner/repo` does not go through an index and still works")
		}
		return nil
	}
	if len(cfg.Indexes) == 0 {
		return fmt.Errorf("no index called %q — there are no indexes configured at all", name)
	}
	return fmt.Errorf("no index called %q. The configured ones are: %s", name, strings.Join(qryIndexNames(cfg), ", "))
}

func qryIndexNames(cfg store.Config) []string {
	out := make([]string, 0, len(cfg.Indexes))
	for _, spec := range cfg.Indexes {
		out = append(out, spec.Name)
	}
	return out
}

func qryIndexList(st *store.Store, cfg store.Config) error {
	if len(cfg.Indexes) == 0 {
		qryNoIndexHelp()
		ui.Info("")
		ui.Info("The config file is %s.", filepath.Join(st.Root, "config.toml"))
		return nil
	}
	// The package count and the cache time both come from the local cache: listing
	// indexes has no business going online — that is `toy doctor`'s job.
	rows := [][]string{{"NAME", "PACKAGES", "LAST CACHED", "URL"}}
	for _, spec := range cfg.Indexes {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.URL
		}
		count, when := qryIndexCacheInfo(st, name)
		rows = append(rows, []string{name, count, when, spec.URL})
	}
	qryTable(rows)
	ui.Info("")
	ui.Info("Counts and times come from the local cache (%s), not from the remote as it is now; `toy search` re-fetches.", st.IndexDir())
	ui.Info("A \"—\" means that index has never been fetched. `toy doctor` probes each one for reachability.")
	return nil
}

// qryIndexCacheInfo reads the local cache and reports the package count and cache
// time. Never cached means saying so plainly.
func qryIndexCacheInfo(st *store.Store, name string) (count, when string) {
	path := rsvIndexCache(st, name)
	fi, err := os.Stat(path)
	if err != nil {
		// A "—" rather than a phrase: tabwriter aligns by rune count, and a cell
		// mixing wide and narrow characters knocks the following column out of
		// alignment (see the comment on qryTable). What this cell means is
		// explained by the sentence printed under the table.
		return "—", "—"
	}
	when = fi.ModTime().Local().Format("2006-01-02 15:04")
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable", when
	}
	idx, _, err := index.Load(data, name)
	if err != nil {
		return "broken", when
	}
	return fmt.Sprint(len(idx.Packages)), when
}

// qryIndexURL normalizes the index location the user typed.
// A bare path is treated as a local file: spec §5 says an index source "is just a URL
// as far as the implementation is concerned", and a v1 index is usually a local json
// — making people hand-write file:///absolute/path only drives them away.
func qryIndexURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("the index URL is empty (give an https://… URL, or the path to a local json file)")
	}
	if !strings.Contains(s, "://") {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("%q is not a URL, and as a local path it cannot be made absolute either: %w", s, err)
		}
		s = "file://" + filepath.ToSlash(abs)
	}
	r, err := ref.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable index location (it has to be https://… or file:///absolute/path): %w", s, err)
	}
	// Given a "://", ref.Parse can only return KindURL or an error; this just writes
	// that assumption down.
	if r.Kind != ref.KindURL {
		return "", fmt.Errorf("%q is not a location — an index has to be https://… or file:///absolute/path", s)
	}
	return r.URL, nil
}

// ---------------------------------------------------------------- toy doctor

// qryCheck is one line of the check-up. Anything wrong MUST come with the exact
// command that fixes it — a report that names problems without offering a way out
// just hands the problem straight back to the user.
type qryCheck struct {
	ok    bool
	title string
	note  string
	fix   string
}

func runDoctor(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	var checks []qryCheck
	checks = append(checks, qryCheckPath(st))
	checks = append(checks,
		qryCheckDir("bin directory", st.BinDir(), false),
		qryCheckDir("pkg directory", filepath.Join(st.Root, "pkg"), false),
		qryCheckDir("keys directory", st.KeysDir(), true),
	)
	checks = append(checks, qryCheckGit(st.BinDir()))
	checks = append(checks, qryCheckIndexes(ctx, cfg)...)
	checks = append(checks, qryCheckLinks(st))
	checks = append(checks, qryCheckManifests(st)...)
	checks = append(checks, qryCheckCache(st))

	ui.Info("Checking %s", st.Root)
	ui.Info("")
	qryPlatformInfo()
	bad := 0
	for _, c := range checks {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			bad++
		}
		ui.Info("%s %s", mark, c.title)
		if c.note != "" {
			ui.Info("    %s", c.note)
		}
		if !c.ok && c.fix != "" {
			ui.Info("    fix: %s", c.fix)
		}
	}
	ui.Info("")
	if bad == 0 {
		ui.Info("All %d checks passed.", len(checks))
		return nil
	}
	// Returning an error means exit code 1 (see main), and the message itself has
	// to read like a sentence too.
	return fmt.Errorf("%d of %d checks failed — each one above prints the command that fixes it", bad, len(checks))
}

// qryPlatformInfo prints which builds this machine will ask for.
//
// It is NOT a check, so it prints no ✓/✗ — "this machine uses musl" is neither right
// nor wrong, and a red cross next to it would send people off fixing something that
// is not broken. It exists because when the installed thing will not start (a glibc
// binary on musl, Rosetta not installed, armv7 on an armv6 board), not one word of
// the error points at the platform, and these lines are the only clue that matches.
func qryPlatformInfo() {
	ui.Info("platform %s", platform.Current())
	ui.Info("    tried in this order: %s", strings.Join(platform.Candidates(), " -> "))
	if lvl := platform.CPULevelHint(); lvl != "" {
		// A hint only: toy does no microarchitecture matching (that means
		// parsing CPUID, which the standard library does not offer, and pulling
		// in a library would violate spec §10).
		ui.Info("    CPU is roughly %s (a hint only, it plays no part in picking a build)", lvl)
	}
	ui.Info("")
}

// qryCheckPath covers the first hole a new user falls into: it installed, and the
// command is still "not found".
func qryCheckPath(st *store.Store) qryCheck {
	bin := st.BinDir()
	if st.InPath() {
		return qryCheck{ok: true, title: bin + " is on PATH"}
	}
	return qryCheck{
		title: bin + " is not on PATH — installed commands will not be found",
		note:  "toy puts every symlink in this one directory; if it is not on PATH, nothing you install can run",
		fix:   fmt.Sprintf(`add this line to ~/.bashrc or ~/.zshrc, then open a new terminal: export PATH="%s:$PATH"`, bin),
	}
}

// qryCheckDir checks that a directory exists and has the right mode.
// strict is the keys/ tier: there is a private key in there and nobody else gets to
// read any of it.
func qryCheckDir(label, dir string, strict bool) qryCheck {
	fi, err := os.Stat(dir)
	if err != nil {
		return qryCheck{
			title: label + " " + dir + " is unusable",
			note:  err.Error(),
			fix:   "mkdir -p " + dir,
		}
	}
	if !fi.IsDir() {
		return qryCheck{
			title: label + " " + dir + " is not a directory",
			fix:   "move it aside and run toy again: mv " + dir + " " + dir + ".bak",
		}
	}
	perm := fi.Mode().Perm()
	if perm&0o700 != 0o700 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — you cannot fully read and write it yourself", label, dir, perm),
			fix:   "chmod u+rwx " + dir,
		}
	}
	if strict && perm&0o077 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — other users can read the private key in it", label, dir, perm),
			fix:   "chmod 700 " + dir,
		}
	}
	if !strict && perm&0o022 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — other users can write into it", label, dir, perm),
			note:  "anyone who can write here can replace a command on your PATH with one of their own",
			fix:   "chmod go-w " + dir,
		}
	}
	return qryCheck{ok: true, title: fmt.Sprintf("%s %s mode %#o", label, dir, perm)}
}

// qryCheckGit: no git means no version discovery, which makes the whole
// github:/gitlab: route dead (spec §2).
//
// It goes through autGitPath rather than exec.LookPath: the one autGitPath picks is
// the one toy will actually execute, and the check-up has to report that same one.
// With LookPath, a package that has taken the name git would make doctor point at
// ~/.toy/bin/git and say "git found at …" — a statement that is true and that
// reports an attack as good health (iron law 8).
func qryCheckGit(binDir string) qryCheck {
	path, err := autGitPath(binDir)
	if err == nil {
		return qryCheck{ok: true, title: "git found at " + path}
	}
	return qryCheck{
		title: "no usable git",
		note:  "toy discovers versions with `git ls-remote --tags`; without it no github:/gitlab: package can be installed. " + ui.SafeURL(err.Error()),
		fix:   "Debian/Ubuntu: sudo apt install git — macOS: xcode-select --install",
	}
}

func qryCheckIndexes(ctx context.Context, cfg store.Config) []qryCheck {
	if len(cfg.Indexes) == 0 {
		// Nothing is wrong with this machine: the built-in index is currently
		// empty (store.DefaultIndexURL), and reporting a ✗ would show every new
		// user a red cross on their first check-up and leave them thinking they
		// broke the install. State it neutrally and still give the way forward
		// (iron law 8: say it plainly, but do not pin it on the innocent).
		return []qryCheck{{
			ok:    true,
			title: "no index configured (`toy search` has nothing to search yet)",
			note: "this is the default and it does not affect `toy install github:owner/repo` — that route never goes through an index. " +
				"To start searching, run `toy index add <name> <url>` (the url can be https://… or the path to a local json file)",
		}}
	}
	out := make([]qryCheck, 0, len(cfg.Indexes))
	for _, spec := range cfg.Indexes {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.URL
		}
		if strings.TrimSpace(spec.URL) == "" {
			out = append(out, qryCheck{
				title: "index " + name + " has no url",
				fix:   "toy index rm " + name + ", then add it again",
			})
			continue
		}
		probe, cancel := context.WithTimeout(ctx, qryIndexProbeTimeout)
		data, err := fetch.GetBytes(probe, spec.URL, fetch.Options{Name: name})
		cancel()
		if err != nil {
			out = append(out, qryCheck{
				title: "index " + name + " cannot be fetched",
				note:  ui.SafeURL(err.Error()),
				fix:   "check the url (" + ui.SafeURL(spec.URL) + "), or drop it with `toy index rm " + name + "`",
			})
			continue
		}
		idx, skipped, err := index.Load(data, name)
		if err != nil {
			out = append(out, qryCheck{
				title: "index " + name + " was fetched but cannot be used",
				note:  err.Error(),
				fix:   "point it at a different url, or `toy index rm " + name + "`",
			})
			continue
		}
		c := qryCheck{ok: true, title: fmt.Sprintf("index %s reachable, %d packages", name, len(idx.Packages))}
		if skipped > 0 {
			// The index's problem, not this machine's — not a ✗, but not
			// something to leave unsaid either.
			c.note = fmt.Sprintf("%d record(s) in it are invalid and were skipped — those packages will not turn up in a search until the index maintainer fixes them", skipped)
		}
		out = append(out, c)
	}
	return out
}

// qryCheckLinks finds symlinks in bin/ pointing at things that are gone.
// On PATH such a link looks like a command; typing it only ever says "no such file".
func qryCheckLinks(st *store.Store) qryCheck {
	bin := st.BinDir()
	entries, err := os.ReadDir(bin)
	if err != nil {
		return qryCheck{title: "cannot read " + bin, note: err.Error(), fix: "mkdir -p " + bin}
	}
	var dangling []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		p := filepath.Join(bin, e.Name())
		if _, err := os.Stat(p); err == nil { // Stat follows symlinks; if it resolves, the link is fine
			continue
		}
		target, err := os.Readlink(p)
		if err != nil {
			target = "(target unreadable)"
		}
		dangling = append(dangling, e.Name()+" → "+target)
	}
	if len(dangling) == 0 {
		return qryCheck{ok: true, title: fmt.Sprintf("%d entries in bin, no dangling symlinks", len(entries))}
	}
	return qryCheck{
		title: fmt.Sprintf("%d dangling symlink(s) — they point at files that are gone", len(dangling)),
		note:  strings.Join(dangling, "; "),
		fix:   "reinstall that package (toy install <ref>), or clear it out with `toy uninstall <name>`",
	}
}

// qryCheckManifests finds orphaned manifests: the manifest is still there, the pkg
// directory is gone. `toy list` lists such a package while there is nothing at all
// behind it.
func qryCheckManifests(st *store.Store) []qryCheck {
	ms, err := st.ListManifests()
	if err != nil {
		return []qryCheck{{
			title: "cannot read the install manifests",
			note:  err.Error(),
			fix:   "delete the broken manifest at the path in the error above (after that, clean up its package by hand under ~/.toy/pkg)",
		}}
	}
	var orphans []string
	for _, m := range ms {
		if m.PkgDir == "" { // the single-file script shape may have no pkg directory at all; do not accuse it
			continue
		}
		if _, err := os.Stat(m.PkgDir); err != nil {
			orphans = append(orphans, m.ID+" ("+m.PkgDir+")")
		}
	}
	if len(orphans) == 0 {
		return []qryCheck{{ok: true, title: fmt.Sprintf("%d install manifest(s), every one matches a package directory on disk", len(ms))}}
	}
	return []qryCheck{{
		title: fmt.Sprintf("%d package(s) still have a manifest but no files", len(orphans)),
		note:  strings.Join(orphans, "; "),
		fix:   "reinstall it (toy install <the id above>) or remove it (toy uninstall <name>)",
	}}
}

// qryPlural is the smallest possible pluralization: writing "1 result(s)" makes the
// whole CLI look half-finished, and this line signs off the search results, which is
// what users read most.

func qryPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// qryCheckCache reports what the download cache adds up to.
//
// Never a ✗: a large cache is not a fault, and uninstall keeps those bytes on
// purpose (they are content-addressed and they are what `toy serve` mirrors,
// spec §8). But it only ever grows, and a number nobody can see is a number
// nobody reclaims -- so doctor states it, and points at `toy prune` once there is
// something worth reclaiming.
func qryCheckCache(st *store.Store) qryCheck {
	es, err := st.CacheEntries()
	if err != nil {
		return qryCheck{ok: false, title: "cannot read the download cache", note: err.Error()}
	}
	var total, reclaimable int64
	var n, rn int
	for _, e := range es {
		total, n = total+e.Bytes, n+1
		if !e.InUse {
			reclaimable, rn = reclaimable+e.Bytes, rn+1
		}
	}
	c := qryCheck{ok: true, title: fmt.Sprintf("download cache: %s in %s", prnSize(total), prnCount(n, "file"))}
	if rn > 0 {
		c.note = fmt.Sprintf("%s of that is no longer used by anything installed. `toy prune` reclaims it "+
			"(those bytes are also what `toy serve` hands out, so pruning leaves it less to mirror)", prnSize(reclaimable))
	}
	return c
}
