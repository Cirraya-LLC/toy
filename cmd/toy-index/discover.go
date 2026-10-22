package main

// discover: find repositories on GitHub worth listing, and write them as a repo list
// that gen reads.
//
// It searches by topic, sorted by stars, and keeps a repository only when gen would
// produce an entry for it -- the same genOne, run without writing anything -- so the
// cap on one batch is spent on repositories that actually ship something toy can
// install, not on the libraries and Python packages that share their topics.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// defaultTopics are where command-line tools tag themselves. Each becomes one search.
// "terminal" is left out on purpose: it is mostly terminal emulators, which are GUI apps.
var defaultTopics = []string{"cli", "command-line", "command-line-tool", "tui", "cli-app"}

type searchItem struct {
	FullName  string    `json:"full_name"`
	Desc      string    `json:"description"`
	Stars     int       `json:"stargazers_count"`
	PushedAt  time.Time `json:"pushed_at"`
	CreatedAt time.Time `json:"created_at"`
	License   *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
}

type discoverConfig struct {
	topics   []string
	curated  []string // curated lists (owner/repo of an awesome-list) whose links are candidates too
	minStars int
	since    time.Time // last push no older than this
	born     time.Time // created no later than this: a project needs a track record first
	max      int       // stop after this many accepted repos
	idx      *index.Index
	lists    lists
	gh       *ghClient
	host     host.Host
	out      string // repo list for gen
	review   string // optional: repos whose licence a person has to read first
}

type pick struct {
	item    searchItem
	licence string
}

func runDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "")
	review := fs.String("review", "", "")
	topics := fs.String("topics", strings.Join(defaultTopics, ","), "")
	listsFlag := fs.String("lists", strings.Join(defaultLists, ","), "")
	minStars := fs.Int("min-stars", 1000, "")
	days := fs.Int("pushed-within", 730, "")
	minAge := fs.Int("min-age", 180, "")
	max := fs.Int("max", 50, "")
	idxPath := fs.String("index", filepath.Join("index", "toy-index.json"), "")
	licenses := fs.String("licenses", filepath.Join("index", "licenses.txt"), "")
	delisted := fs.String("delisted", filepath.Join("index", "delisted.txt"), "")
	declined := fs.String("declined", filepath.Join("index", "declined.txt"), "")
	token := fs.String("token", "", "")
	api := fs.String("api", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*out) == "" || *max < 1 {
		return errUsage
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	if tok == "" {
		// Unauthenticated search is 10 requests a minute and the core API 60 an hour;
		// one batch needs far more, and failing halfway helps nobody.
		return errors.New("discover needs a token: set $GITHUB_TOKEN (gh auth token works) or pass --token")
	}
	idx, err := loadIndex(*idxPath)
	if err != nil {
		return err
	}
	ls, err := loadLists(*licenses, *delisted, *declined)
	if err != nil {
		return err
	}
	gh, err := host.Lookup("github", nil)
	if err != nil {
		return err
	}
	var ts, lsrc []string
	for _, t := range strings.Split(*topics, ",") {
		if t = strings.TrimSpace(t); t != "" {
			ts = append(ts, t)
		}
	}
	for _, l := range strings.Split(*listsFlag, ",") {
		if l = strings.TrimSpace(l); l != "" {
			if _, err := listID(l); err != nil {
				return fmt.Errorf("--lists: %w", err)
			}
			lsrc = append(lsrc, l)
		}
	}
	return discover(ctx, discoverConfig{
		topics: ts, curated: lsrc, minStars: *minStars, since: time.Now().AddDate(0, 0, -*days),
		born: time.Now().AddDate(0, 0, -*minAge), max: *max,
		idx: idx, lists: ls, gh: newGitHub(*api, tok), host: gh, out: *out, review: *review,
	})
}

