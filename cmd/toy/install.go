package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/platform"
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
//
// 两个开关，两件事，不许再合并：
//
//	--reinstall  已经装了这一版，再装一次。日常操作，不放行任何检查。
//	--force      接受安全检查报出的变化（哈希 / 公钥 / 遮蔽 / 信任降级 / 来源）。
//	             真的放行时逐条打印放行了什么。
//
// 解析 ref → (索引查 source) → git ls-remote 拿 tag → 挑最高 semver →
// raw URL 取 toy.toml → 按 os/arch 挑资产 → 下载(带镜像回退) → 验 sha256 →
// TOFU 检查 → 解压或 chmod+x → 软链(拒绝遮蔽) → 写清单 → 打印信任等级
func runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	// 两个开关说的是两件事，别再合并回一个（见 insPerform 上面那段注释）：
	// --reinstall 是日常操作，--force 是「我核对过，放行安全检查报出的变化」。
	reinstall := fs.Bool("reinstall", false, "已经装了这一版，再装一次（不放行任何安全检查）")
	force := fs.Bool("force", false, "接受安全检查报出的变化：哈希变了 / 作者公钥换了 / 命令名遮蔽 / 信任等级降级 / 来源变了")
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

	old, err := insExisting(st, res)
	if err != nil {
		return err
	}
	// 已经是这一版就别再下一遍。--reinstall 是「我知道，再装一次」。
	// old.ID 和这次解析出来的不一样时不能走这条捷径 —— 那说明来源变了，
	// tag 撞上了也不代表装的是同一个东西。
	if old != nil && old.ID == insPackageID(res) && !*reinstall && res.Tag != "" && old.Tag == res.Tag {
		hint := "要重装加 --reinstall"
		if *force {
			// --force 不再顺带触发一次重装。光给 --force 就在这里静默返回的话，
			// 用户会以为他放行的那件事已经生效了。
			hint = "--force 只放行安全检查报出的变化，它自己不会触发一次重装 —— 要重装再加 --reinstall"
		}
		ui.Info("%s 已经是 %s 了，没什么可做的（%s）", old.Name, old.Tag, hint)
		return nil
	}
	// 降级是合法的 —— `toy install foo@v1.0.0` 装 v1.0.0 是用户明说的，不拦。
	// 但不能一声不吭：上游把最新那个 tag 删掉（账号被接管、release 被撤、
	// 自建 host 被换掉）时走的是同一条路，而那时输出里只有一句「已安装 t 1.0.0」。
	// 铁律 8 —— 如实说，让用户自己判断这次降级是不是他要的。
	if old != nil && insIsOlder(*old, res) {
		ui.Warn("这是一次降级：%s 已经装着 %s，这次要装的 %s 比它旧。"+
			"要是你没打算降级，先确认上游那个新 tag 是不是被撤掉了", old.Name, old.Tag, res.Tag)
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

// insIsOlder 判断这次要装的 tag 是不是比已装的那一版旧。
//
// 和 insIsNewer 不是互为反面：两边只要有一个不是合法 semver 就没有大小可言，
// 那时候一句「这是一次降级」是猜的，而猜错了比不说更糟 —— 所以这里返回 false，
// 而 insIsNewer 在同样的情况下按「tag 变了就是有新版」放行。
func insIsOlder(m store.Manifest, res *resolved) bool {
	newV, nerr := semver.Parse(res.Tag)
	oldV, oerr := semver.Parse(m.Tag)
	if nerr != nil || oerr != nil {
		return false
	}
	return semver.Compare(newV, oldV) < 0
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
//
// force 只有一个含义：「安全检查报出的变化，我核对过了，放行」。
// 「重装同一个版本」是 --reinstall，它到不了这里 —— 那是日常操作，不该逼用户
// 举起一把同时能放行「这个版本换了内容」和「作者换了钥匙」的锤子，
// 而那两件正是 TOFU 存在的全部意义（规格 §4、铁律 4/6）。
//
// force 真的放行了什么，必须逐条打印（铁律 8）：一次静默的放行等于没有检查。
func insPerform(ctx context.Context, st *store.Store, cfg store.Config, res *resolved, old *store.Manifest, force bool) error {
	if insTrustDowngraded(old, res) {
		if !force {
			return fmt.Errorf("上一次装 %s 的时候它是已签名的（key %s），这次却验不出签名 —— "+
				"索引里那条记录可能被摘掉了 sig，也可能你这次是从别的路径装的。"+
				"上面那行警告说了具体原因；自己核对过再加 --force 继续", old.Name, old.Signer)
		}
		ui.Warn("--force 放行：%s 上一次是已签名的（key %s），这次验不出签名，信任等级降了一档",
			old.Name, old.Signer)
	}
	if insSourceChanged(old, res) {
		if !force {
			return fmt.Errorf("%s 上一次是从 %s 装的，这次解析出来的来源是 %s —— "+
				"索引条目只是个指针，改一行 source 就能把已装的命令换成另一个仓库的内容。"+
				"确认过这次的来源是你要的，再加 --force 继续", old.Name, old.ID, insPackageID(res))
		}
		ui.Warn("--force 放行：%s 的来源从 %s 换成了 %s", old.Name, old.ID, insPackageID(res))
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
		// 原始地址挂了、字节是从镜像拿到的时候必须说一声（铁律 8）。内容是 hash
		// 寻址的，所以镜像不需要被信任（规格 §8）—— 但「不需要信任」不等于
		// 「不需要知道」，用户有权知道这些字节到底是从哪来的。
		// fetch 只在有 sha256 可比的时候才会去试镜像，所以走到这里必然已经比过了。
		if dl.Source != it.url && !dl.FromCache {
			// 镜像地址的最后一段就是 sha256，不必再把它重复一遍。
			ui.Info("%s：原始地址没拿到，这份字节来自镜像 %s（sha256 已核对，和 toy.toml 声明的一致）",
				it.name, dl.Source)
		}

		// 资产名无条件拼进 TOFU id。它天然就是平台判别符 —— 不同平台的资产名
		// 本来就不一样（linux_amd64.tar.gz vs linux_amd64_musl.tar.gz），而同一个
		// ref@version 在 musl 机器和 glibc 机器上拿到的是两份不同的字节。不拼的话，
		// 换一台机器（或者共享 $HOME）会被报成「内容被人换掉了」——
		// 一个吓人的假警报，正好出现在最不该狼来了的地方。
		// 两个平台指向同一个资产名时它们本来就该共用一条钉住记录，这里也是对的。
		id := trustID + "/" + it.name
		if err := insTOFU(ring, id, "sha256", it.sum, it.name+" 的 sha256", force); err != nil {
			return err
		}
		pins = append(pins, pin{id, "sha256", it.sum})
	}
	// 索引说这个包有作者公钥的话，公钥也钉住（规格 §4：公钥和 artifact hash
	// 首次安装都要钉）。换了一把钥匙就是换了一个"作者"，那正是要拦的事。
	if res.Entry != nil && res.Signer != "" {
		key := strings.TrimSpace(res.Entry.Key)
		if err := insTOFU(ring, insKeyID(res), "key", key, "作者公钥", force); err != nil {
			return err
		}
		pins = append(pins, pin{insKeyID(res), "key", key})
	}

	// 解压和软链全部成功才动正在服役的那一版：先解压进兄弟目录，成了再把旧的
	// 挪开、新的顶上去，软链失败就把旧的挪回来。就地 RemoveAll 再解压的话，
	// 一个投毒或者损坏的上游配上一次很正常的 --force 重装，就是一条「拆掉用户
	// 手上那个命令」的原语：包目录空了、软链悬空，而清单还在、toy list 照样列它。
	pkgDir := st.PkgDir(insHostSeg(res), insOwnerSeg(res), insNameSeg(res, tf), insDirVersion(res))
	staged := pkgDir + ".new"
	if err := insRemovePkgDir(st, staged); err != nil {
		return err
	}
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return fmt.Errorf("建不出包目录 %s：%w", staged, err)
	}
	for _, it := range items {
		if err := insUnpack(tf, it, staged); err != nil {
			os.RemoveAll(staged) // 半个包比没有包更难查
			return err
		}
	}
	backup, err := insSwapIn(st, staged, pkgDir)
	if err != nil {
		return err
	}

	links, err := insLink(st, tf, pkgDir, old, force)
	if err != nil {
		insRestore(st, backup, pkgDir)
		return err
	}
	_ = insRemovePkgDir(st, backup)

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
	// 来源变了的话新清单是另一个 id，旧的那份不删就会变成一条指着已删目录的
	// 幽灵记录：`toy list` 里同一个包出现两次，`toy uninstall <名字>` 报
	// 「对上了 2 个已装的包」，而 doctor 给的修复建议会把搬走的那个仓库再装一遍。
	if old != nil && old.ID != m.ID {
		if err := st.RemoveManifest(old.ID); err != nil && !errors.Is(err, store.ErrNotInstalled) {
			ui.Warn("旧清单 %s 删不掉：%v（`toy list` 会重复列出这个包）", old.ID, err)
		}
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

	cands := platform.Candidates()
	asset, want, matched, err := tf.AssetFor(cands, version, res.Tag)
	if err != nil {
		return nil, err
	}
	// 命中的不是第一个候选 = 可能装的不是「为这台机器构建的那个」。铁律 8：如实说。
	// 要作者声明了哪些平台才判得准，见 insFallbackNote。
	if note := insFallbackNote(cands, matched, tf.Platforms()); note != "" {
		ui.Warn("%s", note)
	}
	u, err := insAssetURL(res, asset, false)
	if err != nil {
		return nil, err
	}
	return []insItem{{name: asset, asset: asset, url: u, want: want}}, nil
}

// insFallbackNote 在命中的不是第一个候选时给出一句人话。命中第一个就返回空串。
//
// 铁律 8：装的不是「为这台机器构建的那个」，用户必须知道 —— 这两种回退失败起来
// 都不像回退的错：Rosetta 装了一个默默慢一截的 Intel 版；把 glibc 构建丢进 Alpine
// 报的是 `not found`，而文件明明就在那儿。
//
// 但「没命中首选候选」并不等于「没拿到为这台机器构建的东西」：在 linux 上我们
// **总会**自己合成一段 libc 后缀当首选（gnu / musl），而绝大多数项目只发两段 key。
// 于是每一次普通安装都会多出一句假警告，把真正要紧的 musl / Rosetta 警告一起淹掉 ——
// 一条每次都响的警告等于没有警告。所以 declared（作者声明了哪些平台）是判据的一部分：
// 裸 linux/amd64 差不多总是 glibc 构建，对 glibc 机器来说它就是对的那个，除非作者
// 自己按 libc 分了档（那时通用档是什么就说不准了）。musl 机器反过来永远要说 ——
// 裸 linux/amd64 几乎不可能是 musl 构建。
func insFallbackNote(cands []string, matched string, declared []string) string {
	if len(cands) == 0 || matched == "" || matched == cands[0] {
		return ""
	}
	want := cands[0]
	if want == matched+"/gnu" && !insSplitsBy(matched, declared) {
		return ""
	}
	switch {
	case want == "darwin/arm64" && matched == "darwin/amd64":
		return "这个包没有 Apple Silicon 的构建，装的是 Intel 版（darwin/amd64）—— " +
			"它靠 Rosetta 2 翻译运行，能跑但会慢一截。没装 Rosetta 2 的话敲下去会直接报「无法打开」，" +
			"`softwareupdate --install-rosetta` 装上即可"
	case want == matched+"/gnu":
		// 走到这里说明作者确实按 libc 分了档，只是没有 glibc 那一档。
		return "这个包按 libc 分了构建，但没有 " + want + " 这一档，装的是通用的 " + matched +
			" —— 通用构建通常就是 glibc 版，只是作者没有明说"
	case strings.HasSuffix(want, "/musl"):
		return "这台机器是 musl（Alpine 这类），而这个包只有通用 Linux 构建（" + matched + "）。" +
			"它要是动态链接的 glibc 程序，在这里根本起不来 —— 而报错会长得像「文件不存在」" +
			"（`not found` / `no such file or directory`），那其实是找不到动态加载器，不是文件没装上"
	default:
		return "没有 " + want + " 的构建，退回到 " + matched + " —— 装上了但不是为这台机器专门构建的那个"
	}
}

// insSplitsBy 判断作者有没有在 base 这个底座上按更细的一维分构建
// （linux/amd64 之下的 linux/amd64/musl 这种）。分了才说明通用档不再是「默认那个」。
func insSplitsBy(base string, declared []string) bool {
	for _, p := range declared {
		if strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
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
		// 先按「不放行」试一次：这样 --force 到底顶掉了什么，能照着 store 自己
		// 的措辞原样说出来，而不用在这里把遮蔽检查重抄一遍（铁律 8）。
		// owned[name] 是我们上一次自己建的那条链，换掉它不是安全放行。
		lp, err := st.Link(name, target, owned[name])
		if err != nil && force && (errors.Is(err, store.ErrShadow) || errors.Is(err, store.ErrLinkExists)) {
			ui.Warn("--force 放行：%v", err)
			lp, err = st.Link(name, target, true)
		}
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
	ui.Warn("--force 放行：%s 的 %s 变了（钉住的 %s → 这次 %s）", id, what, old, value)
	return nil
}

// insExisting 读这个包已经装着的那一份清单。没装过返回 nil，不是错误。
//
// 先按包 id 找；找不到再按索引条目的 id 找一遍 —— 索引里的 source 一变，
// insPackageID 跟着就变了，只按它查的话「上一次装的是什么」直接从视野里消失，
// 于是来源变更拦不住、旧清单也没人清。
func insExisting(st *store.Store, res *resolved) (*store.Manifest, error) {
	m, err := st.LoadManifest(insPackageID(res))
	if err == nil {
		return &m, nil
	}
	if !errors.Is(err, store.ErrNotInstalled) {
		return nil, err
	}
	idxID := insIndexID(res)
	if idxID == "" {
		return nil, nil
	}
	ms, err := st.ListManifests()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.IndexID == idxID {
			return &m, nil
		}
	}
	return nil, nil
}

// insSourceChanged 判断这次装的东西是不是换了一个来源。
//
// 索引条目是一个谁都能提 PR 的 JSON 里的一行指针。把 source 从
// github:alice/tool 改成 github:evil/tool，`toy update` 就会照着新指针重新解析，
// 而三道闸一道都不响：两边都没签名时 insTrustDowngraded 恒为 false，
// TOFU 的 id 带着 source 一起变成了一条全新记录，信任那一行也照旧。
// 所以来源变更和信任降级要一样对待 —— 一次升级不该悄悄换掉用户信的是谁。
func insSourceChanged(old *store.Manifest, res *resolved) bool {
	return old != nil && old.ID != "" && old.ID != insPackageID(res)
}

// insSwapIn 把解压好的 staged 顶成 pkgDir，返回被挪开的旧目录（没有就是空串）。
func insSwapIn(st *store.Store, staged, pkgDir string) (string, error) {
	backup := ""
	if _, err := os.Stat(pkgDir); err == nil {
		backup = pkgDir + ".old"
		if err := insRemovePkgDir(st, backup); err != nil {
			os.RemoveAll(staged)
			return "", err
		}
		if err := os.Rename(pkgDir, backup); err != nil {
			os.RemoveAll(staged)
			return "", fmt.Errorf("挪不开旧的包目录 %s：%w", pkgDir, err)
		}
	}
	if err := os.Rename(staged, pkgDir); err != nil {
		insRestore(st, backup, pkgDir)
		os.RemoveAll(staged)
		return "", fmt.Errorf("装不进包目录 %s：%w", pkgDir, err)
	}
	return backup, nil
}

// insRestore 把 insSwapIn 挪开的那一版放回去。best-effort：这条路上已经有一个
// 错误要报了，再报一个只会盖住真正的原因。
func insRestore(st *store.Store, backup, pkgDir string) {
	if backup == "" {
		return
	}
	_ = insRemovePkgDir(st, pkgDir)
	if err := os.Rename(backup, pkgDir); err != nil {
		ui.Warn("旧的那一版放不回 %s：%v（重跑一次 `toy install` 就能补回来）", pkgDir, err)
	}
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

// insKeyID 是 TOFU 钥匙环里 "key" 那一维的 id。
//
// 必须用索引条目的 id：作者公钥只在索引条目里出现，而 insPackageID 是从那条
// 记录的 source 推出来的 —— 改一行 source 就等于换了一个身份，于是「作者公钥
// 换了一把」这件事根本不会被 Check 看到，而规格 §4 要求公钥变了必须停下来。
func insKeyID(res *resolved) string {
	if id := insIndexID(res); id != "" {
		return id
	}
	return insPackageID(res)
}

// insNameSeg 是 pkg/<host>/<owner>/<这一段>/<version> 里的那一段。
//
// 不能直接用 toy.toml 里的 name：那是作者随便写的，同一个 owner 下的
// tool-rs 和 tool-go 都写 name = "tool" 完全合法，于是两个包共用一个安装目录
// —— 装后一个会静默删掉前一个的文件（而前一个的清单和软链都还在），
// 卸后一个也会把前一个的目录一起删走。仓库名在一个 owner 下天然唯一；
// 裸地址没有仓库名，只能靠地址本身的 hash 区分。
func insNameSeg(res *resolved, tf *toyfile.Toyfile) string {
	if res.Ref.Kind != ref.KindURL && res.Repo != "" {
		return res.Repo
	}
	sum := sha256.Sum256([]byte(insPackageID(res)))
	return tf.Name + "-" + hex.EncodeToString(sum[:])[:8]
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
		// 绝不把仓库 owner 印在勾后面。签名消息是 id + source + toy.toml 原文，
		// 三样都是公开可读的，里面没有一个字节把这把公钥和那个 owner 绑起来
		// —— 任何人都能拿自己的钥匙签别人那条还没认领的记录，而
		// "Derrity (key …)" 读起来就是「Derrity 签的」（铁律 8）。
		// 这个勾能说的只有：索引里登记的这把钥匙，签过这一版的 toy.toml。
		detail := "key " + res.Signer
		if res.IndexName != "" {
			detail += " · 索引 " + res.IndexName + " 登记"
		}
		if who := qryHostLabel(res); who != "" && who != "/" {
			detail += "（不代表 " + who + " 本人）"
		}
		return detail
	case store.TrustHost:
		// 想要的是 "github.com/Derrity" —— 用户信的是那个托管方上的那个账号。
		return qryHostLabel(res)
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
		if errors.Is(err, flag.ErrHelp) {
			// -h 原来只落到 main 那一行语法串上，每个选项到底是什么意思一个字都没有。
			// 而 install 的 --reinstall 和 --force 恰恰是必须在这儿讲清楚的一对：
			// 一个是日常操作，一个是「我核对过，放行安全检查报出的变化」。
			// main 紧接着会打那行用法串，所以这里只补选项。
			fmt.Fprintf(os.Stderr, "toy %s 的选项（写在位置参数前面）:\n", fs.Name())
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
		} else {
			ui.Error("%v", err)
		}
		return errUsage
	}
	return nil
}
