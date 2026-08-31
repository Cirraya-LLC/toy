// Package fetch 下载、校验、缓存。规格 §8 的镜像回退也在这里。
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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

const (
	// maxBytes 是 GetBytes 的上限。这条路只用来取 toy.toml 和索引 JSON，
	// 32 MiB 足够有余，同时挡住一个坏 URL 把内存吃光。
	maxBytes = 32 << 20
	// maxRedirects 跟随重定向但不无限跟，避免转圈。
	maxRedirects = 10
)

var client = &http.Client{
	// 整次下载的兜底上限。真正的"服务器没反应"由 ResponseHeaderTimeout 更快地发现。
	Timeout: 30 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		// 必须关掉透明压缩：Go 会自己加 Accept-Encoding: gzip 再自己解开，
		// 那样我们算的 hash 是【解压后】的字节，而发布者算的是 release 资产文件本身。
		// 后果一是有 [sha256] 的包全部误报"校验失败"，二是没有 hash 的 TOFU 路径会把
		// 错的字节以错的 hash 钉住并存进 cache/<hash> —— 而那个目录就是 toy serve 的镜像源（规格 §6/§8）。
		// 规矩：算 hash 的字节必须就是线上的字节。
		DisableCompression: true,
	},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("重定向超过 %d 次，疑似死循环", maxRedirects)
		}
		return nil
	},
}

// nopSink 让 Options.Sink 为 nil 时不用到处判空。
type nopSink struct{}

func (nopSink) Start(string, int64) {}
func (nopSink) Add(int64)           {}
func (nopSink) Done(error)          {}

// sinkWriter 把写入的字节数转成进度。
type sinkWriter struct{ s Sink }

func (w sinkWriter) Write(p []byte) (int, error) {
	w.s.Add(int64(len(p)))
	return len(p), nil
}

// normalize 统一可选字段，顺便挡住畸形的 sha256。
// sha256 会被当成缓存里的文件名和镜像 URL 的路径段，而它来自网络上的 toy.toml ——
// 不校验格式的话，一个 "../../x" 就能写到缓存目录外面去。
func normalize(opt *Options) error {
	if opt.Sink == nil {
		opt.Sink = nopSink{}
	}
	opt.SHA256 = strings.ToLower(strings.TrimSpace(opt.SHA256))
	if opt.SHA256 == "" {
		return nil
	}
	if len(opt.SHA256) != 64 {
		return fmt.Errorf("fetch: sha256 %q 不是 64 位十六进制，检查 toy.toml 的 [sha256] 段", opt.SHA256)
	}
	if _, err := hex.DecodeString(opt.SHA256); err != nil {
		return fmt.Errorf("fetch: sha256 %q 里有非十六进制字符，检查 toy.toml 的 [sha256] 段", opt.SHA256)
	}
	return nil
}

// sources 返回按顺序尝试的地址：原始地址在前，镜像在后。
// 没有 sha256 就不给镜像 —— 拿不到 hash 就无法验证镜像的内容，那时候用镜像等于信任陌生人（规格 §8）。
func sources(u string, opt Options) []string {
	out := []string{u}
	if opt.SHA256 == "" {
		return out
	}
	for _, m := range opt.Mirrors {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		out = append(out, strings.TrimSuffix(m, "/")+"/"+opt.SHA256)
	}
	return out
}