func discover(ctx context.Context, cfg discoverConfig) error {
	cands, err := searchAll(ctx, cfg)
	if err != nil {
		return err
	}

	// Repositories get renamed and GitHub redirects the old name (Peltoche/lsd is now
	// lsd-rs/lsd), so "already in the index" has to compare current names: a list links
	// to the new one while the index still says the old one, and comparing ids alone
	// would list the same tool twice.
	listed := map[string]bool{}
	ids := make([]string, 0, len(cfg.idx.Packages))
	for _, e := range cfg.idx.Packages {
		listed[foldRepo(e.ID)] = true
		ids = append(ids, e.ID)
	}
	if len(cfg.curated) > 0 {
		current, err := cfg.gh.repoMeta(ctx, ids)
		if err != nil {
			return fmt.Errorf("looking up the current names of the index's repositories: %w", err)
		}
		for _, it := range current {
			listed[foldRepo(it.FullName)] = true
		}
	}

	why := map[string]int{}
	var picked, toReview []pick
	for _, it := range cands {
		if len(picked) >= cfg.max {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		id := it.FullName
		if listed[foldRepo(id)] {
			why["already in the index"]++
			continue
		}
		if cfg.lists.delisted[foldRepo(id)] {
			why["delisted at its maintainer's request"]++
			continue
		}
		if cfg.lists.declined[foldRepo(id)] {
			why["declined by a reviewer"]++
			continue
		}
		// The search result already carries the licence, so most refusals cost no call.
		spdx, st := licenceOf(it.License)
		lic, err := decideLicence(id, spdx, st, cfg.lists.reviewed)
		switch {
		case errors.Is(err, errUnrecognized):
			why["licence GitHub cannot identify (listed for a person to read)"]++
			toReview = append(toReview, pick{item: it})
			continue
		case errors.Is(err, errNoLicence):
			why["no licence (all rights reserved)"]++
			continue
		case err != nil:
			why["licence not open source"]++
			continue
		}
		// Exactly what gen will do with it, minus the probe and minus writing.
		r := genOne(ctx, genConfig{prov: cfg.gh, host: cfg.host, lists: cfg.lists}, &index.Index{Version: index.Version}, id)
		switch {
		case errors.Is(r.err, errRateLimited):
			return r.err
		case errors.Is(r.err, errNoRelease):
			why["never published a release"]++
			continue
		case r.err != nil:
			why["its release has nothing toy can install"]++
			continue
		case r.skip != "":
			why[r.skip]++
			continue
		}
		picked = append(picked, pick{item: it, licence: lic})
	}

	if err := writePicks(cfg.out, picked, "accepted by toy-index discover; feed to `toy-index gen --probe`"); err != nil {
		return err
	}
	if cfg.review != "" {
		if err := writePicks(cfg.review, toReview, "licence files GitHub cannot identify: read each, then add `owner/repo SPDX-id` to index/licenses.txt"); err != nil {
			return err
		}
	}
	discoverReport(cfg, len(cands), picked, toReview, why)
	return nil
}

// searchAll runs one search per topic and returns every distinct repository, most
// stars first. GitHub stops a search at 1000 results; with a star floor no topic gets
// near that.
func searchAll(ctx context.Context, cfg discoverConfig) ([]searchItem, error) {
	seen := map[string]bool{}
	var all []searchItem
	for _, topic := range cfg.topics {
		// created:<= is the track-record rule. A tool people install from this index should
		// have existed in public for a while: in the first run, half the AI agents near the
		// top were a few months old with star counts nobody could vouch for. Too new is not
		// "no" -- they come back on their own once old enough.
		q := fmt.Sprintf("topic:%s stars:>=%d archived:false fork:false mirror:false pushed:>=%s created:<=%s",
			topic, cfg.minStars, cfg.since.Format("2006-01-02"), cfg.born.Format("2006-01-02"))
		for page := 1; page <= 10; page++ {
			total, items, err := cfg.gh.search(ctx, q, page)
			if err != nil {
				return nil, fmt.Errorf("searching topic:%s: %w", topic, err)
			}
			for _, it := range items {
				if k := foldRepo(it.FullName); !seen[k] {
					seen[k] = true
					all = append(all, it)
				}
			}
			if len(items) < 100 || page*100 >= total {
				break
			}
		}
	}
	// The curated lists: the same floors, applied here because no search applied them.
	for _, list := range cfg.curated {
		ids, err := cfg.gh.listRepos(ctx, list)
		if err != nil {
			return nil, err
		}
		var fresh []string
		for _, id := range ids {
			if !seen[foldRepo(id)] {
				fresh = append(fresh, id)
			}
		}
		items, err := cfg.gh.repoMeta(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("looking up the repositories %s links to: %w", list, err)
		}
		for _, it := range items {
			k := foldRepo(it.FullName)
			if seen[k] || it.Stars < cfg.minStars || it.CreatedAt.After(cfg.born) || it.PushedAt.Before(cfg.since) {
				continue
			}
			seen[k] = true
			all = append(all, it)
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].Stars > all[b].Stars })
	return all, nil
}

// search is one page of GitHub's repository search. The search API has its own quota,
// 30 requests a minute with a token; running dry waits for the window to roll over
// rather than failing a batch over a pause of a few seconds.
func (c *ghClient) search(ctx context.Context, q string, page int) (int, []searchItem, error) {
	path := "/search/repositories?" + url.Values{
		"q": {q}, "sort": {"stars"}, "order": {"desc"},
		"per_page": {"100"}, "page": {strconv.Itoa(page)},
	}.Encode()
	var res struct {
		Total int          `json:"total_count"`
		Items []searchItem `json:"items"`
	}
	for attempt := 0; ; attempt++ {
		err := c.get(ctx, path, &res)
		if err == nil {
			return res.Total, res.Items, nil
		}
		wait := time.Until(c.reset) + 2*time.Second
		if !errors.Is(err, errRateLimited) || attempt >= 3 || wait > 2*time.Minute {
			return 0, nil, err
		}
		ui.Info("search quota used up; waiting %s for it to reset", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func writePicks(path string, ps []pick, header string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", header)
	for _, p := range ps {
		desc := strings.Join(strings.Fields(tomlSafe(p.item.Desc, 80)), " ")
		lic := p.licence
		if lic == "" {
			lic = "licence unknown"
		}
		fmt.Fprintf(&b, "%s  # %d stars · %s · %s\n", p.item.FullName, p.item.Stars, lic, desc)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func discoverReport(cfg discoverConfig, seen int, picked, toReview []pick, why map[string]int) {
	ui.Info("")
	ui.Info("Searched %d topics and %d curated lists (stars >= %d, pushed since %s, created by %s): %d distinct repositories.",
		len(cfg.topics), len(cfg.curated), cfg.minStars, cfg.since.Format("2006-01-02"), cfg.born.Format("2006-01-02"), seen)
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return why[keys[a]] > why[keys[b]] })
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', tabwriter.AlignRight)
	for _, k := range keys {
		fmt.Fprintf(tw, "%d\t  %s\n", why[k], k)
	}
	tw.Flush()
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if l != "" {
			ui.Info("  %s", l)
		}
	}
	ui.Info("Accepted %d (cap %d) -> %s", len(picked), cfg.max, cfg.out)
	if len(toReview) > 0 && cfg.review != "" {
		ui.Info("%d need a person to read their licence -> %s", len(toReview), cfg.review)
	}
	if q := cfg.gh.Quota(); q != "" {
		ui.Info("%s", q)
	}
}
