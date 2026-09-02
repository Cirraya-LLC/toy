package main

// discovery 那一面：list / search / info / index / doctor。
//
// 排版有一条统一的规矩：**一切给人看的输出都走 ui**（也就是 stderr，
// 并且顺带过一遍控制字符转义）。索引里的 desc、toy.toml 里的字段都是外来文本，
// 直接 fmt.Fprintln 到终端上的话，一句 "\r\x1b[32m✓ 已签名" 就能把刚打出来的
// 信任等级那一行盖掉重写 —— 铁律 8 说客户端是唯一的安全边界，能被覆写的那一行
// 就不叫边界。所以表格先用 tabwriter 排进内存缓冲，再逐行交给 ui.Info。
// 只有 --json 那份机器可读的输出走 stdout（规格 §9）。

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Cirraya-LLC/toy/internal/fetch"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/platform"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

const (
	// qrySearchLimit 是 `toy search` 默认显示几条。
	qrySearchLimit = 20
	// qryDescWidth 是描述列的截断宽度（按 rune）。索引里的 desc 长度没有上限，
	// 一条几百字的描述会把表格冲垮。
	qryDescWidth = 72
	// qryIndexProbeTimeout 是 doctor 探一个索引的上限。主进程的 ctx 只在 Ctrl-C
	// 时取消，一个不回话的索引会让整次体检挂在那儿。
	qryIndexProbeTimeout = 20 * time.Second
)

// ---------------------------------------------------------------- 排版工具

// qryFlags 建一个子命令的 FlagSet。用法统一由 main 打印（errUsage），
// 所以这里把 flag 包自己那份 usage 关掉，只留它的错误行。
func qryFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {}
	return fs
}

