package main

// --probe：下载一个资产解开看，把 [bin] 右边从「猜的」变成「探到的」。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// probeMaxBytes：我们在下载陌生人的压缩包，而且只是为了看一眼里面的目录结构。
// 一个几百 MB 的资产多半不是 CLI 工具，不值得为它等下去。
const probeMaxBytes = 256 << 20

// probePlatforms 是挑来探的平台，按「最可能存在」排。探哪个平台不影响结论：
// 同一个 release 的各平台包布局一致（goreleaser / cargo-dist 都是一次配置产出全部）。
var probePlatforms = []string{"linux/amd64", "linux/amd64/gnu", "linux/amd64/musl", "linux/arm64"}

// probeBin 返回真实的包内相对路径。
//
// 第二个返回值是「找到了不止一个可执行文件、定不下来」时的候选清单 ——
// 这时第一个返回值是空串，**绝不瞎挑一个**：挑错的症状是用户装完之后 PATH 上
// 那个命令是个补全脚本，而报告里写着「已探测」。
//
// 解压一律走 internal/archive（铁律 7：路径穿越、符号链接逃逸、zip bomb 都在
// 它里面挡着）。这里下载的是陌生人的压缩包，正好需要那几条。
func probeBin(ctx context.Context, h host.Host, p pack) (string, []string, error) {
	plat, tmpl := probePick(p.res.Artifact)
	if tmpl == "" {
		return "", nil, errors.New("no linux/amd64 or linux/arm64 build to probe, skipping")
	}
	asset := toyfile.Expand(tmpl, p.res.Version, p.info.Tag)
	url := h.ReleaseURL(p.owner, p.repo, p.info.Tag, asset)

	got, err := fetch.Get(ctx, url, fetch.Options{Name: asset, MaxBytes: probeMaxBytes})
	if err != nil {
		return "", nil, fmt.Errorf("downloading %s (%s) failed: %w", asset, plat, err)
	}
	defer os.Remove(got.Path)

	dir, err := os.MkdirTemp("", "toy-index-probe-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)

	// archive 是按【文件名后缀】认格式的，而 fetch 落下来的临时文件没有后缀，
	// 所以先建一条带真资产名的软链（同 cmd/toy/install.go 的 insExtract）。
	link := filepath.Join(dir, filepath.Base(asset))
	if err := os.Symlink(got.Path, link); err != nil {
		return "", nil, err
	}
	out := filepath.Join(dir, "x")
	if err := os.Mkdir(out, 0o755); err != nil {
		return "", nil, err
	}
	files, err := archive.Extract(link, out)
	if err != nil {
		return "", nil, fmt.Errorf("unpacking %s failed: %w", asset, err)
	}

	hit, cands := pickExecutable(out, files, p.name)
	if hit == "" && len(cands) == 0 {
		return "", nil, fmt.Errorf("%s holds no executable at all (%d members)", asset, len(files))
	}
	return hit, cands, nil
}

func probePick(artifact map[string]string) (plat, tmpl string) {
	for _, k := range probePlatforms {
		if v, ok := artifact[k]; ok {
			return k, v
		}
	}
	return "", ""
}

// pickExecutable 从解压出来的文件里挑那个真正的命令。
//
// 只有一个可执行文件时就是它。多于一个时（补全脚本、man 页生成器、附带的
// 第二个工具）先看有没有一个的文件名恰好是我们要的命令名；再定不下来就把候选
// 全部交出去让人看 —— 猜一个出来会变成一条「看起来已经核对过」的错条目。
func pickExecutable(root string, files []string, want string) (string, []string) {
	var execs []string
	for _, rel := range files {
		fi, err := os.Lstat(filepath.Join(root, rel))
		// Lstat 而不是 Stat：符号链接指向的东西的权限位不是它自己的。
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		execs = append(execs, filepath.ToSlash(rel))
	}
	sort.Strings(execs)
	switch len(execs) {
	case 0:
		return "", nil
	case 1:
		return execs[0], nil
	}
	var named []string
	for _, e := range execs {
		if base := e[strings.LastIndex(e, "/")+1:]; base == want {
			named = append(named, e)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return "", execs
}

// unstableBinPath 判断探到的这个包内路径能不能写进 [bin]。
//
// [bin] 右边是**逐字**使用的：install 那边不展开 {version}，而且一份 toy.toml
// 只有一行 [bin] 给所有平台用（见 cmd/toy/install.go 的 insLink）。所以包里套了
// 一层 `tool_1.2.3_linux_amd64/` 这种目录时，探到的路径既钉死了版本又钉死了平台
// —— 写进内联条目就是一条「今天能装、上游一发新版就装不上」的记录，和留着
// [sha256] 是同一类错误。
//
// 平台判断借 assetname.Classify（它认出 os+arch 才会给出 Platform），
// 不在这里另抄一份 linux/darwin/amd64 的词表 —— 抄一份就是一处会慢慢分叉的地方。
func unstableBinPath(path, version string) string {
	if version != "" && strings.Contains(path, version) {
		return "it spells out the version " + version + ", so the next upstream release points at a directory that does not exist"
	}
	for _, seg := range strings.Split(path, "/") {
		if p := assetname.Classify(seg).Platform; p != "" {
			return "it spells out a platform name (" + seg + " -> " + p + "), and [bin] is a single line shared by every platform"
		}
	}
	return ""
}
