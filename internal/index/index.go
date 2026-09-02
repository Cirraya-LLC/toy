// Package index 是索引的读写和搜索。规格 §5：索引只存指针，不存实质内容。
package index

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// Version 是本客户端认识的索引格式版本。规格 §5 的 JSON 里那个 "version": 1。
const Version = 1

type Entry struct {
	ID     string   `json:"id"`     // "Derrity/tinyserve"
	Source string   `json:"source"` // "github:Derrity/tinyserve" —— 真相永远是这个
	Desc   string   `json:"desc"`
	Tags   []string `json:"tags,omitempty"`
	Key    string   `json:"key,omitempty"` // 作者公钥 hex，认领了才有
	// Sig 是作者对 id + source + 那一份 toy.toml 原文的签名（base64），
	// 由 `toy publish` 生成。**有 key 没 sig 等于没签名**：一把谁都能抄进来的
	// 公钥证明不了任何事，客户端只在验过这个签名之后才敢打「✓ 已签名」（铁律 8）。
	Sig      string `json:"sig,omitempty"`
	Stars    int    `json:"stars,omitempty"`
	Installs int    `json:"installs,omitempty"`

	// Toyfile 是上游仓库【还没有】 toy.toml 时，由索引提供的那一份原文。
	// 索引冷启动的唯一出路：现存几千个 CLI 工具一个都没有 toy.toml，
	// 只存指针的索引对它们产出的是一堆装不上的条目。
	//
	// 四条边界（少一条这个设计就不成立）：
	//   1. 上游永远优先 —— 只有 raw URL 取 toy.toml 明确不存在时才用这一份，
	//      上游一旦有了自己的就无条件让位。否则一条恶意条目就能覆盖作者的声明。
	//   2. 它永远达不到 TrustSigned。签名覆盖的是提交这条记录的人写的内容，
	//      签名者不是作者 —— 打上勾正是我们一直在避免的那种说过头（§14.3）。
	//      条目同时有 toyfile 和 key/sig 时，签名只证明这条记录没被改过，
	//      档位仍然是 store.TrustIndexed。
	//   3. 校验和上游那份一模一样（下面 validEntry 里就调 toyfile.ParseFrom），
	//      不因为"是我们自己索引里的"走一条更松的路。
	//   4. 资产名仍然只是资产名，下载地址照旧由 host 模板 + tag 拼。所以它的
	//      影响半径被夹在「作者自己那个 release 里的某个资产」+「PATH 上的命令名」
	//      之间，而后者已经有 store 的保留名单和遮蔽检查兜着。**这就是这个设计
	//      能成立的原因** —— 索引能改的是「装哪个」，不是「从哪装」。
	Toyfile string `json:"toyfile,omitempty"`
}

type Index struct {
	Version  int     `json:"version"`
	Name     string  `json:"-"` // 从配置来的本地名字，不进文件
	Packages []Entry `json:"packages"`
}

// Load 解析索引 JSON 并校验：version 必须是 1，id 必须是 owner/repo 形态，
// source 必须能被 ref.Parse 解析。坏条目跳过而不是让整个索引失败 ——
// 一条烂记录不该让整个搜索瘫掉。返回被跳过的条数。
//
// 「坏」另外还包括两种：source 解析出来是索引形态（见 validEntry），
// 以及同一个 ID 的第二条起（见循环里那段注释）。
func Load(data []byte, name string) (*Index, int, error) {
	var raw Index
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, fmt.Errorf("index %s: invalid JSON: %w", name, err)
	}
	if raw.Version != Version {
		return nil, 0, fmt.Errorf("index %s: format version %d, but this client only understands version %d — upgrade toy or use another index",
			name, raw.Version, Version)
	}

	out := &Index{Version: raw.Version, Name: name, Packages: make([]Entry, 0, len(raw.Packages))}
	seen := make(map[string]bool, len(raw.Packages))
	skipped := 0
	for _, e := range raw.Packages {
		if !validEntry(e) {
			skipped++
			continue
		}
		// 同一个 ID 出现两次的索引是有歧义的：Find 只看得到第一条，
		// Remove 删掉第一条之后还会返回 true 而那个 ID 仍在索引里
		// （「删了」是句假话）。第二条起一律当坏记录跳过并计数。
		// 按 foldID 比：Derrity/tinyserve 和 derrity/tinyserve 是同一个仓库，
		// 留着两条等于把上面那个歧义原样保留下来。
		if seen[foldID(e.ID)] {
			skipped++
			continue
		}
		seen[foldID(e.ID)] = true
		out.Packages = append(out.Packages, e)
	}
	return out, skipped, nil
}

// validEntry 是「这条记录能不能用」的全部判据。
func validEntry(e Entry) bool {
	if !validID(e.ID) || e.Source == "" {
		return false
	}
	r, err := ref.Parse(e.Source)
	if err != nil {
		return false
	}
	// source 是这条记录的真相（规格 §5），必须指向一个真能去取东西的地方。
	// 写成裸 owner/repo 会让解析回头再查一次索引 —— 那是个环，不是个来源。
	if r.Kind == ref.KindIndex {
		return false
	}
	// 索引本身是**远端内容**，而这一行决定客户端接下来去打开什么。放行 file://
	// 等于让任何一份索引（谁都能 fork、谁都能提 PR）指使客户端去读本机的
	// ~/.aws/credentials；放行明文 http 等于放行 169.254.169.254 这类只在受害者
	// 机器上才有意义的地址，而且路上谁都能改。托管方那条路（github: 之类）不受
	// 影响 —— 它的地址是我们自己的 host 表拼的，不是索引说了算。
	if r.Kind == ref.KindURL && !strings.HasPrefix(strings.ToLower(r.URL), "https://") {
		return false
	}
	// 内联的 toyfile 在这里就完整解析一遍，坏的当场判坏记录。留到安装时才炸的话，
	// 用户已经在等下载了，而且报错会长得像上游仓库的问题 —— 实际上错的是索引。
	// 校验用的就是上游那条路上的同一个函数，一条都不放松（Entry.Toyfile 第 3 条）。
	if e.Toyfile != "" {
		if _, err := toyfile.ParseFrom([]byte(e.Toyfile), toyfile.OriginIndex); err != nil {
			return false
		}
	}
	return true
}

