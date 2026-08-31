package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// insNoVersion 是「这个包没有版本这回事」时占的位（裸地址那条路）。
// 它会变成 pkg 目录里的一段，所以必须是个能当目录名的词，不能是空串。
const insNoVersion = "unversioned"

// runInstall 是核心流程，规格 §2：
// 解析 ref → (索引查 source) → git ls-remote 拿 tag → 挑最高 semver →
// raw URL 取 toy.toml → 按 os/arch 挑资产 → 下载(带镜像回退) → 验 sha256 →
// TOFU 检查 → 解压或 chmod+x → 软链(拒绝遮蔽) → 写清单 → 打印信任等级
func runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	force := fs.Bool("force", false, "覆盖已有的命令、接受变了的哈希/公钥、重装同一个版本")
	version := fs.String("version", "", "装指定的版本，而不是最高的那个 tag")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}

	r, err := ref.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(*version); v != "" {
		// ref 里的 @version 和 --version 说的是同一件事，两个都写了还不一样
		// 的话，装哪个都是猜 —— 宁可让他自己决定。
		if r.Version != "" && r.Version != v {
			return fmt.Errorf("%s 里写着 @%s，--version 又写了 %s，这两个不一样。"+
				"留一个再试", r.Raw, r.Version, v)
		}
		if r.Kind == ref.KindURL {
			return fmt.Errorf("--version 对裸地址没有意义：%s 直接指着一个 toy.toml，"+
				"没有 tag 可挑。改用 `toy install github:owner/repo@%s` 这种写法", r.URL, v)
		}
		// 版本号会被拼进下载地址和 pkg 目录名，所以照 ref 自己那套规则验一遍，
		// 别绕过去（这里借 ref.Parse，不另抄一份白名单）。
		probe, perr := ref.Parse("owner/repo@" + v)
		if perr != nil {
			return fmt.Errorf("--version %q 用不了：%w", v, perr)
		}
		r.Version = probe.Version
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	res, err := resolve(ctx, st, cfg, r)
	if err != nil {
		return err
	}

	old, err := insExisting(st, insPackageID(res))
	if err != nil {
		return err
	}
	// 已经是这一版就别再下一遍。--force 是「我知道，重装」。
	if old != nil && !*force && res.Tag != "" && old.Tag == res.Tag {
		ui.Info("%s 已经是 %s 了，没什么可做的（要重装加 --force）", old.Name, old.Tag)
		return nil
	}
	return insPerform(ctx, st, cfg, res, old, *force)
}

// runUninstall 读清单，删软链和 pkg 目录。不删 cache —— 那是内容寻址的，
// 而且它同时是镜像目录。
func runUninstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	// 命中多个时 FindByName 已经把候选列出来了，原样透出去别再包一层。
	m, err := st.FindByName(fs.Arg(0))
	if err != nil {
		return err
	}

	// 先删软链：清单还在的时候失败，用户重跑一次就行；反过来先删清单的话
	// 那些软链就再也没人知道该删谁了。
	for _, l := range m.Links {
		if err := st.Unlink(l); err != nil {
			return err
		}
	}
	if err := insRemovePkgDir(st, m.PkgDir); err != nil {
		return err
	}
	if err := st.RemoveManifest(m.ID); err != nil {
		return err
	}
	ui.Info("已卸载 %s %s（缓存留着 —— 它是内容寻址的，同时也是 `toy serve` 的镜像源）",
		m.Name, m.Version)
	return nil
}