// qryCell 把一格里的控制字符换成看得见的 \xNN。
//
// tabwriter 靠 \t 和 \n 划分单元格，而 desc 是索引作者写的：一个 \t 能让这一行
// 之后的列全部错位，一个 \n 能凭空多造一行（后面那半行长得就像一条正常记录）。
// ui 那边最终也会转义一次，但那时列已经算完了 —— 必须在进 tabwriter 之前做。
// 连续空白也折成一个空格：一格内容没有理由自带列间距，而那正是拿一个
// name 在表里画出一整行假记录的原料。
func qryCell(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0 &&
		!strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	space := false
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			fmt.Fprintf(&b, `\x%02x`, r)
			space = false
			continue
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// qryCellWidth 是表格里一格的默认上限（按 rune）。
//
// name / desc / 索引名一路来自 toy.toml 和索引，长度和内容都没有上限。
// tabwriter 的列宽 = 该列最宽那格 + 2，所以一格 300 列宽的 name 会把它右边
// 所有列 —— 包括【每一行】的信任等级 —— 一起顶到屏幕外面去，而那一格里正好
// 排得下一段自己写的 "v3.2.1  ✓ 已签名 9f3c1a20  Derrity/cowsay"。
// 一个恶意包就能让整张表的信任列消失并伪造自己那一行（铁律 8）。
const qryCellWidth = 24

// qryTrunc 按 rune 截断，超了缀省略号。
func qryTrunc(s string, n int) string {
	if n <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// qryTable 把 rows 排成对齐的表格打出去。第一行当表头。
//
// 用 tabwriter 的列宽是按 rune 数算的，而中日韩字符在终端上占两格 —— 所以
// 含中文的列后面会有一两格的参差。把最自由的那一列（描述、来源）放最后就行，
// 别为了这个自己去实现一张 East Asian Width 表。
//
// widths 按列给上限，缺的用 qryCellWidth，0 表示不截。最后一列默认不截：
// 它右边没有东西可以被顶出去。
func qryTable(rows [][]string, widths ...int) {
	if len(rows) == 0 {
		return
	}
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			limit := qryCellWidth
			if i == len(r)-1 {
				limit = 0
			}
			if i < len(widths) {
				limit = widths[i]
			}
			cells[i] = qryTrunc(qryCell(c), limit)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	// tabwriter 只往内存缓冲写，Flush 不会失败到值得处理的地步。
	_ = w.Flush()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		ui.Info("%s", strings.TrimRight(line, " "))
	}
}

// qryTrustLabel 是表格里那一格信任等级。
// 铁律 8：认不出的等级要说出来，不能当成安全的那一档糊过去。
func qryTrustLabel(trust, signer string) string {
	switch trust {
	case store.TrustSigned:
		if signer == "" {
			// 说"已签名"却说不出是谁签的，等于替一把不知道是谁的钥匙背书。
			return "⚠ claims signed, no signer"
		}
		return "✓ signed " + signer
	case store.TrustHost:
		return "· host account"
	case store.TrustIndexed:
		// 这一列要看得出「打包说明是索引提供的」（规则 3）：装哪个资产、
		// 命令叫什么名字不是作者写的，那是这张表里该一眼看见的事。
		return "⚠ packaging from index"
	case store.TrustUnverified:
		return "⚠ unsigned"
	case "":
		return "⚠ no trust level in manifest"
	default:
		return fmt.Sprintf("⚠ unknown level %q", trust)
	}
}

// ---------------------------------------------------------------- 已装索引

// qryInstalledIndex 把安装清单摊成一张查找表，好回答「这个包装没装过」。
//
// 索引条目的 id 是 "Derrity/tinyserve"、source 是 "github:Derrity/tinyserve"，
// 而清单的 id 是后者 —— 两头对不上，所以一个清单登记三个 key。
// 全部小写：GitHub/GitLab 的 owner 和 repo 本来就大小写不敏感。
func qryInstalledIndex(ms []store.Manifest) map[string]store.Manifest {
	out := make(map[string]store.Manifest, len(ms)*3)
	add := func(k string, m store.Manifest) {
		k = strings.ToLower(strings.TrimSpace(k))
		// 先到先得：同一个 owner/repo 在两家托管方各装了一份时，这个笼统的 key
		// 只能指向其中一个。它只用来显示「已装 v1.2.3」，带 host 的那两个 key
		// 更具体、也先登记，所以真正会歧义的只有这一个。
		if k == "" || k == ":" {
			return
		}
		if _, ok := out[k]; !ok {
			out[k] = m
		}
	}
	for _, m := range ms {
		add(m.ID, m)
		if m.Owner != "" && m.Repo != "" {
			if m.Host != "" {
				add(m.Host+":"+m.Owner+"/"+m.Repo, m)
			}
			add(m.Owner+"/"+m.Repo, m)
		}
	}
	return out
}

// qryLookupInstalled 按给的几个 key 依次查，先具体后笼统。
func qryLookupInstalled(idx map[string]store.Manifest, keys ...string) (store.Manifest, bool) {
	for _, k := range keys {
		if m, ok := idx[strings.ToLower(strings.TrimSpace(k))]; ok {
			return m, true
		}
	}
	return store.Manifest{}, false
}

// ---------------------------------------------------------------- toy list

func runList(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	ms, err := st.ListManifests()
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		ui.Info("Nothing installed yet.")
		ui.Info("`toy search <query>` looks through the indexes. And if you already know")
		ui.Info("where the repository is, `toy install github:owner/repo` skips them entirely.")
		return nil
	}

	// 信任放第一列：它是这张表里唯一不能被挤掉的东西，而放在第一列就不存在
	// 「被左边那格顶出屏幕」这回事（铁律 8）。
	rows := [][]string{{"TRUST", "NAME", "VERSION", "SOURCE"}}
	for _, m := range ms {
		name := m.Name
		if name == "" {
			name = m.Repo
		}
		version := m.Version
		if version == "" {
			version = m.Tag
		}
		if version == "" {
			// 裸地址那条路没有版本可言（resolve 说得很清楚），编一个出来就是撒谎。
			version = "—"
		}
		source := m.ID
		if source == "" {
			source = m.Source
		}
		rows = append(rows, []string{qryTrustLabel(m.Trust, m.Signer), name, version, source})
	}
	// 信任那一格要放得下完整指纹，所以它的上限比别的列宽。
	qryTable(rows, 48, qryCellWidth, 16, 0)
	ui.Info("")
	ui.Info("%d installed. `toy info <ref>` for details, `toy update` to upgrade.", len(ms))
	return nil
}

// ---------------------------------------------------------------- toy search

// qrySearchHit 是 --json 的一条结果。刻意不直接吐 index.Entry：
// 索引里以后加的字段不该自动出现在我们承诺的输出格式里。
type qrySearchHit struct {
	ID     string   `json:"id"`
	Source string   `json:"source"`
	Desc   string   `json:"desc,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Index  string   `json:"index"`
	Score  int      `json:"score"`
	// HasKey 只说明索引条目里有一把格式合法的 ed25519 公钥。**不是「已签名」**：
	// search 手里没有那一版的 toy.toml（要为每条结果各发一次网络请求才拿得到），
	// 所以它一次签名都验不了。抄一把别人的公钥是零成本的，条目里连 sig 都可以
	// 是空的 —— 叫它 signed 等于把这三个字白送给任何一个能改索引的人（规格 §5、
	// 铁律 8）。真正验过签名的只有 install / info 那条路。
	HasKey           bool   `json:"has_key"`
	KeyFingerprint   string `json:"key_fingerprint,omitempty"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
}

