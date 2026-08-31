// Package index 是索引的读写和搜索。规格 §5：索引只存指针，不存实质内容。
package index

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/ref"
)

// Version 是本客户端认识的索引格式版本。规格 §5 的 JSON 里那个 "version": 1。
const Version = 1

type Entry struct {
	ID       string   `json:"id"`     // "Derrity/tinyserve"
	Source   string   `json:"source"` // "github:Derrity/tinyserve" —— 真相永远是这个
	Desc     string   `json:"desc"`
	Tags     []string `json:"tags,omitempty"`
	Key      string   `json:"key,omitempty"` // 作者公钥 hex，认领了才有
	Stars    int      `json:"stars,omitempty"`
	Installs int      `json:"installs,omitempty"`
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
		return nil, 0, fmt.Errorf("索引 %s：JSON 解析失败：%w", name, err)
	}
	if raw.Version != Version {
		return nil, 0, fmt.Errorf("索引 %s：版本是 %d，本客户端只认 version %d，请升级 toy 或换一个索引",
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
		if seen[e.ID] {
			skipped++
			continue
		}
		seen[e.ID] = true
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
	return r.Kind != ref.KindIndex
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
		return nil, fmt.Errorf("索引 %s：序列化失败：%w", i.Name, err)
	}
	return append(b, '\n'), nil
}

func (i *Index) Find(id string) (Entry, bool) {
	for _, e := range i.Packages {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Upsert 按 ID 插入或替换，保持 Packages 按 ID 排序（这样索引文件的 diff 才好读）。
func (i *Index) Upsert(e Entry) {
	for n := range i.Packages {
		if i.Packages[n].ID == e.ID {
			i.Packages[n] = e
			return // 原地替换，位置不变，排序也就不变
		}
	}
	i.Packages = append(i.Packages, e)
	sort.Slice(i.Packages, func(a, b int) bool { return i.Packages[a].ID < i.Packages[b].ID })
}

func (i *Index) Remove(id string) bool {
	for n, e := range i.Packages {
		if e.ID == id {
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
			if seen[e.ID] {
				continue // 靠前的索引已经给过这个 ID 了
			}
			// 占坑要在打分【之前】：沾不沾这次查询都算「出现过」。
			// 放在后面的话，靠后的索引只要把 desc/tag 写得沾一点，
			// 就能在靠前索引那条不沾的查询里把同一个 ID 换成自己的 source。
			seen[e.ID] = true
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