// open 打开一个来源，返回内容流和已知长度（未知是 -1）。
// file:// 走 os.Open，不塞给 http.Client。
func open(ctx context.Context, src string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(src, "file://") {
		p, err := filePath(src)
		if err != nil {
			return nil, 0, err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, 0, err
		}
		size := int64(-1)
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		return f, size, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

func filePath(src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil {
		return "", fmt.Errorf("file:// 地址解析不了：%w", err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("file:// 不支持主机名 %q，写成 file:///绝对路径", u.Host)
	}
	if u.Path == "" {
		return "", fmt.Errorf("file:// 地址 %q 里没有路径", src)
	}
	// 调用方一般是 "file://" + 路径拼出来的。路径里只要有 ? 或 #，url.Parse 会把它
	// 之后的部分切成 query/fragment，于是我们【静默地打开另一个文件】——
	// file:///x/a#b 会去开 /x/a。文件地址不该有这两样，宁可报错也不猜（铁律 8）。
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("file:// 地址 %q 里有 ? 或 #，它们会被当成 query/fragment 指向别的文件；文件名里真有这两个字符就写成 %%3F / %%23", src)
	}
	return filepath.FromSlash(u.Path), nil
}

// Get 下载到本地文件。有 CacheDir 且 SHA256 命中缓存时直接返回。
// 原始地址失败（网络错、4xx、5xx、校验不过）就按顺序试 Mirrors，
// 每一个镜像的内容一律重新验 hash —— 所以镜像不需要被信任。
//
// CacheDir 为空时结果落在系统临时目录里，那个文件归调用方处置。
func Get(ctx context.Context, u string, opt Options) (Result, error) {
	if err := normalize(&opt); err != nil {
		return Result{}, err
	}

	// 命中的判据是"文件在 且 重新算一遍对得上"。只看文件在不在的话，
	// 一个被截断或被改过的缓存文件会被当成好的直接装上去。
	if opt.CacheDir != "" && opt.SHA256 != "" {
		p := filepath.Join(opt.CacheDir, opt.SHA256)
		if sum, err := SumFile(p); err == nil && sum == opt.SHA256 {
			return Result{Path: p, SHA256: sum, FromCache: true, Source: u}, nil
		}
	}

	var errs []error
	for _, src := range sources(u, opt) {
		res, err := fetchOne(ctx, src, opt)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s：%w", src, err))
		if ctx.Err() != nil {
			break
		}
	}
	return Result{}, fmt.Errorf("%w：%s：%w", ErrAllFailed, u, errors.Join(errs...))
}

// fetchOne 从一个来源取到本地：先写临时文件，边写边算 hash，验过了才原子 rename
// 进缓存。所以缓存目录里永远不会出现半个文件。
func fetchOne(ctx context.Context, src string, opt Options) (res Result, err error) {
	dir := opt.CacheDir
	if dir != "" {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, fmt.Errorf("建不了缓存目录 %s：%w", dir, err)
		}
	}
	// 临时文件和最终位置必须在同一个目录里，跨文件系统的 rename 不是原子的（而且会直接失败）。
	tmp, err := os.CreateTemp(dir, ".toy-tmp-*")
	if err != nil {
		return Result{}, fmt.Errorf("建不了临时文件：%w", err)
	}
	tmpName := tmp.Name()
	placed, started := false, false
	defer func() {
		tmp.Close()
		if !placed {
			// 校验不过、中途断了、写盘失败 —— 一律不留残骸（铁律 4）。
			os.Remove(tmpName)
		}
		if started {
			opt.Sink.Done(err)
		}
	}()

	body, total, err := open(ctx, src)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	started = true
	opt.Sink.Start(opt.Name, total)
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, h, sinkWriter{opt.Sink}), body); err != nil {
		return Result{}, fmt.Errorf("下载中断：%w", err)
	}
	if err = tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("写临时文件失败：%w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if opt.SHA256 != "" && sum != opt.SHA256 {
		err = fmt.Errorf("%w：期望 %s，实际 %s。已删掉下载的文件，不会安装", ErrChecksum, opt.SHA256, sum)
		return Result{}, err
	}

	path := tmpName
	if opt.CacheDir != "" {
		// 缓存文件名就是内容 hash，所以缓存目录天然可以直接当镜像目录用（规格 §8）。
		if err = os.Chmod(tmpName, 0o644); err != nil {
			return Result{}, err
		}
		path = filepath.Join(opt.CacheDir, sum)
		if err = os.Rename(tmpName, path); err != nil {
			return Result{}, fmt.Errorf("放进缓存失败：%w", err)
		}
	}
	placed = true
	return Result{Path: path, SHA256: sum, Source: src}, nil
}

// GetBytes 用于小文件（toy.toml、索引 JSON）。支持 file:// 和 https://。
// 必须有大小上限，防止一个坏 URL 把内存吃光。
func GetBytes(ctx context.Context, u string, opt Options) ([]byte, error) {
	if err := normalize(&opt); err != nil {
		return nil, err
	}
	var errs []error
	for _, src := range sources(u, opt) {
		b, err := getBytesOne(ctx, src, opt)
		if err == nil {
			return b, nil
		}
		errs = append(errs, fmt.Errorf("%s：%w", src, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w：%s：%w", ErrAllFailed, u, errors.Join(errs...))
}

func getBytesOne(ctx context.Context, src string, opt Options) ([]byte, error) {
	body, _, err := open(ctx, src)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	// 多读一个字节，好区分"正好到上限"和"超了"。
	b, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, fmt.Errorf("内容超过 %d MiB 上限；这条路只用来取 toy.toml 和索引 JSON，检查一下地址对不对", maxBytes>>20)
	}
	if opt.SHA256 != "" {
		s := sha256.Sum256(b)
		if sum := hex.EncodeToString(s[:]); sum != opt.SHA256 {
			return nil, fmt.Errorf("%w：期望 %s，实际 %s", ErrChecksum, opt.SHA256, sum)
		}
	}
	return b, nil
}

// SumFile 算一个已有文件的 sha256（hex）。
func SumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