func runSearch(ctx context.Context, args []string) error {
	fs := qryFlags("search")
	limit := fs.Int("limit", qrySearchLimit, "how many results to show")
	asJSON := fs.Bool("json", false, "machine-readable output on stdout")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errUsage
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit has to be positive, got %d", *limit)
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	// 一个索引都没配是【默认状态】，不是出错（见 store.DefaultIndexURL）。
	// 走下去只会打出一句「索引里没有匹配 X 的包」—— 那句话是假的，我们根本
	// 没找过任何地方（铁律 8）。
	if len(cfg.Indexes) == 0 {
		qryNoIndexHelp()
		if *asJSON {
			return qrySearchJSON(query, nil)
		}
		return nil
	}
	idxs, err := loadIndexes(ctx, st, cfg)
	if err != nil {
		return err
	}
	// 装没装过是本地信息，读不出来不该让整次搜索失败 —— 那一列缺了就是缺了。
	ms, err := st.ListManifests()
	if err != nil {
		ui.Warn("cannot read the install manifests (%v), so the \"installed\" column is missing this time", err)
	}
	installed := qryInstalledIndex(ms)

	results := index.Search(idxs, query, *limit)
	hits := make([]qrySearchHit, 0, len(results))
	for _, r := range results {
		h := qrySearchHit{
			ID:     r.Entry.ID,
			Source: r.Entry.Source,
			Desc:   r.Entry.Desc,
			Tags:   r.Entry.Tags,
			Index:  r.Index,
			Score:  r.Score,
		}
		// 索引里那把 key 不是合法的 ed25519 公钥时，绝不能渲染成一个看起来
		// 正常的指纹（铁律 8）—— rsvFingerprint 认不出就返回空串。
		if fp := rsvFingerprint(r.Entry.Key); fp != "" {
			h.HasKey, h.KeyFingerprint = true, fp
		}
		if m, ok := qryLookupInstalled(installed, r.Entry.Source, r.Entry.ID); ok {
			h.Installed = true
			h.InstalledVersion = m.Version
		}
		hits = append(hits, h)
	}

	if *asJSON {
		return qrySearchJSON(query, hits)
	}

	if len(hits) == 0 {
		// 搜不到不等于装不了：索引只是个目录，托管方才是真相（规格 §5，
		// 「下架无害」说的就是这件事）。一句光秃秃的「没找到」会让人以为
		// 这个工具装不了，而它多半就躺在 GitHub 上等着。
		ui.Info("No package in the indexes matches %q.", query)
		ui.Info("")
		ui.Info("An index is only a directory; the host is the source of truth. If you know where it")
		ui.Info("lives, install it now — there is no need to wait for anyone to list it:")
		ui.Info("    toy install github:owner/repo")
		ui.Info("(Use a gitlab: / codeberg: prefix for other hosts, or `toy index add` someone else's index.)")
		return nil
	}

	rows := [][]string{{"PACKAGE", "STATUS", "INDEX", "DESCRIPTION"}}
	for _, h := range hits {
		state := "—"
		if h.Installed {
			state = "installed"
			if h.InstalledVersion != "" {
				state += " " + h.InstalledVersion
			}
		}
		rows = append(rows, []string{h.ID, state, h.Index, qryTrunc(h.Desc, qryDescWidth)})
	}
	qryTable(rows, 48, 20, qryCellWidth, 0)
	ui.Info("")
	if len(hits) == *limit {
		ui.Info("Showing %d (raise it with --limit). `toy info <package>` for details.", len(hits))
	} else {
		ui.Info("%s. `toy info <package>` for details, `toy install <package>` to install.", qryPlural(len(hits), "result", "results"))
	}
	return nil
}

// qrySearchJSON 是 --json 那份输出。规格 §9：机器可读的走 stdout，其余一切走 stderr。
// 没有结果时也要吐一份合法的空结果，别让脚本对着空 stdout 猜是不是崩了。
func qrySearchJSON(query string, hits []qrySearchHit) error {
	if hits == nil {
		hits = []qrySearchHit{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Query   string         `json:"query"`
		Count   int            `json:"count"`
		Results []qrySearchHit `json:"results"`
	}{query, len(hits), hits})
}

// qryNoIndexHelp 说清楚「一个索引都没配」是怎么回事，并给出照抄就能用的命令。
//
// 这是合法状态而不是故障：内置索引现在是空的（store.DefaultIndexURL），
// 而 `toy install github:owner/repo` 那条路从头到尾不经过索引。所以这里只讲
// 「怎么加一个」和「不加也能装」，一个叉都不打。
func qryNoIndexHelp() {
	ui.Info("No index is configured, so there is nothing to search. This is the default, not a fault.")
	ui.Info("")
	ui.Info("Add one (the name is local to you, pick anything):")
	ui.Info("    toy index add local file:///absolute/path/toy-index.json   # a local file")
	ui.Info("    toy index add local ./index/toy-index.json                # a bare path works too, it becomes file://")
	ui.Info("    toy index add community https://example.com/index.json")
	ui.Info("")
	ui.Info("An index is only a directory; the host is the source of truth. If you know where the")
	ui.Info("repository is, you never needed an index in the first place:")
	ui.Info("    toy install github:owner/repo")
}

