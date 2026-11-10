package main

// dead: find index entries whose upstream has nothing left to install, and say which have
// been that way long enough to drop.
//
// An entry rots in one of two ways. Upstream renames an asset or drops a platform: the
// entry is wrong, and a person fixes it (regenerate, edit). Or upstream stops being a
// source of releases at all -- the repository is deleted or made private, or every
// release is gone. No edit to the entry can fix that one. bradygaster/squad failed every
// daily run for nine days (November 2026) before a person dropped it, and adding entries
// was already hands-off by then, so the daily run now does the dropping as well.
//
// "Dead" takes two witnesses that do not share a failure mode:
//
//   - index-check: the entry failed today and passed nowhere (--results)
//   - GitHub: the repository answers NOT_FOUND, or it has no releases at all
//
// and both have to hold on --days separate daily runs in a row (--state remembers). A
// repository half way through re-publishing its releases, a GitHub hiccup, a day the
// check itself broke: each resets the count, which is the safe direction.
//
// Nothing else is dropped here. A renamed asset, a release that went out without some of
// its builds (dua-cli v3.0.1, whose release workflow was cancelled before the macOS
// archives were uploaded), a network failure: each leaves a release in place, and a
// person decides what to do about it.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/ui"
)

// Counting releases is expensive on GitHub's side: fifty repositories to a query came
// back as a 502 (November 2026), and twenty did not. The daily run only asks about what
// failed that day, which is usually a handful.
const deadBatch = 20

var deadRetryWait = 5 * time.Second

type deadConfig struct {
	index   string
	results []string // index-check --results files; none means GitHub is the only witness
	state   string
	days    int
	apply   bool // remove the ready ones from the index
	report  string
	today   time.Time
}

// deadRecord is one line of the state file: how long an entry has looked dead.
type deadRecord struct {
	id          string
	first, last string // dates (UTC), YYYY-MM-DD
	runs        int    // daily runs in a row that found it dead
	why         string
}

