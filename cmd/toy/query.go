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
			return "⚠ 声称已签名但没有签名者"
		}
		return "✓ 已签名 " + signer
	case store.TrustHost:
		return "· 托管方账号"
	case store.TrustUnverified:
		return "⚠ 未签名"
	case "":
		return "⚠ 清单里没记信任等级"
	default:
		return fmt.Sprintf("⚠ 未知等级 %q", trust)
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
		ui.Info("还没装任何东西。")
		ui.Info("`toy search <关键词>` 在索引里找找看；知道仓库在哪的话，")
		ui.Info("`toy install github:owner/repo` 不经过索引也能直接装。")
		return nil
	}

	// 信任放第一列：它是这张表里唯一不能被挤掉的东西，而放在第一列就不存在
	// 「被左边那格顶出屏幕」这回事（铁律 8）。
	rows := [][]string{{"信任", "名字", "版本", "来源"}}
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
	ui.Info("共 %d 个。`toy info <ref>` 看详情，`toy update` 升级。", len(ms))
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
	limit := fs.Int("limit", qrySearchLimit, "最多显示几条")
	asJSON := fs.Bool("json", false, "机器可读的输出，走 stdout")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errUsage
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit 要是正数，给的是 %d", *limit)
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
		ui.Warn("读不了安装清单（%v），这次不显示「装没装过」那一列", err)
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
		ui.Info("索引里没有匹配 %q 的包。", query)
		ui.Info("")
		ui.Info("索引只是一份目录，托管方才是真相 —— 知道它在哪就直接装，不用等它被收录：")
		ui.Info("    toy install github:owner/repo")
		ui.Info("（在别家就换成 gitlab: / codeberg: 前缀，或者 `toy index add` 加一个别人的索引。）")
		return nil
	}

	rows := [][]string{{"包", "状态", "索引", "描述"}}
	for _, h := range hits {
		state := "—"
		if h.Installed {
			state = "已装"
			if h.InstalledVersion != "" {
				state += " " + h.InstalledVersion
			}
		}
		rows = append(rows, []string{h.ID, state, h.Index, qryTrunc(h.Desc, qryDescWidth)})
	}
	qryTable(rows, 48, 16, qryCellWidth, 0)
	ui.Info("")
	if len(hits) == *limit {
		ui.Info("只显示了 %d 条（--limit 可以调）。`toy info <包>` 看详情。", len(hits))
	} else {
		ui.Info("共 %d 条。`toy info <包>` 看详情，`toy install <包>` 装。", len(hits))
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
	ui.Info("还没配任何索引，所以搜不到东西 —— 这是默认状态，不是出错了。")
	ui.Info("")
	ui.Info("加一个（名字只在本地用，随便起）：")
	ui.Info("    toy index add local file:///绝对路径/toy-index.json   # 本地文件")
	ui.Info("    toy index add local ./index/toy-index.json           # 裸路径也行，会自动补成 file://")
	ui.Info("    toy index add community https://example.com/index.json")
	ui.Info("")
	ui.Info("索引只是一份目录，托管方才是真相 —— 知道仓库在哪就直接装，本来也不需要索引：")
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

	rows := [][]string{{"名字", tf.Name}}
	add := func(label, value string) { rows = append(rows, []string{label, value}) }

	add("引用", res.Ref.String())
	if tf.Desc != "" {
		add("描述", tf.Desc)
	}
	if res.Entry != nil && res.Entry.Desc != "" && res.Entry.Desc != tf.Desc {
		// 索引里的描述和作者自己写的可能不一样，两个都给 —— 哪个是哪个要说清楚。
		add("索引里的描述", res.Entry.Desc)
	}
	add("许可证", qryOr(tf.License, "作者没声明"))
	if tf.Homepage != "" {
		add("主页", tf.Homepage)
	} else if web := res.Host.WebURL(res.Owner, res.Repo); web != "" {
		add("仓库", web)
	}

	if res.Tag != "" {
		add("最新版本", res.Tag)
	} else {
		// 裸地址那条路压根没有 tag 可挑，编一个版本号出来就是撒谎（铁律 8）。
		add("版本", "未知 —— 裸地址里没有版本信息，装出来的就是此刻那个文件")
	}

	// 只比一个精确 key 会把「其实能装」（Rosetta、musl 退回通用构建）报成
	// 「没有对应构建」，所以这里和 install 一样按候选顺序判断。
	cands := platform.Candidates()
	// 报给人看的是两段的底座，不是 cands[0] —— linux 上那个是我们自己合成的
	// linux/amd64/gnu，把它当成「你的平台」写出来会让人去找一个不存在的资产。
	cur, matched := platform.Base(), cands[0]
	if tf.IsScript() {
		add("形态", "单文件脚本：下载 + chmod +x，零解压零构建")
		add("支持平台", "不区分平台（脚本自己决定跑不跑得起来）")
	} else {
		add("形态", "release 二进制：下载 + 解压，一行脚本都不跑")
		plats := tf.Platforms()
		add("支持平台", strings.Join(plats, "  "))
		// 「是不是真的回退」只有一处判据 —— install 打的那句话就是它决定的。
		// info 和 install 对同一个包给出不同说法，比两边都说错更糟。
		switch hit := qryMatchPlatform(plats, cands); {
		case hit == "":
			add("当前平台", cur+"  ✗ 没有对应的构建，装不了")
			add("", "试过 "+strings.Join(cands, "、")+
				"。去上游提个 issue，请作者在 release 里补一个 "+cur+" 的资产并在 toy.toml 里加一行")
		default:
			matched = hit // 下面挑 sha256 要用真正命中的那个 key
			// 装得上，但装的不是为这台机器构建的那个 —— 原因直接用 install 那句话，
			// 别在这里另写一版：cur 和 hit 在「作者按 libc 分了档」时是同一个字符串，
			// 拼成「回退：linux/amd64」会指着自己说自己。
			if note := insFallbackNote(cands, hit, plats); note != "" {
				add("当前平台", cur+"  ✓ 装得上，但不是为这台机器专门构建的那个")
				add("", note)
			} else {
				add("当前平台", cur+"  ✓ 有对应的构建")
			}
		}
	}

	// 装完之后会出现在 PATH 上的名字 —— 这是「装之前该看的那一屏」里最实在的一格。
	declared, what := tf.Bin, "包内路径"
	if tf.IsScript() {
		declared, what = tf.Script, "release 资产"
	}
	label := "命令"
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		add(label, fmt.Sprintf("%s  →  %s %s", name, what, declared[name]))
		label = ""
	}

	switch {
	case tf.IsScript():
		add("完整性", "脚本形态没有按平台声明的 sha256，首次安装钉住哈希（TOFU），之后变了会拒绝")
	case tf.SHA256[matched] != "":
		add("完整性", "当前平台声明了 sha256，安装时强制校验，对不上一律拒绝")
	case len(tf.SHA256) > 0:
		add("完整性", fmt.Sprintf("作者只给了 %d 个平台的 sha256，当前平台不在其中 —— 走 TOFU 钉住", len(tf.SHA256)))
	default:
		add("完整性", "作者没写 sha256，首次安装钉住哈希（TOFU），之后变了会拒绝")
	}

	ms, err := st.ListManifests()
	if err != nil {
		ui.Warn("读不了安装清单（%v），这次说不出装没装过", err)
	}
	keys := []string{res.Ref.String(), res.Ref.ID()}
	if res.HostName != "" && res.Owner != "" && res.Repo != "" {
		keys = append([]string{res.HostName + ":" + res.Owner + "/" + res.Repo}, keys...)
	}
	if m, ok := qryLookupInstalled(qryInstalledIndex(ms), keys...); ok {
		add("已安装", qryOr(m.Version, m.Tag)+"（"+qryTrustLabel(m.Trust, m.Signer)+"）")
	} else {
		add("已安装", "否")
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
		return fmt.Errorf("不认识的子命令 %q —— 只有 add / rm / list", args[0])
	}
}