// ---------------------------------------------------------------- toy info

func runInfo(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	r, err := ref.Parse(args[0])
	if err != nil {
		// `toy list` 和 `toy uninstall` 都认已装的那个短名字（tinyserve），而 info
		// 只认 ref —— 于是「列表里看到的名字」在这一个子命令上报「缺少斜杠」，
		// 同一个字符串在同一个 CLI 里有两种含义。装过的包我们手里就有它的 id，
		// 没有理由让用户再拼一遍。找不到就把原来那条解析错误还回去：
		// 那时它确实是个写坏了的 ref，不是一个已装的包。
		m, ferr := st.FindByName(args[0])
		if ferr != nil {
			return err
		}
		r2, perr := ref.Parse(m.ID)
		if perr != nil {
			return err
		}
		r = r2
	}
	res, err := resolve(ctx, st, cfg, r)
	if err != nil {
		return err
	}
	tf := res.Toyfile

	rows := [][]string{{"name", tf.Name}}
	add := func(label, value string) { rows = append(rows, []string{label, value}) }

	add("reference", res.Ref.String())
	if tf.Desc != "" {
		add("description", tf.Desc)
	}
	if res.Entry != nil && res.Entry.Desc != "" && res.Entry.Desc != tf.Desc {
		// 索引里的描述和作者自己写的可能不一样，两个都给 —— 哪个是哪个要说清楚。
		add("index description", res.Entry.Desc)
	}
	add("license", qryOr(tf.License, "not declared by the author"))
	if tf.Homepage != "" {
		add("homepage", tf.Homepage)
	} else if web := res.Host.WebURL(res.Owner, res.Repo); web != "" {
		add("repository", web)
	}

	if res.Tag != "" {
		add("latest version", res.Tag)
	} else {
		// 裸地址那条路压根没有 tag 可挑，编一个版本号出来就是撒谎（铁律 8）。
		add("version", "unknown — a plain URL carries no version; you get whatever that file is right now")
	}

	// 只比一个精确 key 会把「其实能装」（Rosetta、musl 退回通用构建）报成
	// 「没有对应构建」，所以这里和 install 一样按候选顺序判断。
	cands := platform.Candidates()
	// 报给人看的是两段的底座，不是 cands[0] —— linux 上那个是我们自己合成的
	// linux/amd64/gnu，把它当成「你的平台」写出来会让人去找一个不存在的资产。
	cur, matched := platform.Base(), cands[0]
	if tf.IsScript() {
		add("kind", "single-file script: download + chmod +x, nothing unpacked, nothing built")
		add("platforms", "platform-independent (whether it runs is up to the script)")
	} else {
		add("kind", "release binary: download + unpack, not one line of script is run")
		plats := tf.Platforms()
		add("platforms", strings.Join(plats, "  "))
		// 「是不是真的回退」只有一处判据 —— install 打的那句话就是它决定的。
		// info 和 install 对同一个包给出不同说法，比两边都说错更糟。
		switch hit := qryMatchPlatform(plats, cands); {
		case hit == "":
			add("this machine", cur+"  ✗ no matching build, cannot install")
			add("", "tried "+strings.Join(cands, ", ")+
				". Open an issue upstream asking the author to publish a "+cur+" asset and add one line for it in toy.toml")
		default:
			matched = hit // 下面挑 sha256 要用真正命中的那个 key
			// 装得上，但装的不是为这台机器构建的那个 —— 原因直接用 install 那句话，
			// 别在这里另写一版：cur 和 hit 在「作者按 libc 分了档」时是同一个字符串，
			// 拼成「回退：linux/amd64」会指着自己说自己。
			if note := insFallbackNote(cands, hit, plats); note != "" {
				add("this machine", cur+"  ✓ installable, but not the build made for this machine")
				add("", note)
			} else {
				add("this machine", cur+"  ✓ has a matching build")
			}
		}
	}

	// 装完之后会出现在 PATH 上的名字 —— 这是「装之前该看的那一屏」里最实在的一格。
	declared, what := tf.Bin, "path in the package"
	if tf.IsScript() {
		declared, what = tf.Script, "release asset"
	}
	label := "commands"
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		add(label, fmt.Sprintf("%s  →  %s: %s", name, what, declared[name]))
		label = ""
	}

	switch {
	case tf.IsScript():
		add("integrity", "scripts declare no per-platform sha256; the hash is pinned on first install (TOFU) and a later change is refused")
	case tf.SHA256[matched] != "":
		add("integrity", "this platform declares a sha256; it is checked on install and a mismatch is always refused")
	case len(tf.SHA256) > 0:
		add("integrity", fmt.Sprintf("the author gave a sha256 for %d platform(s), not this one — so the hash gets pinned on first install (TOFU)", len(tf.SHA256)))
	default:
		add("integrity", "the author declared no sha256; the hash is pinned on first install (TOFU) and a later change is refused")
	}

	ms, err := st.ListManifests()
	if err != nil {
		ui.Warn("cannot read the install manifests (%v), so this cannot say whether it is installed", err)
	}
	keys := []string{res.Ref.String(), res.Ref.ID()}
	if res.HostName != "" && res.Owner != "" && res.Repo != "" {
		keys = append([]string{res.HostName + ":" + res.Owner + "/" + res.Repo}, keys...)
	}
	if m, ok := qryLookupInstalled(qryInstalledIndex(ms), keys...); ok {
		add("installed", qryOr(m.Version, m.Tag)+" ("+qryTrustLabel(m.Trust, m.Signer)+")")
	} else {
		add("installed", "no")
	}

	qryTable(rows, 16, 0)
	ui.Info("")
	// 信任等级走 ui.Trust，和安装完那一行是同一套措辞（规格 §4、铁律 8）。
	ui.Trust(res.Trust, insTrustDetail(res))
	return nil
}

