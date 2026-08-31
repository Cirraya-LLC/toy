package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
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
	Raw      []byte       // toy.toml 的原始字节，作者签名覆盖的就是它（见 autSignMessage）
	Entry    *index.Entry // 经索引来的才有，否则 nil
	Trust    string       // store.TrustSigned / TrustHost / TrustUnverified
	Signer   string       // 公钥指纹，未签名为空
}

// rsvTagsTimeout 是 `git ls-remote` 的上限。主进程的 ctx 只在 Ctrl-C 时取消，
// 而一个不回话的托管方会让它挂到天荒地老（host.Tags 的注释要求必须带超时）。
const rsvTagsTimeout = 60 * time.Second

// rsvMaxTagsShown 是「没有你要的那个版本，现有的是这些」里最多列几个。
const rsvMaxTagsShown = 20

// openStore 打开 ~/.toy 并读配置，是每个子命令的第一行。
func openStore() (*store.Store, store.Config, error) {
	st, err := store.Open("")
	if err != nil {
		return nil, store.Config{}, err
	}
	// LoadConfig 在文件不存在时会返回内置默认，但那份默认是看不见的 ——
	// 用户第一次跑任何命令都该在磁盘上得到一个能直接改的 config.toml
	// （加索引、加镜像、加自建托管方都要改它）。写不出来不致命，警告一声照跑。
	if _, err := os.Stat(filepath.Join(st.Root, "config.toml")); errors.Is(err, os.ErrNotExist) {
		if err := st.SaveConfig(store.DefaultConfig()); err != nil {
			ui.Warn("写不出默认配置：%v（这次先用内置默认）", err)
		}
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, store.Config{}, err
	}
	return st, cfg, nil
}

// loadIndexes 按配置顺序加载全部索引（走 fetch，所以 file:// 和 https:// 都行，
// 并缓存到 ~/.toy/index/）。单个索引失败只警告不中断 —— 一个索引挂了
// 不该让另一个索引里的包也搜不到。
func loadIndexes(ctx context.Context, st *store.Store, cfg store.Config) ([]*index.Index, error) {
	var out []*index.Index
	failed := 0
	for _, spec := range cfg.Indexes {
		url := strings.TrimSpace(spec.URL)
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = url
		}
		if url == "" {
			ui.Warn("配置里有一条索引没写 url（name = %q），跳过", spec.Name)
			failed++
			continue
		}

		cache := rsvIndexCache(st, name)
		data, err := fetch.GetBytes(ctx, url, fetch.Options{Name: name})
		switch {
		case err == nil:
			rsvWriteCache(cache, data, name)
		case ctx.Err() != nil:
			// Ctrl-C：别再挨个去试后面的索引，也别拿缓存假装成功。
			return nil, ctx.Err()
		default:
			cached, cerr := os.ReadFile(cache)
			if cerr != nil {
				ui.Warn("索引 %s 拉不到（%v），本地也没有缓存 —— 这次搜不到它里面的包", name, err)
				failed++
				continue
			}
			ui.Warn("索引 %s 拉不到（%v），用的是 %s 之前缓存的索引", name, err, rsvCacheAge(cache))
			data = cached
		}

		idx, skipped, err := index.Load(data, name)
		if err != nil {
			ui.Warn("索引 %s 用不了：%v", name, err)
			failed++
			continue
		}
		// 静默跳过等于骗人：用户搜不到某个包时，得知道是索引里那条记录坏了。
		if skipped > 0 {
			ui.Warn("索引 %s 里有 %d 条记录不合法，已跳过（id 不是 owner/repo 形态、"+
				"source 解析不了或者又指回索引、id 重复）", name, skipped)
		}
		out = append(out, idx)
	}
	// 一个都没成，就不能装作「搜不到这个包」——那句话是假的。
	if len(out) == 0 && failed > 0 {
		return nil, fmt.Errorf("配置里的 %d 个索引一个都用不了（原因在上面）。"+
			"`toy install github:owner/repo` 这条路不经过索引，仍然能装", failed)
	}
	return out, nil
}