func runDead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dead", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	idx := fs.String("index", filepath.Join("index", "toy-index.json"), "")
	results := fs.String("results", "", "")
	state := fs.String("state", "", "")
	days := fs.Int("days", 3, "")
	apply := fs.Bool("apply", false, "")
	report := fs.String("report", "", "")
	token := fs.String("token", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *state == "" || *days < 1 {
		return errUsage
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	cfg := deadConfig{index: *idx, state: *state, days: *days, apply: *apply, report: *report, today: time.Now().UTC()}
	if *results != "" {
		cfg.results = strings.Split(*results, ",")
	}
	return dead(ctx, cfg, newGitHub("", tok))
}

func dead(ctx context.Context, cfg deadConfig, gh *ghClient) error {
	idx, err := loadIndex(cfg.index)
	if err != nil {
		return err
	}
	failing, err := failedEverywhere(cfg.results)
	if err != nil {
		return err
	}

	// Who can be asked. A signed entry is never dropped by a machine: removing it erases
	// a signature, and that is a person's call however dead the repository looks.
	var ask []string
	for _, e := range idx.Packages {
		if failing != nil && !failing[foldRepo(e.ID)] {
			continue
		}
		repo, ok := strings.CutPrefix(e.Source, "github:")
		if !ok {
			continue // only GitHub can be asked; anything else stays a person's job
		}
		if e.Key != "" || e.Sig != "" {
			ui.Warn("%s is signed; if it is dead, a person has to drop it", e.ID)
			continue
		}
		if _, err := listID(repo); err != nil {
			continue
		}
		ask = append(ask, e.ID+"\t"+repo)
	}

	facts, err := gh.upstreamGone(ctx, ask)
	if err != nil {
		return err
	}

	old, err := readDeadState(cfg.state)
	if err != nil {
		return err
	}
	day := cfg.today.Format("2006-01-02")
	var now []deadRecord
	var ready []deadRecord
	for _, line := range ask {
		id, _, _ := strings.Cut(line, "\t")
		why, isDead := facts[id]
		if !isDead {
			continue
		}
		r := deadRecord{id: id, first: day, last: day, runs: 1, why: why}
		if p, ok := old[foldRepo(id)]; ok {
			r.first, r.runs = p.first, p.runs
			if p.last != day { // a second run on the same day is not a second day
				r.runs++
			}
		}
		now = append(now, r)
		if r.runs >= cfg.days {
			ready = append(ready, r)
		}
		ui.Info("%s: %s (%d of %d days)", id, why, min(r.runs, cfg.days), cfg.days)
	}
	if err := writeDeadState(cfg.state, now); err != nil {
		return err
	}
	if len(now) == 0 {
		ui.Info("no entry's upstream looks dead")
	}

	if cfg.apply && len(ready) > 0 {
		for _, r := range ready {
			idx.Remove(r.id)
			ui.Info("dropped %s", r.id)
		}
		if err := writeIndex(cfg.index, idx); err != nil {
			return err
		}
	}
	if cfg.report != "" && len(ready) > 0 {
		return os.WriteFile(cfg.report, []byte(deadReport(ready, cfg.days)), 0o644)
	}
	return nil
}

// failedEverywhere reads index-check results: the entries that failed somewhere and
// passed nowhere. nil (not empty) means no results were given, so every entry is asked.
func failedEverywhere(paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	failed, passed := map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		res, err := readResults(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		for id, v := range res {
			switch v.status {
			case "pass":
				passed[id] = true
			case "fail":
				failed[id] = true
			}
		}
	}
	for id := range passed {
		delete(failed, id)
	}
	return failed, nil
}

// upstreamGone asks GitHub about each "id<TAB>owner/repo", deadBatch to a GraphQL query,
// and returns why for the ones with nothing left to install. Only two answers count: a
// NOT_FOUND for that repository, or a release count of zero. Anything else -- an error of
// another type, a missing field -- is not evidence, and the entry is left alone today.
func (c *ghClient) upstreamGone(ctx context.Context, ask []string) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(ask); start += deadBatch {
		batch := ask[start:min(start+deadBatch, len(ask))]
		var q strings.Builder
		q.WriteString("query {")
		for i, line := range batch {
			_, repo, _ := strings.Cut(line, "\t")
			owner, name, _ := strings.Cut(repo, "/")
			// listID has already held these to [A-Za-z0-9._-]; they cannot leave the string.
			fmt.Fprintf(&q, ` r%d: repository(owner: %q, name: %q) { releases { totalCount } }`, i, owner, name)
		}
		q.WriteString(" }")
		var res struct {
			Data map[string]*struct {
				Releases *struct {
					TotalCount *int `json:"totalCount"`
				} `json:"releases"`
			} `json:"data"`
			Errors []struct {
				Type string `json:"type"`
				Path []any  `json:"path"`
			} `json:"errors"`
		}
		var err error
		for try := 0; try < 3; try++ {
			if try > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(try) * deadRetryWait):
				}
			}
			if err = c.graphql(ctx, q.String(), &res); err == nil {
				break
			}
		}
		if err != nil {
			return nil, err
		}
		notFound := map[string]bool{}
		for _, e := range res.Errors {
			if e.Type == "NOT_FOUND" && len(e.Path) == 1 {
				if alias, ok := e.Path[0].(string); ok {
					notFound[alias] = true
				}
			}
		}
		for i, line := range batch {
			id, repo, _ := strings.Cut(line, "\t")
			alias := "r" + strconv.Itoa(i)
			r, present := res.Data[alias]
			switch {
			case present && r == nil && notFound[alias]:
				out[id] = "github.com/" + repo + " does not exist any more (deleted, or made private)"
			case r != nil && r.Releases != nil && r.Releases.TotalCount != nil && *r.Releases.TotalCount == 0:
				out[id] = "github.com/" + repo + " has no releases left"
			}
		}
	}
	return out, nil
}

func readDeadState(path string) (map[string]deadRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]deadRecord{}, nil // the first run, or the cache was evicted: start counting
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]deadRecord{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 5)
		if len(p) != 5 {
			continue
		}
		n, err := strconv.Atoi(p[3])
		if err != nil || n < 1 {
			continue
		}
		out[foldRepo(p[0])] = deadRecord{id: p[0], first: p[1], last: p[2], runs: n, why: p[4]}
	}
	return out, sc.Err()
}

// writeDeadState keeps only what looked dead on this run: anything that did not is
// forgotten, so the count is always days in a row.
func writeDeadState(path string, recs []deadRecord) error {
	sort.Slice(recs, func(i, j int) bool { return foldRepo(recs[i].id) < foldRepo(recs[j].id) })
	var b strings.Builder
	for _, r := range recs {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\n", r.id, r.first, r.last, r.runs, strings.Join(strings.Fields(r.why), " "))
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func deadReport(ready []deadRecord, days int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The daily index-check found these entries failing, and GitHub confirms their upstream has "+
		"nothing left to install, on %d daily runs in a row:\n\n", days)
	b.WriteString("| Entry | Why | Since |\n| --- | --- | --- |\n")
	for _, r := range ready {
		fmt.Fprintf(&b, "| [%s](https://github.com/%s) | %s | %s |\n", r.id, r.id, mdCell(r.why), r.first)
	}
	b.WriteString("\nDropped with `toy-index drop`, not declined: if releases come back, discovery may propose " +
		"the entry again (an entry that was added by hand needs `toy-index add` again).\n\n" +
		"A renamed asset, a release missing some of its builds or a network failure never lands here: " +
		"those leave a release in place, and they stay on the index-check issue for a person.\n")
	return b.String()
}