// qryMatchPlatform 返回 candidates 里第一个被声明了的平台 key，一个都没有就是空串。
// 和 toyfile.AssetFor 的挑选顺序必须一致 —— 不一致的话 info 会说"装得上"而 install 拒绝，
// 或者反过来。
func qryMatchPlatform(declared, candidates []string) string {
	for _, c := range candidates {
		if slices.Contains(declared, c) {
			return c
		}
	}
	return ""
}

func qryOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// qryHostLabel 是给人看的来源，形如 "github.com/Derrity"。
func qryHostLabel(res *resolved) string {
	if u, err := url.Parse(res.Host.WebURL(res.Owner, res.Repo)); err == nil && u.Hostname() != "" {
		return u.Hostname() + "/" + res.Owner
	}
	if res.HostName != "" {
		return res.HostName + "/" + res.Owner
	}
	return res.Owner
}

// ---------------------------------------------------------------- toy index

func runIndex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return errUsage
		}
		return qryIndexAdd(st, cfg, args[1], args[2])
	case "rm", "remove":
		if len(args) != 2 {
			return errUsage
		}
		return qryIndexRm(st, cfg, args[1])
	case "list", "ls":
		if len(args) != 1 {
			return errUsage
		}
		return qryIndexList(st, cfg)
	default:
		return fmt.Errorf("no such subcommand %q — there is only add / rm / list", args[0])
	}
}

func qryIndexAdd(st *store.Store, cfg store.Config, name, rawURL string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("an index needs a name: `toy index add <name> <url>`. The name is local to you, pick anything")
	}
	if qryCell(name) != name {
		return fmt.Errorf("the index name %q contains control characters — it gets printed to your terminal as-is, so pick another one", name)
	}
	u, err := qryIndexURL(rawURL)
	if err != nil {
		return err
	}
	for _, spec := range cfg.Indexes {
		if strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			return fmt.Errorf("there is already an index called %q (%s). Run `toy index rm %s` first, then add it again",
				spec.Name, spec.URL, name)
		}
		if spec.URL == u {
			// 允许，但要说 —— 同一个地址挂两个名字会被拉两遍，搜索结果里靠后
			// 那份会被去重掉，看起来像是「加了没生效」。
			ui.Warn("index %q already points at this URL; the same index under two names just gets fetched twice", spec.Name)
		}
	}
	cfg.Indexes = append(cfg.Indexes, store.IndexRef{Name: name, URL: u})
	if err := st.SaveConfig(cfg); err != nil {
		return err
	}
	ui.Info("added: %s -> %s", name, u)
	ui.Info("`toy doctor` checks that it is reachable; `toy search <query>` starts searching right away.")
	return nil
}