func qryIndexAdd(st *store.Store, cfg store.Config, name, rawURL string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("索引得有个名字：`toy index add <名字> <地址>`。名字只在本地用，随便起")
	}
	if qryCell(name) != name {
		return fmt.Errorf("索引名 %q 里有控制字符 —— 它会被原样打印在终端上，换个名字", name)
	}
	u, err := qryIndexURL(rawURL)
	if err != nil {
		return err
	}
	for _, spec := range cfg.Indexes {
		if strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			return fmt.Errorf("已经有一个叫 %q 的索引了（%s）。先 `toy index rm %s` 再加",
				spec.Name, spec.URL, name)
		}
		if spec.URL == u {
			// 允许，但要说 —— 同一个地址挂两个名字会被拉两遍，搜索结果里靠后
			// 那份会被去重掉，看起来像是「加了没生效」。
			ui.Warn("索引 %q 用的也是这个地址；同一份索引挂两个名字只会被拉两遍", spec.Name)
		}
	}
	cfg.Indexes = append(cfg.Indexes, store.IndexRef{Name: name, URL: u})
	if err := st.SaveConfig(cfg); err != nil {
		return err
	}
	ui.Info("加好了：%s → %s", name, u)
	ui.Info("`toy doctor` 验一下它可不可达，`toy search <关键词>` 直接开搜。")
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
		ui.Info("删了：%s → %s", spec.Name, spec.URL)
		// 缓存留着不删：它只在离线回退时被读，而那时这条索引已经不在配置里了，
		// 没人会去读它。真要清就删 ~/.toy/index/ 下对应那个文件。
		if len(cfg.Indexes) == 0 {
			ui.Warn("现在一个索引都没有了，`toy search` 什么也搜不到。" +
				"`toy install github:owner/repo` 不经过索引，仍然能装")
		}
		return nil
	}
	if len(cfg.Indexes) == 0 {
		return fmt.Errorf("没有叫 %q 的索引 —— 配置里一个索引都没有", name)
	}
	return fmt.Errorf("没有叫 %q 的索引。现有的是：%s", name, strings.Join(qryIndexNames(cfg), "、"))
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
		ui.Info("配置文件在 %s。", filepath.Join(st.Root, "config.toml"))
		return nil
	}
	// 包数和缓存时间都只看本地缓存 —— 列个索引不该联网，那是 `toy doctor` 的活。
	rows := [][]string{{"名字", "包数", "上次缓存", "地址"}}
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
	ui.Info("包数和时间来自本地缓存（%s），不代表现在的远端内容；`toy search` 会重新拉。", st.IndexDir())
	ui.Info("「—」表示这个索引还没拉过。`toy doctor` 会挨个探一遍可达性。")
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
		return "读不了", when
	}
	idx, _, err := index.Load(data, name)
	if err != nil {
		return "坏了", when
	}
	return fmt.Sprint(len(idx.Packages)), when
}