// rsvIndexCache 是一个索引在 ~/.toy/index/ 下的缓存路径。
//
// 名字是用户在 config.toml 里随便写的，可能带斜杠和冒号，所以要转义；而转义
// 是多对一的（"a/b" 和 "a:b" 都变成 "a_b"），两个索引共用一份缓存就会在离线时
// 拿出另一个索引的内容。抄 store.safeSeg 的做法：动过就缀上原串的 hash。
func rsvIndexCache(st *store.Store, name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if strings.Trim(slug, ".") == "" {
		slug = "_"
	}
	if slug != name {
		sum := sha256.Sum256([]byte(name))
		slug += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return filepath.Join(st.IndexDir(), slug+".json")
}

// rsvWriteCache 落一份索引缓存。同目录临时文件 + rename：Ctrl-C 或者磁盘满
// 留下半个 JSON 的话，下次离线回退读到的就是一个永远修不好的坏缓存。
func rsvWriteCache(path string, data []byte, name string) {
	f, err := os.CreateTemp(filepath.Dir(path), ".idx-*")
	if err == nil {
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), path)
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}
	if err != nil {
		// 缓存不下来只影响下次离线，这次照用。
		ui.Warn("索引 %s 缓存不下来：%v", name, err)
	}
}

func rsvCacheAge(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "不知道多久"
	}
	return time.Since(fi.ModTime()).Round(time.Minute).String()
}

// resolve 执行规格 §2 的第 1–3 步：
//
//	KindIndex → 查索引拿到 source，再按 KindHost 走下去；查不到就报错并建议用 github: 直接装
//	KindHost  → git ls-remote 拿 tag，挑最高 semver（r.Version 非空则用它），
//	            再用 raw URL 模板取那个 tag 上的 toy.toml
//	KindURL   → 直接取那个 toy.toml，版本无从得知
//
// 同时定出 Trust 等级（规格 §4）。不下载 artifact，不碰磁盘上的 pkg 目录。
func resolve(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref) (*resolved, error) {
	switch r.Kind {
	case ref.KindIndex:
		return rsvViaIndex(ctx, st, cfg, r)
	case ref.KindHost:
		return rsvViaHost(ctx, cfg, r)
	case ref.KindURL:
		return rsvViaURL(ctx, r)
	default:
		return nil, fmt.Errorf("不认识的包引用形态：%q", r.Raw)
	}
}

// rsvViaIndex 查索引拿到 source，然后照 source 说的那条路走下去。
// 索引只是个目录 —— source 才是真相（规格 §5）。
func rsvViaIndex(ctx context.Context, st *store.Store, cfg store.Config, r ref.Ref) (*resolved, error) {
	idxs, err := loadIndexes(ctx, st, cfg)
	if err != nil {
		return nil, err
	}
	id := r.ID()
	for _, idx := range idxs {
		e, ok := idx.Find(id)
		if !ok {
			continue
		}
		src, err := ref.Parse(e.Source)
		if err != nil {
			return nil, fmt.Errorf("索引 %s 里 %s 的 source %q 解析不了：%w", idx.Name, e.ID, e.Source, err)
		}
		src.Version = r.Version // 用户写的 @version 说了算

		var res *resolved
		switch src.Kind {
		case ref.KindHost:
			res, err = rsvViaHost(ctx, cfg, src)
		case ref.KindURL:
			// 裸地址没有版本可挑，装不出用户点名的那一版；这时候按最新版装
			// 是在给他一个他没要的东西，宁可说清楚。
			if r.Version != "" {
				return nil, fmt.Errorf("索引 %s 里 %s 指向一个固定地址（%s），没有版本可选，"+
					"所以 @%s 装不了。找到它的仓库用 github:owner/repo@%s 这种写法装",
					idx.Name, e.ID, e.Source, r.Version, r.Version)
			}
			res, err = rsvViaURL(ctx, src)
		default:
			// index.Load 已经把这种记录当坏记录跳过了，走到这儿说明它变了。
			return nil, fmt.Errorf("索引 %s 里 %s 的 source 是 %q，那又是一次索引查询，是个环",
				idx.Name, e.ID, e.Source)
		}
		if err != nil {
			return nil, err
		}

		res.Entry = &e
		rsvApplySignature(idx.Name, e, res)
		return res, nil
	}

	// 索引查不到不代表装不了 —— github: 那条路从头到尾就是真相，这正是
	// 这个架构的意义（规格 §5：下架无害）。
	return nil, fmt.Errorf("索引里查不到 %s。这不代表它装不了：索引只是个目录，"+
		"托管方才是真相 —— 知道它在哪就直接写 `toy install github:%s`"+
		"（在别家就换成 gitlab: / codeberg: 等前缀）。`toy search %s` 也许能找到名字相近的",
		id, id, r.Repo)
}

