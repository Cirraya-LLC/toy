package main

// Repo/release facts from GitHub. THIS BINARY IS THE ONLY ONE THAT TOUCHES A HOST'S
// REST API -- the reasoning is in main.go's package comment (spec §2: not one call
// allowed on the client side).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The three flavours of "this repo cannot produce an entry" each need to say something
// different in plain words, so they are three sentinels rather than one error string.
// Classification always goes through errors.Is, NEVER by matching error text -- that
// text carries content the server wrote verbatim (same as spec §5.1 rule 3 and §14.10
// point 1).
var (
	errNoRepo      = errors.New("repo does not exist, or is private and invisible to this token")
	errNoRelease   = errors.New("this repo has never published a release")
	errRateLimited = errors.New("host API rate limit")
)

// ghMaxBody caps how much of one API response we read. The response is remote content,
// and no cap means upstream decides our memory use; a repo's release metadata is tens of
// KB, so 4 MiB is wildly generous.
const ghMaxBody = 4 << 20

// repoInfo is every remote fact needed to produce one index entry for a repo.
//
// Assets holds FILENAMES ONLY, deliberately without download URLs: the URL is built from
// the host template plus the tag (spec §5.1 -- what the index can change is WHICH asset,
// not WHERE it comes from). Recording the browser_download_url the API hands us would
// give that boundary one more place to be bypassed.
type repoInfo struct {
	Desc     string
	Homepage string
	License  string       // SPDX identifier; empty unless LicState is licenceKnown
	LicState licenceState // whether GitHub found a licence file, and could name it
	Stars    int
	Tag      string   // the latest release's tag, verbatim (usually with a v)
	Assets   []string // asset filenames
}

// provider is "read one repo's release facts from one host".
//
// GitHub is the only implementation today. The shape stays because GitLab's releases API
// (GET /projects/:id/releases, assets under assets.links[].name) uses entirely different
// field names: adding a second host means touching this one method and nothing in the
// gen pipeline.
type provider interface {
	Fetch(ctx context.Context, owner, repo string) (repoInfo, error)
	// Quota is the quota left after the most recent request, for the end of the report.
	// Returns an empty string when it is not known yet.
	Quota() string
}

type ghClient struct {
	base  string // API root; tests swap in an httptest server
	token string
	hc    *http.Client

	remaining int // -1 means not known yet
	limit     int
	reset     time.Time
}

func newGitHub(base, token string) *ghClient {
	if base == "" {
		base = "https://api.github.com"
	}
	return &ghClient{
		base:      strings.TrimRight(base, "/"),
		token:     token,
		hc:        &http.Client{Timeout: 30 * time.Second},
		remaining: -1,
	}
}

func (c *ghClient) Quota() string {
	if c.remaining < 0 {
		return ""
	}
	s := strconv.Itoa(c.remaining)
	if c.limit > 0 {
		s += "/" + strconv.Itoa(c.limit)
	}
	s = "API quota: " + s + " remaining"
	if !c.reset.IsZero() {
		s += fmt.Sprintf(", resets at %s", c.reset.Local().Format("15:04:05"))
	}
	if c.token == "" {
		s += ". No token in use, and unauthenticated is only 60/hour: set $GITHUB_TOKEN to get 5000"
	}
	return s
}

func (c *ghClient) Fetch(ctx context.Context, owner, repo string) (repoInfo, error) {
	var meta struct {
		Description string `json:"description"`
		Homepage    string `json:"homepage"`
		Stars       int    `json:"stargazers_count"`
		// null when the repo has no licence file at all; spdx_id "NOASSERTION" (key
		// "other") when it has one GitHub cannot match. The two mean different
		// things -- all rights reserved versus "a person has to read it" -- so the
		// pointer is what keeps them apart.
		License *struct {
			SPDX string `json:"spdx_id"`
		} `json:"license"`
	}
	if err := c.get(ctx, "/repos/"+owner+"/"+repo, &meta); err != nil {
		if errors.Is(err, errNoRepo) {
			return repoInfo{}, errNoRepo
		}
		return repoInfo{}, err
	}

	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := c.get(ctx, "/repos/"+owner+"/"+repo+"/releases/latest", &rel); err != nil {
		if errors.Is(err, errNoRepo) {
			// A 404 on this path means "no release", not "no such repo" -- the request
			// above already proved the repo is there.
			return repoInfo{}, errNoRelease
		}
		return repoInfo{}, err
	}
	if strings.TrimSpace(rel.Tag) == "" {
		return repoInfo{}, errNoRelease
	}

	info := repoInfo{
		Desc:     meta.Description,
		Homepage: meta.Homepage,
		Stars:    meta.Stars,
		Tag:      rel.Tag,
	}
	info.License, info.LicState = licenceOf(meta.License)
	for _, a := range rel.Assets {
		info.Assets = append(info.Assets, a.Name)
	}
	return info, nil
}

func (c *ghClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "toy-index") // GitHub 403s anything without a UA
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", path, err)
	}
	defer resp.Body.Close()
	c.noteQuota(resp.Header)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNoRepo
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		// On GitHub a 403 means both "rate limited" and "not enough permission"; the
		// remaining quota tells them apart. Point the message the wrong way and people go
		// audit token scopes instead of waiting for the quota to reset.
		if c.remaining == 0 {
			return fmt.Errorf("%w: %s", errRateLimited, c.Quota())
		}
		return fmt.Errorf("%s was refused (HTTP %d). %s", path, resp.StatusCode, c.Quota())
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}

	// The response is remote content: no length limit means upstream decides our memory use.
	body, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxBody+1))
	if err != nil {
		return fmt.Errorf("reading the response from %s failed: %w", path, err)
	}
	if int64(len(body)) > ghMaxBody {
		return fmt.Errorf("the response from %s is over %d bytes, which is not what this API normally returns", path, int64(ghMaxBody))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("the response from %s is not the JSON we expected: %w", path, err)
	}
	return nil
}

func (c *ghClient) noteQuota(h http.Header) {
	if v, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
		c.remaining = v
	}
	if v, err := strconv.Atoi(h.Get("X-RateLimit-Limit")); err == nil {
		c.limit = v
	}
	if v, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && v > 0 {
		c.reset = time.Unix(v, 0)
	}
}

// licenceOf turns GitHub's licence object into an SPDX identifier and a state.
// "NOASSERTION" is not an SPDX identifier, and writing it into toy.toml would only make
// readers think it names some kind of licence.
func licenceOf(l *struct {
	SPDX string `json:"spdx_id"`
}) (string, licenceState) {
	switch {
	case l == nil:
		return "", licenceNone
	case l.SPDX == "" || l.SPDX == "NOASSERTION":
		return "", licenceUnrecognized
	default:
		return l.SPDX, licenceKnown
	}
}