// qryIndexURL 把用户写的索引地址规范化。
// 裸路径当本地文件处理：规格 §5 说索引来源「实现上就是一个 URL」，而 v1 的
// 索引通常就是本地一个 json，逼人手写 file:///绝对路径 只会劝退。
func qryIndexURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("索引地址是空的（https://… 或者本地那个 json 的路径）")
	}
	if !strings.Contains(s, "://") {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("%q 不是 URL，当本地路径也转不成绝对路径：%w", s, err)
		}
		s = "file://" + filepath.ToSlash(abs)
	}
	r, err := ref.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q 不是一个能用的索引地址（要 https://… 或 file:///绝对路径）：%w", s, err)
	}
	// ref.Parse 见到 "://" 就只会给出 KindURL 或报错，这里只是把那个前提写下来。
	if r.Kind != ref.KindURL {
		return "", fmt.Errorf("%q 不是一个地址 —— 索引要写成 https://… 或 file:///绝对路径", s)
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
		qryCheckDir("bin 目录", st.BinDir(), false),
		qryCheckDir("pkg 目录", filepath.Join(st.Root, "pkg"), false),
		qryCheckDir("keys 目录", st.KeysDir(), true),
	)
	checks = append(checks, qryCheckGit(st.BinDir()))
	checks = append(checks, qryCheckIndexes(ctx, cfg)...)
	checks = append(checks, qryCheckLinks(st))
	checks = append(checks, qryCheckManifests(st)...)

	ui.Info("体检 %s", st.Root)
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
			ui.Info("    修复：%s", c.fix)
		}
	}
	ui.Info("")
	if bad == 0 {
		ui.Info("%d 项全过。", len(checks))
		return nil
	}
	// 返回 error 就是退出码 1（见 main），错误信息本身也得说人话。
	return fmt.Errorf("%d/%d 项没过，每一条上面都写了修复命令", bad, len(checks))
}