// rsvViaHost 走规格 §2 的正路：ls-remote 拿 tag → 挑版本 → raw 取 toy.toml。
func rsvViaHost(ctx context.Context, cfg store.Config, r ref.Ref) (*resolved, error) {
	h, err := host.Lookup(r.Host, cfg.Hosts)
	if err != nil {
		return nil, err
	}
	hostName := h.Name
	if hostName == "" {
		hostName = strings.ToLower(r.Host)
	}

	// 版本发现是 `git ls-remote --tags`：一条命令，所有 git 托管方一致，
	// 不用 API、不用 token、不限流（规格 §2）。
	clone := h.CloneURL(r.Owner, r.Repo)
	tagCtx, cancel := context.WithTimeout(ctx, rsvTagsTimeout)
	defer cancel()
	tags, err := host.Tags(tagCtx, clone)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("%s 一个 tag 都没有 —— toy 靠 tag 发现版本。"+
			"如果这是你的仓库：打一个 v1.0.0 的 tag、发一版 release、加个 toy.toml 就能装了", clone)
	}
	v, err := rsvPickVersion(tags, r.Version)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", r.String(), err)
	}

	rawURL := h.RawURL(r.Owner, r.Repo, v.Raw, "toy.toml")
	if rawURL == "" {
		return nil, fmt.Errorf("托管方 %q 没配 raw 模板，取不了 toy.toml。"+
			"在 ~/.toy/config.toml 的 [hosts.%s] 里补上 raw = \"…/{owner}/{repo}/…/{ref}/{path}\"",
			hostName, hostName)
	}
	tf, raw, err := rsvGetToyfile(ctx, rawURL, fmt.Sprintf("%s:%s/%s@%s", hostName, r.Owner, r.Repo, v.Raw))
	if err != nil {
		return nil, err
	}

	return &resolved{
		Ref:      r,
		HostName: hostName,
		Host:     h,
		Owner:    r.Owner,
		Repo:     r.Repo,
		Tag:      v.Raw,
		Version:  v,
		Toyfile:  tf,
		Raw:      raw,
		// 信任锚点是托管方的账号体系，没有别的（规格 §4）。
		Trust: store.TrustHost,
	}, nil
}

// rsvViaURL 是裸地址那条路：什么都不信，也没有版本这回事。
func rsvViaURL(ctx context.Context, r ref.Ref) (*resolved, error) {
	tf, raw, err := rsvGetToyfile(ctx, r.URL, r.URL)
	if err != nil {
		return nil, err
	}
	// 这条路没有 tag，{version} / {tag} 展不开。硬展成空串会拼出一个
	// 长得像模像样、其实指向别的文件（甚至一张 404 页面）的下载地址，
	// 而那个字节流没有 hash 时会被 TOFU 原样钉住 —— 宁可现在说清楚。
	for _, m := range []map[string]string{tf.Artifact, tf.Script} {
		for k, val := range m {
			if strings.Contains(val, "{version}") || strings.Contains(val, "{tag}") {
				return nil, fmt.Errorf("裸地址装不了这个包：%s 的资产名是 %q，"+
					"带 {version}/{tag} 占位符，而一个 toy.toml 的地址里没有版本信息。"+
					"改用 `toy install github:owner/repo` 这种写法（那条路用 git tag 定版本），"+
					"或者请作者把资产名写死", k, val)
			}
		}
	}
	return &resolved{
		Ref:     r,
		Toyfile: tf,
		Raw:     raw,
		// Tag 空、Version 零值：无从得知，编一个出来就是撒谎（铁律 8）。
		Trust: store.TrustUnverified,
	}, nil
}

// rsvGetToyfile 取并解析一个 toy.toml。where 是给人看的位置说明。
func rsvGetToyfile(ctx context.Context, url, where string) (*toyfile.Toyfile, []byte, error) {
	data, err := fetch.GetBytes(ctx, url, fetch.Options{Name: "toy.toml"})
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// 绝大多数时候这就是 404：仓库还没接入 toy。绝不猜资产名、绝不构建（铁律 2）。
		return nil, nil, fmt.Errorf("%s 上没有 toy.toml：%w。"+
			"这个仓库多半还没接入 toy —— toy 只装 release 里的二进制或单文件脚本，"+
			"不会去猜资产名，也永远不从源码构建。如果这是你的仓库：在根目录加一个 8 行的 "+
			"toy.toml（name / [bin] / [artifact] 三段）就行，不用求任何人批准",
			where, err)
	}
	tf, err := toyfile.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s 的 toy.toml 有问题：%w", where, err)
	}
	// 原始字节要留着：作者的签名签的是「用户真正下载到的那份文件」，
	// 重新序列化一遍等于换了一份文本去验（见 autSignMessage 的注释）。
	return tf, data, nil
}