// runUpdate 对每个已装的包重新解析最新 tag，比 semver，有更新才走安装流程。
// 不带参数时更新全部，逐个报告，一个失败不影响其它。
func runUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errUsage
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	var todo []store.Manifest
	if fs.NArg() == 1 {
		m, err := st.FindByName(fs.Arg(0))
		if err != nil {
			return err
		}
		todo = []store.Manifest{m}
	} else if todo, err = st.ListManifests(); err != nil {
		return err
	}
	if len(todo) == 0 {
		ui.Info("一个包都没装，没什么可升级的")
		return nil
	}

	updated, current, failed := 0, 0, 0
	for _, m := range todo {
		switch err := insUpdateOne(ctx, st, cfg, m); {
		case err == nil:
			updated++
		case errors.Is(err, insErrUpToDate):
			current++
		case errors.Is(err, context.Canceled):
			return err // Ctrl-C 就是 Ctrl-C，别接着去拉下一个包
		default:
			// 一个包失败不该拖住别的包（它们各自独立，铁律 3）。
			ui.Error("%s：%v", m.ID, err)
			failed++
		}
	}
	ui.Info("%d 个已更新，%d 个已是最新，%d 个失败", updated, current, failed)
	if failed > 0 {
		return fmt.Errorf("%d 个包没能更新（原因在上面）", failed)
	}
	return nil
}

// insErrUpToDate 不是错误，是 insUpdateOne 的第三种结果。
var insErrUpToDate = errors.New("已是最新")

func insUpdateOne(ctx context.Context, st *store.Store, cfg store.Config, m store.Manifest) error {
	// 当初怎么装的就怎么升：经索引装的要再走一次索引，否则作者的签名（只写在
	// 索引条目里）第一次升级就丢了，而清单会如实记下降了一档的信任等级。
	id := m.ID
	if m.IndexID != "" {
		id = m.IndexID
	}
	r, err := ref.Parse(id)
	if err != nil {
		return fmt.Errorf("清单里的 id %q 解析不了，没法重新查它的版本：%w", id, err)
	}
	if r.Kind == ref.KindURL {
		// 裸地址指着一个固定的 toy.toml，没有 tag 可比 —— 想换版本只能
		// 重新 install 一次那个地址。说清楚，不要假装检查过了。
		ui.Info("跳过 %s：它是从一个固定地址装的，没有版本可比。要换内容就重新 "+
			"`toy install %s`", m.Name, m.ID)
		return insErrUpToDate
	}

	res, err := resolve(ctx, st, cfg, r)
	if err != nil && m.IndexID != "" && m.IndexID != m.ID {
		// 索引里查不到了不代表装不了 —— source 才是真相，下架无害（规格 §5）。
		// 但要说出来：这时候作者的公钥和签名也跟着没了，信任等级会掉一档。
		ui.Warn("%s 现在不在索引里了（%v），改用 %s 直接升级；"+
			"索引条目没了，作者签名也就无从验起", m.Name, err, m.ID)
		if r, err = ref.Parse(m.ID); err != nil {
			return fmt.Errorf("清单里的 id %q 解析不了，没法重新查它的版本：%w", m.ID, err)
		}
		res, err = resolve(ctx, st, cfg, r)
	}
	if err != nil {
		return err
	}
	if !insIsNewer(m, res) {
		ui.Info("%s %s 已是最新", m.Name, m.Tag)
		return insErrUpToDate
	}
	ui.Step("%s %s → %s", m.Name, m.Tag, res.Tag)
	old := m
	return insPerform(ctx, st, cfg, res, &old, false)
}

// insIsNewer 判断远端那个 tag 是不是比已装的新。
func insIsNewer(m store.Manifest, res *resolved) bool {
	newV, nerr := semver.Parse(res.Tag)
	oldV, oerr := semver.Parse(m.Tag)
	if nerr != nil || oerr != nil {
		// 有一边不是 semver 就没有大小可言。这时候只能按「tag 变了就是有新版」
		// 判断 —— 把解析不了的当成 0.0.0 会让一个正常的包被一路"降级"。
		return res.Tag != "" && res.Tag != m.Tag
	}
	return semver.Compare(newV, oldV) > 0
}

// insItem 是这次安装要下的一个文件。[artifact] 形态只有一项（可能是压缩包），
// [script] 形态是每个条目一项。
type insItem struct {
	name   string // 命令名（script）或资产名（artifact），显示用
	asset  string // 展开完占位符的资产名 / 仓库内路径
	url    string
	want   string // toy.toml 声明的 sha256，空表示走 TOFU
	path   string // 下载落地的位置
	sum    string // 实际算出来的 sha256
	source string // 实际成功的那个地址（可能是镜像）
}