// qryPlatformInfo 打印这台机器会去要哪些构建。
//
// 它【不是】检查项，所以不打 ✓/✗ —— 「这台机器是 musl」既不对也不错，
// 给它一个红叉会让人去修一件根本没坏的事。它存在的理由是：装出来的东西起不来时
// （musl 上跑 glibc 二进制、Rosetta 没装、armv6 上跑 armv7），
// 报错里没有一个字指向平台，而这几行是唯一能对上号的线索。
func qryPlatformInfo() {
	ui.Info("平台 %s", platform.Current())
	ui.Info("    候选顺序 %s", strings.Join(platform.Candidates(), " → "))
	if lvl := platform.CPULevelHint(); lvl != "" {
		// 只是线索：toy 不做微架构匹配（要解析 CPUID，标准库不给，引库违反规格 §10）。
		ui.Info("    CPU 指令集大致是 %s（只是线索，不参与挑构建）", lvl)
	}
	ui.Info("")
}

// qryCheckPath 是新用户第一个坑：装完了，命令却「找不到」。
func qryCheckPath(st *store.Store) qryCheck {
	bin := st.BinDir()
	if st.InPath() {
		return qryCheck{ok: true, title: "PATH 里有 " + bin}
	}
	return qryCheck{
		title: "PATH 里没有 " + bin + " —— 装上的命令敲不出来",
		note:  "toy 只往这一个目录里放软链，它不在 PATH 上的话什么都跑不起来",
		fix:   fmt.Sprintf(`把这一行加进 ~/.bashrc 或 ~/.zshrc 再开个新终端：export PATH="%s:$PATH"`, bin),
	}
}

// qryCheckDir 检查目录在不在、权限对不对。
// strict 是 keys/ 那一档：里面是私钥，别人一点都不能读。
func qryCheckDir(label, dir string, strict bool) qryCheck {
	fi, err := os.Stat(dir)
	if err != nil {
		return qryCheck{
			title: label + " " + dir + " 用不了",
			note:  err.Error(),
			fix:   "mkdir -p " + dir,
		}
	}
	if !fi.IsDir() {
		return qryCheck{
			title: label + " " + dir + " 不是目录",
			fix:   "把它挪开再重新跑 toy：mv " + dir + " " + dir + ".bak",
		}
	}
	perm := fi.Mode().Perm()
	if perm&0o700 != 0o700 {
		return qryCheck{
			title: fmt.Sprintf("%s %s 权限是 %#o，自己都读写不全", label, dir, perm),
			fix:   "chmod u+rwx " + dir,
		}
	}
	if strict && perm&0o077 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s 权限是 %#o，别人读得到里面的私钥", label, dir, perm),
			fix:   "chmod 700 " + dir,
		}
	}
	if !strict && perm&0o022 != 0 {
		return qryCheck{
			title: fmt.Sprintf("%s %s 权限是 %#o，别人写得进来", label, dir, perm),
			note:  "别人能往这里写，就等于能把你 PATH 上的命令换成他自己的",
			fix:   "chmod go-w " + dir,
		}
	}
	return qryCheck{ok: true, title: fmt.Sprintf("%s %s 权限 %#o", label, dir, perm)}
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
		return qryCheck{ok: true, title: "git 在：" + path}
	}
	return qryCheck{
		title: "找不到能用的 git",
		note:  "toy 用 `git ls-remote --tags` 发现版本，没有它就装不了任何 github:/gitlab: 形态的包。" + ui.SafeURL(err.Error()),
		fix:   "Debian/Ubuntu: sudo apt install git；macOS: xcode-select --install",
	}
}

