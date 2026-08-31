// Package host 把「git 托管方」变成一张配置表而不是三份 API 适配器。
// 规格 §2：绝不用 GitHub/GitLab 的 REST API —— 限流 60 次/小时，而且要写三套。
package host

import (
	"context"
	"errors"
)

var ErrUnknownHost = errors.New("host: 不认识这个托管方")

// Host 是四个 URL 模板。占位符：{owner} {repo} {ref} {path} {tag} {asset}
type Host struct {
	Name    string `toml:"-"`
	Raw     string `toml:"raw"`     // 取仓库里某个文件
	Clone   string `toml:"clone"`   // git ls-remote 用
	Release string `toml:"release"` // release 资产下载地址
	Web     string `toml:"web"`     // 给人看的仓库地址
}

// Builtin 返回内置的托管方表：github / gitlab / codeberg / gitea 通用形态。
// 用户在 config.toml 里加三行就能多一个托管方 —— host 是配置不是代码。
func Builtin() map[string]Host { panic("todo") }

// Lookup 先查 overrides 再查内置表。
func Lookup(name string, overrides map[string]Host) (Host, error) { panic("todo") }

func (h Host) RawURL(owner, repo, ref, path string) string      { panic("todo") }
func (h Host) CloneURL(owner, repo string) string               { panic("todo") }
func (h Host) ReleaseURL(owner, repo, tag, asset string) string { panic("todo") }
func (h Host) WebURL(owner, repo string) string                 { panic("todo") }

// Tags 用 `git ls-remote --tags <clone-url>` 拿全部 tag 名。
// 一条命令，所有 git 托管方行为一致，不用 API、不用 token、不限流。
// 必须带 context 超时；剥掉 refs/tags/ 前缀和 ^{} 后缀（annotated tag 的解引用行）。
func Tags(ctx context.Context, cloneURL string) ([]string, error) { panic("todo") }