// insPerform 是 install 和 update 共用的那一段：下载 → TOFU → 落盘 → 软链 →
// 写清单 → 钉住 → 打印信任等级。old 非 nil 表示这是在替换一个已经装上的版本。
func insPerform(ctx context.Context, st *store.Store, cfg store.Config, res *resolved, old *store.Manifest, force bool) error {
	if !force && insTrustDowngraded(old, res) {
		return fmt.Errorf("上一次装 %s 的时候它是已签名的（key %s），这次却验不出签名 —— "+
			"索引里那条记录可能被摘掉了 sig，也可能你这次是从别的路径装的。"+
			"上面那行警告说了具体原因；自己核对过再加 --force 继续", old.Name, old.Signer)
	}
	tf := res.Toyfile
	items, err := insItems(res)
	if err != nil {
		return err
	}
	ring, err := sign.OpenRing(st.KnownDir())
	if err != nil {
		return err
	}

	// 下载 + TOFU 全部做完再动磁盘上的包目录：中途拒绝的话，已经装着的那一版
	// 必须原封不动。
	type pin struct{ id, kind, value string }
	var pins []pin
	trustID := insTrustID(res)
	for i := range items {
		it := &items[i]
		dl, err := fetch.Get(ctx, it.url, fetch.Options{
			SHA256:   it.want,
			Mirrors:  cfg.Mirrors,
			CacheDir: st.CacheDir(),
			Sink:     ui.NewProgress(),
			Name:     it.name,
		})
		if err != nil {
			if errors.Is(err, fetch.ErrChecksum) {
				// 铁律 4：对不上一律拒绝。期望/实际由 fetch 写在里面了。
				return fmt.Errorf("%s 的内容和 toy.toml 里声明的 sha256 对不上，已拒绝安装：%w", it.asset, err)
			}
			return err
		}
		it.path, it.sum, it.source = dl.Path, dl.SHA256, dl.Source

		id := trustID
		if len(items) > 1 {
			id += "/" + it.name
		}
		if err := insTOFU(ring, id, "sha256", it.sum, it.name+" 的 sha256", force); err != nil {
			return err
		}
		pins = append(pins, pin{id, "sha256", it.sum})
	}
	// 索引说这个包有作者公钥的话，公钥也钉住（规格 §4：公钥和 artifact hash
	// 首次安装都要钉）。换了一把钥匙就是换了一个"作者"，那正是要拦的事。
	if res.Entry != nil && res.Signer != "" {
		key := strings.TrimSpace(res.Entry.Key)
		if err := insTOFU(ring, insPackageID(res), "key", key, "作者公钥", force); err != nil {
			return err
		}
		pins = append(pins, pin{insPackageID(res), "key", key})
	}

	pkgDir := st.PkgDir(insHostSeg(res), insOwnerSeg(res), tf.Name, insDirVersion(res))
	if err := insRemovePkgDir(st, pkgDir); err != nil {
		return err
	}
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return fmt.Errorf("建不出包目录 %s：%w", pkgDir, err)
	}
	for _, it := range items {
		if err := insUnpack(tf, it, pkgDir); err != nil {
			os.RemoveAll(pkgDir) // 半个包比没有包更难查
			return err
		}
	}

	links, err := insLink(st, tf, pkgDir, old, force)
	if err != nil {
		return err
	}

	m := store.Manifest{
		ID: insPackageID(res), IndexID: insIndexID(res), Name: tf.Name,
		Version: insVersionLabel(res), Tag: res.Tag,
		Host: res.HostName, Owner: res.Owner, Repo: res.Repo,
		Trust: res.Trust, Signer: res.Signer,
		SHA256: items[0].sum, Source: items[0].source,
		PkgDir: pkgDir, Links: links,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.SaveManifest(m); err != nil {
		return err
	}
	// 装成了才钉。失败还钉住的话，下一次安装会拿一个坏值当"首次安装钉住的"。
	for _, p := range pins {
		if err := ring.Pin(p.id, p.kind, p.value); err != nil {
			ui.Warn("钉不住 %s 的 %s：%v（下次安装会当成第一次）", p.id, p.kind, err)
		}
	}
	insCleanOld(st, old, pkgDir, links)

	ui.Info("已安装 %s %s → %s", tf.Name, m.Version, strings.Join(insBaseNames(links), "、"))
	if !st.InPath() {
		ui.Warn("%s 还不在 PATH 里，装好的命令暂时敲不出来。把它加进 PATH 再试（`toy doctor` 有具体做法）", st.BinDir())
	}
	ui.Trust(res.Trust, insTrustDetail(res))
	return nil
}

