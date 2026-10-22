package main

// The second source for discover: curated lists. Plenty of well-known tools never tag
// themselves with a topic, so the topic search cannot see them, but someone put them
// on an awesome-list years ago. Those lists are already in owner/repo form, which is
// the cold-start plan the index started from.
//
// A list is a README full of GitHub links. Their metadata comes from one GraphQL query
// per 50 repositories: asking the REST API one by one would spend a list's worth of a
// GITHUB_TOKEN's 1000 calls an hour before gen had looked at anything.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// defaultLists are where command-line tools get collected by hand.
var defaultLists = []string{"agarrharr/awesome-cli-apps", "ibraheemdev/modern-unix", "rothgar/awesome-tuis"}

// ghLink finds github.com/owner/repo in markdown. Owners are letters, digits and
// hyphens; a repo name may also carry dots and underscores.
var ghLink = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)`)

// notAnOwner are path segments on github.com that look like an owner and are not one.
var notAnOwner = map[string]bool{
	"about": true, "apps": true, "features": true, "login": true, "marketplace": true,
	"orgs": true, "settings": true, "sponsors": true, "topics": true, "user-attachments": true,
}

// listRepos reads one list's README and returns the repositories it links to, in order.
func (c *ghClient) listRepos(ctx context.Context, list string) ([]string, error) {
	var readme struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.get(ctx, "/repos/"+list+"/readme", &readme); err != nil {
		return nil, fmt.Errorf("reading the README of %s: %w", list, err)
	}
	if readme.Encoding != "base64" {
		return nil, fmt.Errorf("the README of %s came back as %q, not base64", list, readme.Encoding)
	}
	md, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(readme.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("the README of %s does not decode: %w", list, err)
	}
	return linkedRepos(string(md), list), nil
}

func linkedRepos(md, self string) []string {
	seen := map[string]bool{foldRepo(self): true}
	var out []string
	for _, m := range ghLink.FindAllStringSubmatch(md, -1) {
		owner, repo := m[1], strings.TrimSuffix(strings.TrimRight(m[2], "."), ".git")
		if notAnOwner[strings.ToLower(owner)] || repo == "" {
			continue
		}
		id := owner + "/" + repo
		if _, err := listID(id); err != nil || seen[foldRepo(id)] {
			continue
		}
		seen[foldRepo(id)] = true
		out = append(out, id)
	}
	return out
}

// repoMeta looks up repositories in batches of 50 and returns them as search results,
// so the rest of discover cannot tell which source a candidate came from. A repository
// that no longer exists is simply absent.
func (c *ghClient) repoMeta(ctx context.Context, ids []string) ([]searchItem, error) {
	var out []searchItem
	for start := 0; start < len(ids); start += 50 {
		batch := ids[start:min(start+50, len(ids))]
		var q strings.Builder
		q.WriteString("query {")
		for i, id := range batch {
			owner, name, _ := strings.Cut(id, "/")
			// listID has already held these to [A-Za-z0-9._-], so they cannot break
			// out of the string literal.
			fmt.Fprintf(&q, ` r%d: repository(owner: %q, name: %q) { nameWithOwner description stargazerCount `+
				`isArchived isFork isMirror createdAt pushedAt licenseInfo { spdxId } }`, i, owner, name)
		}
		q.WriteString(" }")
		var res struct {
			Data map[string]*struct {
				Name      string    `json:"nameWithOwner"`
				Desc      string    `json:"description"`
				Stars     int       `json:"stargazerCount"`
				Archived  bool      `json:"isArchived"`
				Fork      bool      `json:"isFork"`
				Mirror    bool      `json:"isMirror"`
				CreatedAt time.Time `json:"createdAt"`
				PushedAt  time.Time `json:"pushedAt"`
				License   *struct {
					SPDX string `json:"spdxId"`
				} `json:"licenseInfo"`
			} `json:"data"`
		}
		// Partial data with an "errors" array is the normal answer when one of the 50
		// is gone; only a response with no data at all is a failure.
		if err := c.graphql(ctx, q.String(), &res); err != nil {
			return nil, err
		}
		for _, r := range res.Data {
			if r == nil || r.Archived || r.Fork || r.Mirror {
				continue
			}
			it := searchItem{FullName: r.Name, Desc: r.Desc, Stars: r.Stars, PushedAt: r.PushedAt, CreatedAt: r.CreatedAt}
			if r.License != nil {
				it.License = &struct {
					SPDX string `json:"spdx_id"`
				}{SPDX: r.License.SPDX}
			}
			out = append(out, it)
		}
	}
	return out, nil
}

func (c *ghClient) graphql(ctx context.Context, query string, v any) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "toy-index")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("GraphQL request failed: %w", err)
	}
	defer resp.Body.Close()
	c.noteQuota(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxBody+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GraphQL returned HTTP %d. %s", resp.StatusCode, c.Quota())
	}
	if len(data) > ghMaxBody {
		return fmt.Errorf("the GraphQL response is over %d bytes", ghMaxBody)
	}
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || len(probe.Data) == 0 || string(probe.Data) == "null" {
		return fmt.Errorf("GraphQL answered without data: %s", tomlSafe(string(data), 300))
	}
	return json.Unmarshal(data, v)
}