// validID 判断 "owner/repo" 形态。规则直接借 ref 的，不自己再抄一份白名单：
// 这两段最后会拼成 ~/.toy/pkg/<host>/<owner>/<name>/（规格 §6），
// 而 ref 已经在那里拦了 "." ".." 和超长段。抄一份的下场就是慢慢分叉 ——
// 上一版真的放行了 "../x" 和 "a/.."。
func validID(id string) bool {
	r, err := ref.Parse(id)
	// r.ID() == id 顺带钉死了两件事：不许带 @version，不许有首尾空白
	// （ref.Parse 会把空白吃掉，而带空白的 ID 会让 Find 和去重都对不上）。
	return err == nil && r.Kind == ref.KindIndex && r.ID() == id
}

func (i *Index) Marshal() ([]byte, error) {
	out := *i
	if out.Version == 0 {
		out.Version = Version
	}
	if out.Packages == nil {
		out.Packages = []Entry{} // 空索引要写成 []，不是 null
	}
	// 缩进 + 结尾换行：索引文件是要进 git 的，diff 得好读。
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("index %s: cannot serialize: %w", i.Name, err)
	}
	return append(b, '\n'), nil
}

// foldID 是 ID 的比较形式。GitHub / GitLab 的 owner 和 repo 本来就是大小写
// 不敏感的，所以 `toy install derrity/tinyserve` 必须找得到索引里的
// Derrity/tinyserve —— 否则用户拿到的是一句「查不到这个包」，而包就在那儿。
//
// 折叠只发生在比较这一侧：ref 那边必须原样保留大小写，raw URL 和 release
// 下载地址是按原样拼的，小写化会拼出 404。
func foldID(id string) string { return strings.ToLower(id) }

// Find 按 ID 找条目，大小写不敏感（见 foldID）。
func (i *Index) Find(id string) (Entry, bool) {
	want := foldID(id)
	for _, e := range i.Packages {
		if foldID(e.ID) == want {
			return e, true
		}
	}
	return Entry{}, false
}

// Upsert 按 ID 插入或替换，保持 Packages 按 ID 排序（这样索引文件的 diff 才好读）。
func (i *Index) Upsert(e Entry) {
	for n := range i.Packages {
		if foldID(i.Packages[n].ID) == foldID(e.ID) {
			i.Packages[n] = e
			return // 原地替换，位置不变，排序也就不变
		}
	}
	i.Packages = append(i.Packages, e)
	sort.Slice(i.Packages, func(a, b int) bool { return i.Packages[a].ID < i.Packages[b].ID })
}

func (i *Index) Remove(id string) bool {
	want := foldID(id)
	for n, e := range i.Packages {
		if foldID(e.ID) == want {
			i.Packages = append(i.Packages[:n], i.Packages[n+1:]...)
			return true
		}
	}
	return false
}

type Result struct {
	Entry Entry
	Index string // 来自哪个索引
	Score int
}

// 排序档位。数值本身没有意义，只有大小关系有。
const (
	scoreExactID  = 100 // ID 完全匹配
	scoreIDPrefix = 80  // ID 前缀
	scoreName     = 60  // 名字（斜杠后那段）子串
	scoreDesc     = 40  // 描述子串
	scoreTag      = 20  // tag 命中
)

// Search 在多个索引里搜。排序：ID 完全匹配 > ID 前缀 > 名字子串 > 描述子串 > tag 命中，
// 同分再按 Installs 降序。不区分大小写。空 query 返回全部（按 Installs 降序）。
// 同一个 ID 在多个索引里出现时只保留第一个索引的（配置里靠前的赢）。
func Search(idxs []*Index, query string, limit int) []Result {
	q := strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]bool)

	var out []Result
	for _, idx := range idxs {
		if idx == nil {
			continue
		}
		for _, e := range idx.Packages {
			if seen[foldID(e.ID)] {
				continue // 靠前的索引已经给过这个 ID 了
			}
			// 占坑要在打分【之前】：沾不沾这次查询都算「出现过」。
			// 放在后面的话，靠后的索引只要把 desc/tag 写得沾一点，
			// 就能在靠前索引那条不沾的查询里把同一个 ID 换成自己的 source。
			seen[foldID(e.ID)] = true
			score := scoreOf(e, q)
			if q != "" && score == 0 {
				continue
			}
			out = append(out, Result{Entry: e, Index: idx.Name, Score: score})
		}
	}

	// 稳定排序：全平的时候保持「索引顺序 + 索引内顺序」，结果才是可复现的。
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].Entry.Installs > out[b].Entry.Installs
	})

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scoreOf 取命中的最高一档。q 已经是小写且去过空白；q 为空时一律 0。
func scoreOf(e Entry, q string) int {
	if q == "" {
		return 0
	}
	id := strings.ToLower(e.ID)
	switch {
	case id == q:
		return scoreExactID
	case strings.HasPrefix(id, q):
		return scoreIDPrefix
	}
	name := id
	if _, repo, ok := strings.Cut(id, "/"); ok {
		name = repo
	}
	if strings.Contains(name, q) {
		return scoreName
	}
	if strings.Contains(strings.ToLower(e.Desc), q) {
		return scoreDesc
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return scoreTag
		}
	}
	return 0
}