// insTrustDowngraded 判断这次安装是不是把一个已签名的包降级成了没签名的。
//
// 光验签名还不够：把 sig 从索引条目里删掉就能让「✓ 已签名」悄悄变成
// 「· 来源 …」，而没人会注意到少了一个勾。TOFU 记的是「上次是什么样」，
// 所以这里也一样 —— 降级要停下来让用户亲手 --force（规格 §4、铁律 8）。
func insTrustDowngraded(old *store.Manifest, res *resolved) bool {
	return old != nil && old.Trust == store.TrustSigned && res.Trust != store.TrustSigned
}

// insItems 按形态挑出这次要下载的东西。
func insItems(res *resolved) ([]insItem, error) {
	tf := res.Toyfile
	version := insVersion(res)

	if tf.IsScript() {
		// [script] 的值是仓库里的一个路径（例子里就是 "gitwip.sh"），走 raw 模板，
		// 不是 release 资产 —— 单文件脚本的意义就是不用发 release。
		out := make([]insItem, 0, len(tf.Script))
		for _, name := range slices.Sorted(maps.Keys(tf.Script)) {
			asset := toyfile.Expand(tf.Script[name], version, res.Tag)
			// 展开之后才是真正拼进地址的那一串，而 {tag} 是上游给的。再验一次。
			if err := insSafeAsset(name, asset); err != nil {
				return nil, err
			}
			u, err := insAssetURL(res, asset, true)
			if err != nil {
				return nil, err
			}
			out = append(out, insItem{name: name, asset: asset, url: u})
		}
		return out, nil
	}

	asset, want, err := tf.Asset(runtime.GOOS, runtime.GOARCH, version, res.Tag)
	if err != nil {
		return nil, err
	}
	u, err := insAssetURL(res, asset, false)
	if err != nil {
		return nil, err
	}
	return []insItem{{name: asset, asset: asset, url: u, want: want}}, nil
}

// insAssetURL 拼一个资产的下载地址。
func insAssetURL(res *resolved, asset string, script bool) (string, error) {
	if res.Ref.Kind == ref.KindURL {
		// 裸地址那条路没有托管方，资产只能相对 toy.toml 自己的地址解析 ——
		// 除此之外没有任何信息能说出它在哪。
		base, err := url.Parse(res.Ref.URL)
		if err != nil {
			return "", fmt.Errorf("%s 解析不了，拼不出资产地址：%w", res.Ref.URL, err)
		}
		rel, err := url.Parse(asset)
		if err != nil {
			return "", fmt.Errorf("资产名 %q 没法当成相对地址：%w", asset, err)
		}
		return base.ResolveReference(rel).String(), nil
	}
	var u, what string
	if script {
		u, what = res.Host.RawURL(res.Owner, res.Repo, res.Tag, asset), "raw"
	} else {
		u, what = res.Host.ReleaseURL(res.Owner, res.Repo, res.Tag, asset), "release"
	}
	if u == "" {
		return "", fmt.Errorf("托管方 %q 没配 %s 模板，拼不出下载地址。"+
			"在 ~/.toy/config.toml 的 [hosts.%s] 里补上 %s = \"…\"",
			res.HostName, what, res.HostName, what)
	}
	return u, nil
}

