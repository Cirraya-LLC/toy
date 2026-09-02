package main

// GitHub 的仓库/发布信息。**只有这个二进制会碰托管方的 REST API**，
// 理由见 main.go 的包注释（规格 §2：客户端那一侧一条都不许有）。

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

// 三种「这个仓库产不出条目」的失败各自要说不同的人话，所以是三个哨兵而不是
// 一句 error string。分类判断一律走 errors.Is，绝不按错误文本匹配 ——
// 那段文本里有服务器原样发来的内容（同规格 §5.1 第 3 条、§14.10 第 1 条）。
var (
	errNoRepo      = errors.New("repo does not exist, or is private and invisible to this token")
	errNoRelease   = errors.New("this repo has never published a release")
	errRateLimited = errors.New("host API rate limit")
)

// ghMaxBody 是单次 API 响应的读取上限。响应是远端内容，不设上限等于让上游
// 决定我们的内存用量；一个仓库的 release 元数据几十 KB 就够了，4 MiB 极宽松。
const ghMaxBody = 4 << 20

// repoInfo 是一个仓库产出一条索引条目所需要的全部远端事实。
//
// Assets 只有**文件名**，故意不带下载地址：地址由 host 模板 + tag 拼
// （规格 §5.1 —— 索引能改的是「装哪个」，不是「从哪装」）。把 API 给的
// browser_download_url 记下来会让这条边界多一个可以被绕过的地方。
type repoInfo struct {
	Desc     string
	Homepage string
	License  string
	Stars    int
	Tag      string   // 最新 release 的 tag，原样（通常带 v）
	Assets   []string // 资产文件名
}

// provider 是「从一个托管方读一个仓库的发布事实」。
//
// 现在只有 GitHub 一个实现。留这个形状是因为 GitLab 的 releases API
// （GET /projects/:id/releases，资产在 assets.links[].name）字段名完全不同，
// 接第二家的时候要动的只有这一个方法，gen 那条流水线一个字都不用改。
type provider interface {
	Fetch(ctx context.Context, owner, repo string) (repoInfo, error)
	// Quota 是最近一次请求之后剩下的配额，给报告结尾用。还不知道就返回空串。
	Quota() string
}

type ghClient struct {
	base  string // API 根，测试用 httptest 替掉
	token string
	hc    *http.Client

	remaining int // -1 表示还不知道
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
		License     struct {
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
			// 这条路上的 404 说的是「没有 release」而不是「没有这个仓库」——
			// 上面那次请求已经证明仓库在。
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
		License:  meta.License.SPDX,
		Stars:    meta.Stars,
		Tag:      rel.Tag,
	}
	// GitHub 对没有 license 的仓库给的是 "NOASSERTION"，那不是一个 SPDX 标识符，
	// 写进 toy.toml 只会让读的人以为是某种许可证。
	if info.License == "NOASSERTION" {
		info.License = ""
	}
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
	req.Header.Set("User-Agent", "toy-index") // GitHub 没有 UA 一律 403
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
		// 403 在 GitHub 上既是限流也是权限不足，靠剩余配额区分：
		// 报错文案指错方向的话，人会去查 token 权限而不是去等配额。
		if c.remaining == 0 {
			return fmt.Errorf("%w: %s", errRateLimited, c.Quota())
		}
		return fmt.Errorf("%s was refused (HTTP %d). %s", path, resp.StatusCode, c.Quota())
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s returned HTTP %d", path, resp.StatusCode)
	}

	// 响应是远端内容：不限长等于让上游决定我们的内存用量。
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
