// Package index 是索引的读写和搜索。规格 §5：索引只存指针，不存实质内容。
package index

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
func Load(data []byte, name string) (*Index, int, error) { panic("todo") }

func (i *Index) Marshal() ([]byte, error) { panic("todo") }

func (i *Index) Find(id string) (Entry, bool) { panic("todo") }

// Upsert 按 ID 插入或替换，保持 Packages 按 ID 排序（这样索引文件的 diff 才好读）。
func (i *Index) Upsert(e Entry) { panic("todo") }

func (i *Index) Remove(id string) bool { panic("todo") }

type Result struct {
	Entry Entry
	Index string // 来自哪个索引
	Score int
}

// Search 在多个索引里搜。排序：ID 完全匹配 > ID 前缀 > 名字子串 > 描述子串 > tag 命中，
// 同分再按 Installs 降序。不区分大小写。空 query 返回全部（按 Installs 降序）。
// 同一个 ID 在多个索引里出现时只保留第一个索引的（配置里靠前的赢）。
func Search(idxs []*Index, query string, limit int) []Result { panic("todo") }