// insUnpack 把一个下载好的文件放进包目录。
// 铁律 1、2：只有解压和 chmod，没有任何一步会执行包里的东西。
func insUnpack(tf *toyfile.Toyfile, it insItem, pkgDir string) error {
	switch {
	case tf.IsScript():
		// 单文件脚本：下载 + chmod 0755 + 软链，零解压零构建（规格 §3）。
		return insCopy(it.path, filepath.Join(pkgDir, it.name), 0o755)
	case archive.IsArchive(it.asset):
		return insExtract(it.path, it.asset, pkgDir)
	default:
		// 裸二进制。[bin] 的值要写成这个文件名，写错了 store.Link 会说清楚。
		return insCopy(it.path, filepath.Join(pkgDir, filepath.Base(it.asset)), 0o755)
	}
}

// insExtract 解压进 pkgDir。
//
// archive 是按【文件名后缀】认格式的，而缓存里的文件名就是内容 hash、没有后缀
// （规格 §6：文件名即 sha256，所以缓存天然是个镜像目录）—— 直接把缓存路径交给它
// 一律是「不认识这个压缩格式」。所以先在临时目录里建一条带原资产名的软链。
func insExtract(src, asset, pkgDir string) error {
	tmp, err := os.MkdirTemp("", "toy-extract-")
	if err != nil {
		return fmt.Errorf("建不了临时目录：%w", err)
	}
	defer os.RemoveAll(tmp)

	named := filepath.Join(tmp, filepath.Base(asset))
	if err := os.Symlink(src, named); err != nil {
		return fmt.Errorf("准备解压失败：%w", err)
	}
	if _, err := archive.Extract(named, pkgDir); err != nil {
		return err
	}
	return nil
}

// insCopy 把下载下来的文件复制进包目录。
// 不用硬链接：缓存文件是 0644 而且被 `toy serve` 当镜像对外提供，
// 共用一个 inode 意味着这里的 chmod +x 会改到缓存那份上去。
func insCopy(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读不了下载下来的文件 %s：%w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("写不了 %s：%w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("写 %s 失败：%w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("写 %s 失败：%w", dst, err)
	}
	// O_CREATE 那个 perm 会被 umask 削掉，umask 022 下就是 0755，
	// 但 umask 077 下就成了 0700 —— 而这个文件是要给用户跑的。
	return os.Chmod(dst, perm)
}

// insLink 给每个命令建软链，一条失败就把这次建的全撤掉。
// 半套命令留在 PATH 上比一条都没有更糟：跑起来才发现少一个。
func insLink(st *store.Store, tf *toyfile.Toyfile, pkgDir string, old *store.Manifest, force bool) ([]string, error) {
	targets := map[string]string{}
	if tf.IsScript() {
		for name := range tf.Script {
			targets[name] = name // 脚本就落在包目录里，名字就是命令名
		}
	} else {
		maps.Copy(targets, tf.Bin)
	}

	// 升级/重装时，同名软链本来就是我们自己上一次建的 —— 替换它不需要用户
	// 再确认一次。别的名字照旧走遮蔽检查（铁律 5）。
	owned := map[string]bool{}
	if old != nil {
		for _, l := range old.Links {
			owned[filepath.Base(l)] = true
		}
	}

	var links []string
	for _, name := range slices.Sorted(maps.Keys(targets)) {
		target := filepath.Join(pkgDir, filepath.FromSlash(targets[name]))
		// zip 常常不带执行位，tar 里也见过 0644 的二进制。链上去之前补一下，
		// 否则装完是一条「存在但跑不了」的命令。
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 == 0 {
			os.Chmod(target, fi.Mode().Perm()|0o755)
		}
		lp, err := st.Link(name, target, force || owned[name])
		if err != nil {
			for _, done := range links {
				if uerr := st.Unlink(done); uerr != nil {
					ui.Warn("回滚时删不掉 %s：%v", done, uerr)
				}
			}
			// 只有真的建过链接才可能把上一版的顶掉；一条都没建就闭嘴，
			// 不然一次干净利落的拒绝看起来像是把已装的包弄坏了。
			if old != nil && len(links) > 0 {
				ui.Warn("%s 之前那一版的软链可能已经被换掉了，重跑一次 `toy install` 或 `toy update` 就能补回来", old.Name)
			}
			return nil, err
		}
		links = append(links, lp)
	}
	return links, nil
}