func qryIndexRm(st *store.Store, cfg store.Config, name string) error {
	name = strings.TrimSpace(name)
	for i, spec := range cfg.Indexes {
		if !strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			continue
		}
		cfg.Indexes = slices.Delete(cfg.Indexes, i, i+1)
		if err := st.SaveConfig(cfg); err != nil {
			return err
		}
		ui.Info("removed: %s -> %s", spec.Name, spec.URL)
		// 缓存留着不删：它只在离线回退时被读，而那时这条索引已经不在配置里了，
		// 没人会去读它。真要清就删 ~/.toy/index/ 下对应那个文件。
		if len(cfg.Indexes) == 0 {
			ui.Warn("no indexes are left, so `toy search` will find nothing. " +
				"`toy install github:owner/repo` does not go through an index and still works")
		}
		return nil
	}
	if len(cfg.Indexes) == 0 {
		return fmt.Errorf("no index called %q — there are no indexes configured at all", name)
	}
	return fmt.Errorf("no index called %q. The configured ones are: %s", name, strings.Join(qryIndexNames(cfg), ", "))
}

func qryIndexNames(cfg store.Config) []string {
	out := make([]string, 0, len(cfg.Indexes))
	for _, spec := range cfg.Indexes {
		out = append(out, spec.Name)
	}
	return out
}

func qryIndexList(st *store.Store, cfg store.Config) error {
	if len(cfg.Indexes) == 0 {
		qryNoIndexHelp()
		ui.Info("")
		ui.Info("The config file is %s.", filepath.Join(st.Root, "config.toml"))
		return nil
	}
	// 包数和缓存时间都只看本地缓存 —— 列个索引不该联网，那是 `toy doctor` 的活。
	rows := [][]string{{"NAME", "PACKAGES", "LAST CACHED", "URL"}}
	for _, spec := range cfg.Indexes {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.URL
		}
		count, when := qryIndexCacheInfo(st, name)
		rows = append(rows, []string{name, count, when, spec.URL})
	}
	qryTable(rows)
	ui.Info("")
	ui.Info("Counts and times come from the local cache (%s), not from the remote as it is now; `toy search` re-fetches.", st.IndexDir())
	ui.Info("A \"—\" means that index has never been fetched. `toy doctor` probes each one for reachability.")
	return nil
}

// qryIndexCacheInfo 读本地缓存报出包数和缓存时间。没缓存过就如实说没有。
func qryIndexCacheInfo(st *store.Store, name string) (count, when string) {
	path := rsvIndexCache(st, name)
	fi, err := os.Stat(path)
	if err != nil {
		// 用 "—" 而不是「还没拉过」：tabwriter 按 rune 数排列，而中文一个字占两格，
		// 同一列里中英混排会把后面那一列顶得参差（见 qryTable 的注释）。
		// 这一格的含义交给表格下面那句话去说。
		return "—", "—"
	}
	when = fi.ModTime().Local().Format("2006-01-02 15:04")
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable", when
	}
	idx, _, err := index.Load(data, name)
	if err != nil {
		return "broken", when
	}
	return fmt.Sprint(len(idx.Packages)), when
}

// qryIndexURL 把用户写的索引地址规范化。
// 裸路径当本地文件处理：规格 §5 说索引来源「实现上就是一个 URL」，而 v1 的
// 索引通常就是本地一个 json，逼人手写 file:///绝对路径 只会劝退。
func qryIndexURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("the index URL is empty (give an https://… URL, or the path to a local json file)")
	}
	if !strings.Contains(s, "://") {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("%q is not a URL, and as a local path it cannot be made absolute either: %w", s, err)
		}
		s = "file://" + filepath.ToSlash(abs)
	}
	r, err := ref.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable index location (it has to be https://… or file:///absolute/path): %w", s, err)
	}
	// ref.Parse 见到 "://" 就只会给出 KindURL 或报错，这里只是把那个前提写下来。
	if r.Kind != ref.KindURL {
		return "", fmt.Errorf("%q is not a location — an index has to be https://… or file:///absolute/path", s)
	}
	return r.URL, nil
}

// ---------------------------------------------------------------- toy doctor

// qryCheck 是体检的一条。有问题就必须给得出具体的修复命令 ——
// 一份只报告不给办法的体检报告等于把问题原样退回给用户。
type qryCheck struct {
	ok    bool
	title string
	note  string
	fix   string
}

func runDoctor(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	var checks []qryCheck
	checks = append(checks, qryCheckPath(st))
	checks = append(checks,
		qryCheckDir("bin directory", st.BinDir(), false),
		qryCheckDir("pkg directory", filepath.Join(st.Root, "pkg"), false),
		qryCheckDir("keys directory", st.KeysDir(), true),
	)
	checks = append(checks, qryCheckGit(st.BinDir()))
	checks = append(checks, qryCheckIndexes(ctx, cfg)...)
	checks = append(checks, qryCheckLinks(st))
	checks = append(checks, qryCheckManifests(st)...)

	ui.Info("Checking %s", st.Root)
	ui.Info("")
	qryPlatformInfo()
	bad := 0
	for _, c := range checks {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			bad++
		}
		ui.Info("%s %s", mark, c.title)
		if c.note != "" {
			ui.Info("    %s", c.note)
		}
		if !c.ok && c.fix != "" {
			ui.Info("    fix: %s", c.fix)
		}
	}
	ui.Info("")
	if bad == 0 {
		ui.Info("All %d checks passed.", len(checks))
		return nil
	}
	// 返回 error 就是退出码 1（见 main），错误信息本身也得说人话。
	return fmt.Errorf("%d of %d checks failed — each one above prints the command that fixes it", bad, len(checks))
}

