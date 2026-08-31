package main

import "context"

// runList 打印已装的包：名字、版本、信任等级、来源。
func runList(ctx context.Context, args []string) error { panic("todo") }

// runSearch 加载配置里的全部索引（走 fetch，所以 file:// 和 https:// 都行），
// 本地搜索并打印排名结果。索引拉不到时警告并继续用其它索引。
func runSearch(ctx context.Context, args []string) error { panic("todo") }

// runInfo 解析 ref 并把能查到的全部信息打出来：描述、可用版本、支持的平台、
// 信任状态、是否已装。这是用户在装之前该看的那一屏。
func runInfo(ctx context.Context, args []string) error { panic("todo") }

// runIndex 是 add / rm / list 三个子动作。
func runIndex(ctx context.Context, args []string) error { panic("todo") }

// runDoctor 检查：BinDir 在不在 PATH、目录权限、git 在不在、
// 每个索引可不可达、有没有悬空的软链。
func runDoctor(ctx context.Context, args []string) error { panic("todo") }