// insCleanOld 收拾被替换掉的那一版：新版不再提供的命令、以及旧的包目录。
// 全是 best-effort —— 新版已经装好并写进清单了，清不干净不该让安装失败。
func insCleanOld(st *store.Store, old *store.Manifest, pkgDir string, links []string) {
	if old == nil {
		return
	}
	for _, l := range old.Links {
		if slices.Contains(links, l) {
			continue
		}
		if err := st.Unlink(l); err != nil {
			ui.Warn("旧版留下的 %s 删不掉：%v", l, err)
		}
	}
	if old.PkgDir == "" || old.PkgDir == pkgDir {
		return
	}
	if err := insRemovePkgDir(st, old.PkgDir); err != nil {
		ui.Warn("旧版的包目录清不掉：%v", err)
	}
}

// insTOFU 做一次 TOFU 检查（规格 §4，模型抄 SSH 的 known_hosts）。
// 变了就停下来，除非 --force —— 而且必须把旧值新值都打出来，用户才有得判断。
func insTOFU(ring *sign.Ring, id, kind, value, what string, force bool) error {
	trust, old, err := ring.Check(id, kind, value)
	if err != nil {
		return fmt.Errorf("读不了 TOFU 记录（%s/%s）：%w", kind, id, err)
	}
	if trust != sign.TrustChanged {
		return nil
	}
	if !force {
		// 一行写完：ui 会把换行转义成 \x0a（它必须这么做 —— 能插换行就能把
		// 信任等级那一行盖掉重写，铁律 8），所以这里的 \n 只会变成噪音。
		return fmt.Errorf("%w：%s 的 %s 和首次安装时钉住的不一样 —— 钉住的是 %s，"+
			"这次拿到的是 %s。这可能是作者重发了同一个版本，也可能是内容被人换掉了；"+
			"自己核对过再加 --force 覆盖", sign.ErrChanged, id, what, old, value)
	}
	ui.Warn("%s 的 %s 变了（钉住的 %s → 这次 %s），--force 已放行", id, what, old, value)
	return nil
}

// insExisting 读这个包已经装着的那一份清单。没装过返回 nil，不是错误。
func insExisting(st *store.Store, id string) (*store.Manifest, error) {
	m, err := st.LoadManifest(id)
	if errors.Is(err, store.ErrNotInstalled) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// insRemovePkgDir 删一个包目录，删之前确认它真的在 ~/.toy 里面。
// 清单是磁盘上的文件，一个被改坏的 pkg_dir 不能变成「删 /」的授权
// （store.Unlink 对软链就是这么防的，这里补齐同一条）。
func insRemovePkgDir(st *store.Store, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("%q 没法转成绝对路径：%w", dir, err)
	}
	root := filepath.Clean(st.Root)
	if abs != root && !strings.HasPrefix(filepath.Clean(abs), root+string(os.PathSeparator)) {
		return fmt.Errorf("拒绝删 %s —— 它不在 %s 里，toy 只删自己装出来的东西", abs, root)
	}
	if abs == root {
		return fmt.Errorf("拒绝删 %s —— 那是整个 toy 目录，不是一个包", abs)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("删不掉包目录 %s：%w", abs, err)
	}
	return nil
}

// insPackageID 是这个包在 installed/ 和钥匙环里的身份，跨版本不变。
func insPackageID(res *resolved) string {
	if res.Ref.Kind == ref.KindURL {
		return res.Ref.URL
	}
	return res.HostName + ":" + res.Owner + "/" + res.Repo
}

// insIndexID 是这个包在索引里的 id，没经过索引就是空串。
func insIndexID(res *resolved) string {
	if res.Entry == nil {
		return ""
	}
	return res.Entry.ID
}