// qryPlatformInfo 打印这台机器会去要哪些构建。
//
// 它【不是】检查项，所以不打 ✓/✗ —— 「这台机器是 musl」既不对也不错，
// 给它一个红叉会让人去修一件根本没坏的事。它存在的理由是：装出来的东西起不来时
// （musl 上跑 glibc 二进制、Rosetta 没装、armv6 上跑 armv7），
// 报错里没有一个字指向平台，而这几行是唯一能对上号的线索。
func qryPlatformInfo() {
	ui.Info("platform %s", platform.Current())
	ui.Info("    tried in this order: %s", strings.Join(platform.Candidates(), " -> "))
	if lvl := platform.CPULevelHint(); lvl != "" {
		// 只是线索：toy 不做微架构匹配（要解析 CPUID，标准库不给，引库违反规格 §10）。
		ui.Info("    CPU is roughly %s (a hint only, it plays no part in picking a build)", lvl)
	}
	ui.Info("")
}

// qryCheckPath 是新用户第一个坑：装完了，命令却「找不到」。
func qryCheckPath(st *store.Store) qryCheck {
	bin := st.BinDir()
	if st.InPath() {
		return qryCheck{ok: true, title: bin + " is on PATH"}
	}
	return qryCheck{
		title: bin + " is not on PATH — installed commands will not be found",
		note:  "toy puts every symlink in this one directory; if it is not on PATH, nothing you install can run",
		fix:   fmt.Sprintf(`add this line to ~/.bashrc or ~/.zshrc, then open a new terminal: export PATH="%s:$PATH"`, bin),
	}
}

// qryCheckDir 检查目录在不在、权限对不对。
// strict 是 keys/ 那一档：里面是私钥，别人一点都不能读。
func qryCheckDir(label, dir string, strict bool) qryCheck {
	fi, err := os.Stat(dir)
	if err != nil {
		return qryCheck{
			title: label + " " + dir + " is unusable",
			note:  err.Error(),
			fix:   "mkdir -p " + dir,
		}
	}
	if !fi.IsDir() {
		return qryCheck{
			title: label + " " + dir + " is not a directory",
			fix:   "move it aside and run toy again: mv " + dir + " " + dir + ".bak",
		}
	}
	perm := fi.Mode().Perm()
	if perm&0o700 != 0o700 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — you cannot fully read and write it yourself", label, dir, perm),
			fix:   "chmod u+rwx " + dir,
		}
	}
	if strict && perm&0o077 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — other users can read the private key in it", label, dir, perm),
			fix:   "chmod 700 " + dir,
		}
	}
	if !strict && perm&0o022 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s is mode %#o — other users can write into it", label, dir, perm),
			note:  "anyone who can write here can replace a command on your PATH with one of their own",
			fix:   "chmod go-w " + dir,
		}
	}
	return qryCheck{ok: true, title: fmt.Sprintf("%s %s mode %#o", label, dir, perm)}
}

// qryCheckGit：没有 git 就没有版本发现，github:/gitlab: 那整条路都是废的（规格 §2）。
//
// 走 autGitPath 而不是 exec.LookPath：toy 真正会执行的就是它挑出来的那一个，
// 体检报的必须是同一个。用 LookPath 的话，一个装上来的包把 git 这个名字占下时，
// doctor 会指着 ~/.toy/bin/git 说一句「git 在：…」—— 一条如实但把攻击说成健康的话
// （铁律 8）。
func qryCheckGit(binDir string) qryCheck {
	path, err := autGitPath(binDir)
	if err == nil {
		return qryCheck{ok: true, title: "git found at " + path}
	}
	return qryCheck{
		title: "no usable git",
		note:  "toy discovers versions with `git ls-remote --tags`; without it no github:/gitlab: package can be installed. " + ui.SafeURL(err.Error()),
		fix:   "Debian/Ubuntu: sudo apt install git — macOS: xcode-select --install",
	}
}

