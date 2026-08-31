package main

import "context"

// runInstall 是核心流程，规格 §2：
// 解析 ref → (索引查 source) → git ls-remote 拿 tag → 挑最高 semver →
// raw URL 取 toy.toml → 按 os/arch 挑资产 → 下载(带镜像回退) → 验 sha256 →
// TOFU 检查 → 解压或 chmod+x → 软链(拒绝遮蔽) → 写清单 → 打印信任等级
func runInstall(ctx context.Context, args []string) error { panic("todo") }

// runUninstall 读清单，删软链和 pkg 目录。不删 cache —— 那是内容寻址的，
// 而且它同时是镜像目录。
func runUninstall(ctx context.Context, args []string) error { panic("todo") }

// runUpdate 对每个已装的包重新解析最新 tag，比 semver，有更新才走安装流程。
// 不带参数时更新全部，逐个报告，一个失败不影响其它。
func runUpdate(ctx context.Context, args []string) error { panic("todo") }