// insTrustID 是 TOFU 钥匙环里 sha256 那一维的 id。
//
// 铁律 6：某个 ref@version 的 hash 一旦记下就永不改指向 —— 所以它必须带上解析
// 出来的那个 tag。只写 owner/repo 的话，正常升级到下一版会被报成「哈希变了」，
// 而那句警告一旦天天出现就没人再当回事。
func insTrustID(res *resolved) string {
	id := insPackageID(res)
	if res.Tag == "" {
		return id
	}
	return id + "@" + res.Tag
}

// insVersion 是给 {version} 占位符用的版本号。裸地址那条路没有版本，返回空串
// —— 编一个出来就是撒谎（铁律 8），而 resolve 已经保证那条路上没有占位符。
func insVersion(res *resolved) string {
	if res.Tag == "" {
		return ""
	}
	return res.Version.Number()
}

// insVersionLabel 是清单里和界面上显示的版本。
func insVersionLabel(res *resolved) string {
	if v := insVersion(res); v != "" {
		return v
	}
	return insNoVersion
}

func insDirVersion(res *resolved) string { return insVersionLabel(res) }

// insHostSeg / insOwnerSeg 是 pkg/<host>/<owner>/… 那两段。裸地址没有托管方也
// 没有 owner，用 url/<域名> 顶上，免得所有裸地址装的包挤在同一个目录里。
func insHostSeg(res *resolved) string {
	if res.Ref.Kind == ref.KindURL {
		return "url"
	}
	return res.HostName
}

func insOwnerSeg(res *resolved) string {
	if res.Ref.Kind != ref.KindURL {
		return res.Owner
	}
	if u, err := url.Parse(res.Ref.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return "unknown"
}

// insTrustDetail 是安装完那一行信任等级后面跟的那段（规格 §4）。
func insTrustDetail(res *resolved) string {
	switch res.Trust {
	case store.TrustSigned:
		return fmt.Sprintf("%s (key %s)", res.Owner, res.Signer)
	case store.TrustHost:
		// 想要的是 "github.com/Derrity" —— 用户信的是那个托管方上的那个账号。
		domain := res.HostName
		if u, err := url.Parse(res.Host.WebURL(res.Owner, res.Repo)); err == nil && u.Host != "" {
			domain = u.Host
		}
		return domain + "/" + res.Owner
	default:
		return ""
	}
}

// insSafeAsset 挡住展开占位符之后才出现的危险路径。{tag} 是上游给的，一个叫
// "../../x" 的 tag 能把下载地址指到别处去，而安装完那一行仍然如实写着作者的
// 名字（铁律 7、8）。[artifact] 那条路 toyfile.Asset 已经验过了，这里管 [script]。
func insSafeAsset(name, asset string) error {
	switch {
	case strings.TrimSpace(asset) == "":
		return fmt.Errorf("[script] 里 %q 展开之后是空的，装不了", name)
	case strings.HasPrefix(asset, "/"), filepath.IsAbs(asset):
		return fmt.Errorf("[script] 里 %q 展开成绝对路径 %q，只能写仓库内的相对路径", name, asset)
	case strings.ContainsAny(asset, "?#"):
		return fmt.Errorf("[script] 里 %q 展开成 %q，含 ? 或 #，拼进地址后会取到另一个文件", name, asset)
	}
	for _, seg := range strings.Split(filepath.ToSlash(asset), "/") {
		if seg == ".." {
			return fmt.Errorf(`[script] 里 %q 展开成 %q，含 ".." 段，会指到仓库外面去`, name, asset)
		}
	}
	return nil
}

func insBaseNames(links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, filepath.Base(l))
	}
	return out
}

// insParseFlags 解析子命令的 flag。flag 包自己那套用法排版跟 main 的重复，
// 所以把它的输出扔掉，只留一句具体错误，用法交给 main 打（errUsage）。
func insParseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			ui.Error("%v", err)
		}
		return errUsage
	}
	return nil
}
