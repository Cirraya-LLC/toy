// Package fetch 下载、校验、缓存。规格 §8 的镜像回退也在这里。
package fetch

import (
	"context"
	"errors"
)

var (
	ErrChecksum  = errors.New("fetch: sha256 校验失败")
	ErrAllFailed = errors.New("fetch: 原始地址和所有镜像都拿不到")
)

// Sink 是进度回调。定义在这里而不是依赖 ui，是为了让依赖方向朝下。
type Sink interface {
	Start(name string, total int64)
	Add(n int64)
	Done(err error)
}

type Options struct {
	SHA256   string   // 非空则强制校验，对不上返回 ErrChecksum 并删掉临时文件（铁律 4）
	Mirrors  []string // 镜像 base；镜像地址 = base + "/" + sha256（只在有 SHA256 时可用）
	CacheDir string   // 空表示不缓存
	Sink     Sink     // 可为 nil
	Name     string   // 显示用
}

type Result struct {
	Path      string // 落地的文件路径（缓存里或临时目录里）
	SHA256    string // 实际算出来的
	FromCache bool
	Source    string // 实际成功的那个 URL，用于如实告知用户
}

// Get 下载到本地文件。有 CacheDir 且 SHA256 命中缓存时直接返回。
// 原始地址失败（网络错、4xx、5xx、校验不过）就按顺序试 Mirrors，
// 每一个镜像的内容一律重新验 hash —— 所以镜像不需要被信任。
func Get(ctx context.Context, url string, opt Options) (Result, error) { panic("todo") }

// GetBytes 用于小文件（toy.toml、索引 JSON）。支持 file:// 和 https://。
// 必须有大小上限，防止一个坏 URL 把内存吃光。
func GetBytes(ctx context.Context, url string, opt Options) ([]byte, error) { panic("todo") }

// SumFile 算一个已有文件的 sha256（hex）。
func SumFile(path string) (string, error) { panic("todo") }