func qryCheckIndexes(ctx context.Context, cfg store.Config) []qryCheck {
	if len(cfg.Indexes) == 0 {
		// 这不是本机的毛病：内置索引现在就是空的（store.DefaultIndexURL），
		// 报成 ✗ 等于让每个新用户第一次体检就看见一条红叉，还以为是自己装坏了。
		// 中性地说一句，办法照给（铁律 8：如实说，但别栽赃）。
		return []qryCheck{{
			ok:    true,
			title: "没有配索引（`toy search` 暂时搜不到东西）",
			note: "这是默认状态，不影响 `toy install github:owner/repo` —— 那条路本来就不经过索引。" +
				"要开搜就 `toy index add <名字> <地址>`（地址可以是 https://…，也可以是本地那个 json 的路径）",
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
				title: "索引 " + name + " 没写 url",
				fix:   "toy index rm " + name + " 再重新 add",
			})
			continue
		}
		probe, cancel := context.WithTimeout(ctx, qryIndexProbeTimeout)
		data, err := fetch.GetBytes(probe, spec.URL, fetch.Options{Name: name})
		cancel()
		if err != nil {
			out = append(out, qryCheck{
				title: "索引 " + name + " 拉不到",
				note:  ui.SafeURL(err.Error()),
				fix:   "对一下地址（" + ui.SafeURL(spec.URL) + "），不要了就 `toy index rm " + name + "`",
			})
			continue
		}
		idx, skipped, err := index.Load(data, name)
		if err != nil {
			out = append(out, qryCheck{
				title: "索引 " + name + " 拉到了但用不了",
				note:  err.Error(),
				fix:   "换一个地址，或者 `toy index rm " + name + "`",
			})
			continue
		}
		c := qryCheck{ok: true, title: fmt.Sprintf("索引 %s 可达，%d 个包", name, len(idx.Packages))}
		if skipped > 0 {
			// 索引方的问题，不是本机的问题 —— 不算 ✗，但也不能不说。
			c.note = fmt.Sprintf("里面有 %d 条记录不合法，已跳过（那些包搜不到，得让索引维护者修）", skipped)
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
		return qryCheck{title: "读不了 " + bin, note: err.Error(), fix: "mkdir -p " + bin}
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
			target = "（读不出目标）"
		}
		dangling = append(dangling, e.Name()+" → "+target)
	}
	if len(dangling) == 0 {
		return qryCheck{ok: true, title: fmt.Sprintf("bin 里 %d 项，没有悬空软链", len(entries))}
	}
	return qryCheck{
		title: fmt.Sprintf("有 %d 条悬空软链（指向已经没了的文件）", len(dangling)),
		note:  strings.Join(dangling, "；"),
		fix:   "重装那个包（toy install <ref>），或者 `toy uninstall <名字>` 清掉",
	}
}

// qryCheckManifests 找孤儿清单：清单还在，pkg 目录没了。
// 那种包 `toy list` 列得出来，跑起来却什么都没有。
func qryCheckManifests(st *store.Store) []qryCheck {
	ms, err := st.ListManifests()
	if err != nil {
		return []qryCheck{{
			title: "读不了安装清单",
			note:  err.Error(),
			fix:   "按上面那条错误里的路径把坏掉的清单删了（删了那个包就得手动清 ~/.toy/pkg）",
		}}
	}
	var orphans []string
	for _, m := range ms {
		if m.PkgDir == "" { // 单文件脚本形态可能压根没有 pkg 目录，别冤枉它
			continue
		}
		if _, err := os.Stat(m.PkgDir); err != nil {
			orphans = append(orphans, m.ID+"（"+m.PkgDir+"）")
		}
	}
	if len(orphans) == 0 {
		return []qryCheck{{ok: true, title: fmt.Sprintf("%d 份安装清单，都对得上磁盘上的包目录", len(ms))}}
	}
	return []qryCheck{{
		title: fmt.Sprintf("有 %d 个包的清单还在、文件没了", len(orphans)),
		note:  strings.Join(orphans, "；"),
		fix:   "重装（toy install <上面那个 id>）或者卸掉（toy uninstall <名字>）",
	}}
}
