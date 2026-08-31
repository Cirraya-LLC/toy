package main

import (
	"context"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// resolved 是把一个 ref 走到「可以开始下载了」这一步的全部结果。
// install / info / update 三条路共用它，别各写一份。
type resolved struct {
	Ref      ref.Ref
	HostName string
	Host     host.Host
	Owner    string
	Repo     string
	Tag      string   // 原始 tag，通常带 v
	Version  semver.V //
	Toyfile  *toyfile.Toyfile
	Entry    *index.Entry // 经索引来的才有，否则 nil
	Trust    string       // store.TrustSigned / TrustHost / TrustUnverified
	Signer   string       // 公钥指纹，未签名为空
}

// openStore 打开 ~/.toy 并读配置，是每个子命令的第一行。
func openStore() (*store.Store, store.Config, error) { panic("todo") }

// loadIndexes 按配置顺序加载全部索引（走 fetch，所以 file:// 和 https:// 都行，
// 并缓存到 ~/.toy/index/）。单个索引失败只警告不中断 —— 一个索引挂了
// 不该让另一个索引里的包也搜不到。
func loadIndexes(ctx context.Context, st *store.Store, cfg store.Config) ([]*index.Index, error) {
	panic("todo")
}

// resolve 执行规格 §2 的第 1–3 步：
//
//	KindIndex → 查索引拿到 source，再按 KindHost 走下去；查不到就报错并建议用 github: 直接装
//	KindHost  → git ls-remote 拿 tag，挑最高 semver（r.Version 非空则用它），
//	            再用 raw URL 模板取那个 tag 上的 toy.toml
//	KindURL   → 直接取那个 toy.toml，Tag/Version 由 toyfile 里没有 —— 用 "unknown"
//
// 同时定出 Trust 等级（规格 §4）。不下载 artifact，不碰磁盘上的 pkg 目录。
func resolve(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref) (*resolved, error) {
	panic("todo")
}