// rsvPickVersion 挑版本：用户指名了就找那个 tag，否则取最高的正式版。
func rsvPickVersion(tags []string, want string) (semver.V, error) {
	if want == "" {
		return semver.Max(tags, false)
	}
	for _, t := range tags {
		// "v1.2.3" 和 "1.2.3" 指的是同一个 tag，用户写哪种都认。
		if t == want || strings.TrimPrefix(t, "v") == strings.TrimPrefix(want, "v") {
			v, err := semver.Parse(t)
			if err != nil {
				// 用户指名了这个 tag 就按它装；不是 semver 只影响「谁更新」的比较，
				// 不影响这次安装。Raw 留着，{version} 靠它展开。
				return semver.V{Raw: t}, nil
			}
			return v, nil
		}
	}
	return semver.V{}, fmt.Errorf("没有 %q 这个版本。现有的是：%s", want, rsvShowTags(tags))
}

// rsvShowTags 把 tag 列表排成一句人看得懂的话：能解析的按版本从新到旧，
// 剩下的（release-2024 这类）排在后面。
func rsvShowTags(tags []string) string {
	sorted := slices.Clone(tags)
	slices.SortStableFunc(sorted, func(a, b string) int {
		va, ea := semver.Parse(a)
		vb, eb := semver.Parse(b)
		switch {
		case ea == nil && eb == nil:
			return semver.Compare(vb, va) // 新的在前
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(sorted) > rsvMaxTagsShown {
		return strings.Join(sorted[:rsvMaxTagsShown], "、") +
			fmt.Sprintf("…（共 %d 个）", len(sorted))
	}
	return strings.Join(sorted, "、")
}

// rsvApplySignature 决定这条索引记录到底算不算「已签名」（规格 §4、铁律 8）。
//
// 只有 key 不作数：索引是一个谁都能 fork、谁都能提 PR 的 JSON 文件，往条目里
// 抄一把公钥是零成本的。**验的是签名**：作者用私钥签过 id + source + 这一份
// toy.toml 的原文（autSignMessage），所以签名同时钉住了「谁」「指向哪」和
// 「装的是什么」—— 换掉其中任何一样都验不过。
//
// 验不过就降级成底下那一档（托管方账号 / 未验证）并说清楚原因，绝不打勾：
// 一个见到 key 就打勾的客户端，等于把「已签名」这三个字白送给任何一个能改
// 索引的人，而客户端是唯一的安全边界。
func rsvApplySignature(idxName string, e index.Entry, res *resolved) {
	key := strings.TrimSpace(e.Key)
	if key == "" {
		return // 没认领过名字，本来就不该有签名（签名是可选升级，不是入场券）
	}
	fp := rsvFingerprint(key)
	switch {
	case fp == "":
		ui.Warn("索引 %s 里 %s 的 key 不是一把合法的 ed25519 公钥，当作未签名处理", idxName, e.ID)
	case strings.TrimSpace(e.Sig) == "":
		ui.Warn("索引 %s 里 %s 有作者公钥却没有签名 —— 一把没签过任何东西的公钥证明不了什么，"+
			"当作未签名处理（作者跑一次 `toy publish` 就会带上 sig）", idxName, e.ID)
	default:
		if err := sign.Verify(key, autSignMessage(e.ID, e.Source, res.Raw), strings.TrimSpace(e.Sig)); err != nil {
			ui.Warn("索引 %s 里 %s 的签名验不过这一版的 toy.toml（%v），当作未签名处理 —— "+
				"可能是作者改了 toy.toml 却没重新 `toy publish`，也可能是索引里这条被人动过", idxName, e.ID, err)
			return
		}
		res.Trust, res.Signer = store.TrustSigned, fp
	}
}

// rsvFingerprint 把索引条目里的 hex 公钥变成界面上那个短指纹。
// 不是一把合法的 ed25519 公钥就返回空串 —— 调用方据此不声称已签名。
func rsvFingerprint(hexKey string) string {
	b, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return ""
	}
	return sign.Fingerprint(ed25519.PublicKey(b))
}