func qryCheckIndexes(ctx context.Context, cfg store.Config) []qryCheck {
	if len(cfg.Indexes) == 0 {
		// 这不是本机的毛病：内置索引现在就是空的（store.DefaultIndexURL），
		// 报成 ✗ 等于让每个新用户第一次体检就看见一条红叉，还以为是自己装坏了。
		// 中性地说一句，办法照给（铁律 8：如实说，但别栽赃）。
		return []qryCheck{{
			ok:    true,
			title: "no index configured (`toy search` has nothing to search yet)",
			note: "this is the default and it does not affect `toy install github:owner/repo` — that route never goes through an index. " +
				"To start searching, run `toy index add <name> <url>` (the url can be https://… or the path to a local json file)",
		}}
	}
	out := make([]qryCheck, 0, len(cfg.Indexes))
	for _, spec := range cfg.Indexes {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			name = spec.URL
		}
		if strings.TrimSpace(spec.URL) == "" {
			out = append(out, qryCheck{
				title: "index " + name + " has no url",
				fix:   "toy index rm " + name + ", then add it again",
			})
			continue
		}
		probe, cancel := context.WithTimeout(ctx, qryIndexProbeTimeout)
		data, err := fetch.GetBytes(probe, spec.URL, fetch.Options{Name: name})
		cancel()
		if err != nil {
			out = append(out, qryCheck{
				title: "index " + name + " cannot be fetched",
				note:  ui.SafeURL(err.Error()),
				fix:   "check the url (" + ui.SafeURL(spec.URL) + "), or drop it with `toy index rm " + name + "`",
			})
			continue
		}
		idx, skipped, err := index.Load(data, name)
		if err != nil {
			out = append(out, qryCheck{
				title: "index " + name + " was fetched but cannot be used",
				note:  err.Error(),
				fix:   "point it at a different url, or `toy index rm " + name + "`",
			})
			continue
		}
		c := qryCheck{ok: true, title: fmt.Sprintf("index %s reachable, %d packages", name, len(idx.Packages))}
		if skipped > 0 {
			// 索引方的问题，不是本机的问题 —— 不算 ✗，但也不能不说。
			c.note = fmt.Sprintf("%d record(s) in it are invalid and were skipped — those packages will not turn up in a search until the index maintainer fixes them", skipped)
		}
		out = append(out, c)
	}
	return out
}

// qryCheckLinks 找 bin/ 里指向已经不存在的东西的软链。
// 那种链在 PATH 上看起来是个命令，敲下去只会说「文件不存在」。
func qryCheckLinks(st *store.Store) qryCheck {
	bin := st.BinDir()
	entries, err := os.ReadDir(bin)
	if err != nil {
		return qryCheck{title: "cannot read " + bin, note: err.Error(), fix: "mkdir -p " + bin}
	}
	var dangling []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		p := filepath.Join(bin, e.Name())
		if _, err := os.Stat(p); err == nil { // Stat 跟随软链，能跟到就是好的
			continue
		}
		target, err := os.Readlink(p)
		if err != nil {
			target = "(target unreadable)"
		}
		dangling = append(dangling, e.Name()+" → "+target)
	}
	if len(dangling) == 0 {
		return qryCheck{ok: true, title: fmt.Sprintf("%d entries in bin, no dangling symlinks", len(entries))}
	}
	return qryCheck{
		title: fmt.Sprintf("%d dangling symlink(s) — they point at files that are gone", len(dangling)),
		note:  strings.Join(dangling, "; "),
		fix:   "reinstall that package (toy install <ref>), or clear it out with `toy uninstall <name>`",
	}
}

// qryCheckManifests 找孤儿清单：清单还在，pkg 目录没了。
// 那种包 `toy list` 列得出来，跑起来却什么都没有。
func qryCheckManifests(st *store.Store) []qryCheck {
	ms, err := st.ListManifests()
	if err != nil {
		return []qryCheck{{
			title: "cannot read the install manifests",
			note:  err.Error(),
			fix:   "delete the broken manifest at the path in the error above (after that, clean up its package by hand under ~/.toy/pkg)",
		}}
	}
	var orphans []string
	for _, m := range ms {
		if m.PkgDir == "" { // 单文件脚本形态可能压根没有 pkg 目录，别冤枉它
			continue
		}
		if _, err := os.Stat(m.PkgDir); err != nil {
			orphans = append(orphans, m.ID+" ("+m.PkgDir+")")
		}
	}
	if len(orphans) == 0 {
		return []qryCheck{{ok: true, title: fmt.Sprintf("%d install manifest(s), every one matches a package directory on disk", len(ms))}}
	}
	return []qryCheck{{
		title: fmt.Sprintf("%d package(s) still have a manifest but no files", len(orphans)),
		note:  strings.Join(orphans, "; "),
		fix:   "reinstall it (toy install <the id above>) or remove it (toy uninstall <name>)",
	}}
}

// qryPlural 是最小的单复数：写 "1 result(s)" 会让整个 CLI 显得像半成品，
// 而这一行是搜索结果的落款，用户看得最多。
func qryPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
