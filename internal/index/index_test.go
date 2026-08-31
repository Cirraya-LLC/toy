package index

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/ref"
)

func TestLoadRejectsWholeFile(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"不是 JSON", "这不是 json"},
		{"空输入", ""},
		{"版本是 2", `{"version":2,"packages":[]}`},
		{"没写版本", `{"packages":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := Load([]byte(tt.data), "默认"); err == nil {
				t.Fatal("这份索引应该整体失败，却成功了")
			}
		})
	}
}

// 这些条目在到达 source 检查之前就被判坏（ID 形态不对，或 source 是空的）。
func TestLoadSkipsBadEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{"ID 没有斜杠", Entry{ID: "tinyserve", Source: "github:x/tinyserve"}},
		{"ID 两个斜杠", Entry{ID: "a/b/c", Source: "github:a/b"}},
		{"ID owner 为空", Entry{ID: "/tinyserve", Source: "github:x/tinyserve"}},
		{"ID repo 为空", Entry{ID: "Derrity/", Source: "github:Derrity/x"}},
		{"ID 是空串", Entry{ID: "", Source: "github:x/y"}},
		{"ID 有非法字符", Entry{ID: "Derr ity/tiny", Source: "github:x/y"}},
		{"ID 有中文", Entry{ID: "德睿/小服务", Source: "github:x/y"}},
		{"source 是空的", Entry{ID: "Derrity/tinyserve", Source: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(Index{Version: 1, Packages: []Entry{tt.entry}})
			if err != nil {
				t.Fatal(err)
			}
			idx, skipped, err := Load(data, "默认")
			if err != nil {
				t.Fatalf("一条烂记录不该让整个索引失败：%v", err)
			}
			if skipped != 1 {
				t.Errorf("skipped = %d，想要 1", skipped)
			}
			if len(idx.Packages) != 0 {
				t.Errorf("坏条目被留下了：%+v", idx.Packages)
			}
			if idx.Name != "默认" {
				t.Errorf("Name = %q，想要 %q", idx.Name, "默认")
			}
		})
	}
}

func TestLoadEmptyPackages(t *testing.T) {
	idx, skipped, err := Load([]byte(`{"version":1,"packages":[]}`), "空索引")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(idx.Packages) != 0 {
		t.Fatalf("skipped=%d packages=%d，想要 0/0", skipped, len(idx.Packages))
	}
	if idx.Version != 1 {
		t.Errorf("Version = %d，想要 1", idx.Version)
	}
}

func TestLoadKeepsGoodEntriesAndCountsSkips(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"Derrity/tinyserve","source":"github:Derrity/tinyserve","desc":"好的"},
		{"id":"没有斜杠","source":"github:a/b","desc":"坏 ID"},
		{"id":"Derrity/other","source":"","desc":"空 source"},
		{"id":"Other/ok","source":"github:Other/ok","desc":"也是好的"}
	]}`)
	idx, skipped, err := Load(data, "默认")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d，想要 2", skipped)
	}
	if len(idx.Packages) != 2 {
		t.Fatalf("留下了 %d 条，想要 2：%+v", len(idx.Packages), idx.Packages)
	}
	if _, ok := idx.Find("Derrity/tinyserve"); !ok {
		t.Error("好条目 Derrity/tinyserve 不见了")
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"Derrity/tinyserve", true},
		{"a/b", true},
		{"a.b_c-d/e.f_g-h", true},
		{"UPPER/Case9", true},
		{"", false},
		{"tinyserve", false},
		{"a/b/c", false},
		{"/b", false},
		{"a/", false},
		{"a b/c", false},
		{"a/b c", false},
		{"a@b/c", false},
		{"德睿/工具", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := validID(tt.id); got != tt.want {
				t.Errorf("validID(%q) = %v，想要 %v", tt.id, got, tt.want)
			}
		})
	}
}

// 每一档命中都单独钉一次，顺序不能变。
func TestScoreTiers(t *testing.T) {
	e := Entry{
		ID:   "Derrity/tinyserve",
		Desc: "一个很小的静态文件服务器 with HTTP",
		Tags: []string{"web", "server"},
	}
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"ID 完全匹配", "derrity/tinyserve", scoreExactID},
		{"ID 完全匹配忽略大小写", "DERRITY/TINYSERVE", scoreExactID},
		{"ID 前缀", "derrity/tiny", scoreIDPrefix},
		{"ID 前缀是 owner", "derr", scoreIDPrefix},
		{"名字子串", "tinyserv", scoreName},
		{"名字子串在中间", "nyser", scoreName},
		{"描述子串", "静态文件", scoreDesc},
		{"描述子串忽略大小写", "http", scoreDesc},
		{"tag 命中", "web", scoreTag},
		{"tag 命中忽略大小写", "SERVER", scoreTag},
		{"什么都不沾", "postgres", 0},
		{"空 query", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scoreOf(e, strings.ToLower(tt.query)); got != tt.want {
				t.Errorf("scoreOf(%q) = %d，想要 %d", tt.query, got, tt.want)
			}
		})
	}
}

func TestScoreTakesTheHighestTier(t *testing.T) {
	// 这条记录 ID 前缀、名字、描述、tag 全都命中 "tiny"，只能算最高那一档。
	e := Entry{ID: "tiny/tinyserve", Desc: "tiny", Tags: []string{"tiny"}}
	if got := scoreOf(e, "tiny"); got != scoreIDPrefix {
		t.Errorf("scoreOf = %d，想要 %d（ID 前缀这一档）", got, scoreIDPrefix)
	}
}

func idsOf(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Entry.ID
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSearch(t *testing.T) {
	main := &Index{Version: 1, Name: "默认", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "静态文件服务器", Tags: []string{"web"}, Installs: 5},
		{ID: "Derrity/tinytool", Desc: "另一个工具", Installs: 50},
		{ID: "Other/serve", Desc: "提到了 tinyserve 三个字", Installs: 100},
		{ID: "Third/thing", Desc: "毫无关系", Tags: []string{"tinyserve"}, Installs: 1000},
	}}

	tests := []struct {
		name  string
		query string
		limit int
		want  []string
	}{
		{
			name:  "各档排序",
			query: "tinyserve",
			// 完全匹配没有；ID 前缀没有；名字子串 tinyserve > 描述 > tag
			want: []string{"Derrity/tinyserve", "Other/serve", "Third/thing"},
		},
		{
			name:  "同分（都是 ID 前缀）按 Installs 降序",
			query: "derrity/tiny",
			want:  []string{"Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "名字 > 描述 > tag，同为名字子串再按 Installs",
			query: "tiny",
			want: []string{
				"Derrity/tinytool",  // 名字 60，装机 50
				"Derrity/tinyserve", // 名字 60，装机 5
				"Other/serve",       // 描述 40
				"Third/thing",       // tag 20，装机 1000 也压不过档位
			},
		},
		{
			name:  "空 query 返回全部并按 Installs 降序",
			query: "",
			want:  []string{"Third/thing", "Other/serve", "Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "只有空白也算空 query",
			query: "   ",
			want:  []string{"Third/thing", "Other/serve", "Derrity/tinytool", "Derrity/tinyserve"},
		},
		{
			name:  "limit 截断",
			query: "tinyserve",
			limit: 2,
			want:  []string{"Derrity/tinyserve", "Other/serve"},
		},
		{
			name:  "limit 为 0 表示不限",
			query: "tinyserve",
			limit: 0,
			want:  []string{"Derrity/tinyserve", "Other/serve", "Third/thing"},
		},
		{
			name:  "一条都不沾",
			query: "postgres",
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idsOf(Search([]*Index{main}, tt.query, tt.limit))
			if !eq(got, tt.want) {
				t.Errorf("Search(%q, limit=%d) = %v，想要 %v", tt.query, tt.limit, got, tt.want)
			}
		})
	}
}

func TestSearchTierBeatsInstalls(t *testing.T) {
	idx := &Index{Name: "默认", Packages: []Entry{
		{ID: "Big/tiny-thing", Installs: 999999}, // 名字子串
		{ID: "tiny/thing", Installs: 1},          // ID 前缀，档位更高
	}}
	got := idsOf(Search([]*Index{idx}, "tiny", 0))
	want := []string{"tiny/thing", "Big/tiny-thing"}
	if !eq(got, want) {
		t.Errorf("ID 前缀该压过名字子串（装机量差 100 万倍也一样），实际 %v", got)
	}
}

func TestSearchExactIDWins(t *testing.T) {
	idx := &Index{Name: "默认", Packages: []Entry{
		// 装机量差 1000 倍也压不过完全匹配
		{ID: "Other/tinyserve-plus", Desc: "tinyserve", Installs: 100000},
		{ID: "Derrity/tinyserve", Installs: 1},
	}}
	got := idsOf(Search([]*Index{idx}, "Derrity/tinyserve", 0))
	if len(got) == 0 || got[0] != "Derrity/tinyserve" {
		t.Fatalf("完全匹配应该排第一，实际 %v", got)
	}
}

func TestSearchDedupKeepsTheEarlierIndex(t *testing.T) {
	first := &Index{Name: "官方", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "官方那份", Installs: 1},
	}}
	second := &Index{Name: "我的", Packages: []Entry{
		{ID: "Derrity/tinyserve", Desc: "我 fork 的那份", Installs: 9999},
		{ID: "Me/only-here", Desc: "tinyserve 相关"},
	}}

	got := Search([]*Index{first, second}, "tinyserve", 0)
	seen := 0
	for _, r := range got {
		if r.Entry.ID != "Derrity/tinyserve" {
			continue
		}
		seen++
		if r.Index != "官方" {
			t.Errorf("重复 ID 应该保留靠前索引的，实际来自 %q", r.Index)
		}
		if r.Entry.Desc != "官方那份" {
			t.Errorf("留下的是后面那条记录：%q", r.Entry.Desc)
		}
	}
	if seen != 1 {
		t.Errorf("同一个 ID 出现了 %d 次，想要 1 次", seen)
	}
	if len(got) != 2 {
		t.Errorf("结果 %d 条，想要 2 条（去重后）", len(got))
	}
}

func TestSearchIgnoresNilIndex(t *testing.T) {
	idx := &Index{Name: "默认", Packages: []Entry{{ID: "a/b", Desc: "x"}}}
	if got := Search([]*Index{nil, idx, nil}, "", 0); len(got) != 1 {
		t.Fatalf("nil 索引该被跳过，实际拿到 %d 条", len(got))
	}
	if got := Search(nil, "tiny", 0); len(got) != 0 {
		t.Fatalf("没有索引时该是空结果，实际 %v", got)
	}
}

func TestUpsertKeepsSorted(t *testing.T) {
	idx := &Index{Version: 1}
	for _, id := range []string{"Zed/z", "Alpha/a", "Mid/m", "Beta/b"} {
		idx.Upsert(Entry{ID: id, Source: "github:" + id})
	}
	want := []string{"Alpha/a", "Beta/b", "Mid/m", "Zed/z"}
	var got []string
	for _, e := range idx.Packages {
		got = append(got, e.ID)
	}
	if !eq(got, want) {
		t.Fatalf("Upsert 之后顺序 = %v，想要 %v", got, want)
	}
}

func TestUpsertReplacesInPlace(t *testing.T) {
	idx := &Index{Version: 1}
	idx.Upsert(Entry{ID: "a/b", Desc: "旧的", Installs: 1})
	idx.Upsert(Entry{ID: "c/d", Desc: "另一个"})
	idx.Upsert(Entry{ID: "a/b", Desc: "新的", Installs: 7})

	if len(idx.Packages) != 2 {
		t.Fatalf("同一个 ID 被插了两条：%+v", idx.Packages)
	}
	e, ok := idx.Find("a/b")
	if !ok {
		t.Fatal("a/b 不见了")
	}
	if e.Desc != "新的" || e.Installs != 7 {
		t.Errorf("没有被替换：%+v", e)
	}
	if idx.Packages[0].ID != "a/b" {
		t.Errorf("替换之后顺序乱了：%+v", idx.Packages)
	}
}

func TestFindAndRemove(t *testing.T) {
	idx := &Index{Version: 1, Packages: []Entry{
		{ID: "a/b", Desc: "一"},
		{ID: "c/d", Desc: "二"},
	}}
	if _, ok := idx.Find("a/b"); !ok {
		t.Error("Find 没找到 a/b")
	}
	if _, ok := idx.Find("nope/nope"); ok {
		t.Error("Find 找到了不存在的条目")
	}
	if !idx.Remove("a/b") {
		t.Error("Remove 该返回 true")
	}
	if idx.Remove("a/b") {
		t.Error("重复 Remove 该返回 false")
	}
	if len(idx.Packages) != 1 || idx.Packages[0].ID != "c/d" {
		t.Errorf("Remove 删错了：%+v", idx.Packages)
	}
}

func TestMarshal(t *testing.T) {
	idx := &Index{Version: 1, Name: "默认"}
	b, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "null") {
		t.Errorf("空 packages 应该写成 []，不是 null：%s", s)
	}
	if strings.Contains(s, "默认") {
		t.Errorf("Name 是本地的，不该进文件：%s", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("索引文件要以换行结尾，diff 才干净")
	}
	if !strings.Contains(s, "\n  ") {
		t.Errorf("索引文件要缩进，diff 才好读：%s", s)
	}

	idx.Upsert(Entry{ID: "z/z", Source: "github:z/z", Desc: "后"})
	idx.Upsert(Entry{ID: "a/a", Source: "github:a/a", Desc: "先"})
	b, err = idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(string(b), `"a/a"`), strings.Index(string(b), `"z/z"`); i < 0 || j < 0 || i > j {
		t.Errorf("文件里应该按 ID 排序：%s", b)
	}

	// 序列化出来的东西必须还能被自己读回去（这一段不碰 ref：只看 JSON 层）。
	var back Index
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("自己写的文件自己读不回来：%v", err)
	}
	if back.Version != 1 || len(back.Packages) != 2 {
		t.Errorf("读回来的不对：%+v", back)
	}
}

func TestMarshalFillsMissingVersion(t *testing.T) {
	b, err := (&Index{}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 1`) {
		t.Errorf("没写版本时该补成 1：%s", b)
	}
}

// 骨架注释说「source 必须能被 ref.Parse 解析」。空 source 之外，
// 真正畸形的 source 也得被拦下来 —— 这一档原来一条用例都没有。
func TestLoadSkipsUnparsableSource(t *testing.T) {
	for _, src := range []string{
		"这不是 ref",
		"github:a b/c",     // owner 里有空格
		"github:../x",      // 会变成目录名的路径穿越
		"ftp://x/toy.toml", // 不认的 scheme
		"a/b/c",
		"   ",
	} {
		t.Run(src, func(t *testing.T) {
			data, err := json.Marshal(Index{Version: 1, Packages: []Entry{{ID: "a/b", Source: src}}})
			if err != nil {
				t.Fatal(err)
			}
			idx, skipped, err := Load(data, "默认")
			if err != nil {
				t.Fatal(err)
			}
			if skipped != 1 || len(idx.Packages) != 0 {
				t.Errorf("source %q 该被跳过：skipped=%d 留下 %+v", src, skipped, idx.Packages)
			}
		})
	}
}

// source 是这条记录的真相（规格 §5），必须指向某个 host 或某个 URL。
// 写成裸 owner/repo 就是「查索引」——解析会绕回索引，那是个环。
func TestLoadSkipsSourceThatPointsBackAtTheIndex(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"a/b","source":"a/b"},
		{"id":"c/d","source":"c/d@v1.0.0"},
		{"id":"e/f","source":"github:e/f"}
	]}`)
	idx, skipped, err := Load(data, "默认")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d，想要 2（两条 source 指回索引）", skipped)
	}
	if len(idx.Packages) != 1 || idx.Packages[0].ID != "e/f" {
		t.Errorf("只该留下 github: 那条：%+v", idx.Packages)
	}
}

// 同一个 ID 两条记录会让 Find 和 Remove 说假话：Remove 返回 true，ID 却还在。
func TestLoadSkipsDuplicateIDs(t *testing.T) {
	data := []byte(`{"version":1,"packages":[
		{"id":"a/b","source":"github:a/b","desc":"真的"},
		{"id":"a/b","source":"github:evil/x","desc":"假的"},
		{"id":"c/d","source":"github:c/d"}
	]}`)
	idx, skipped, err := Load(data, "默认")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d，想要 1（第二条 a/b）", skipped)
	}
	if e, _ := idx.Find("a/b"); e.Desc != "真的" {
		t.Errorf("留下的该是第一条：%+v", e)
	}
	if !idx.Remove("a/b") {
		t.Fatal("Remove 该返回 true")
	}
	if _, ok := idx.Find("a/b"); ok {
		t.Error("Remove 说删了，a/b 却还在 —— 索引里有重复 ID")
	}
}

// validID 的规则必须和 ref 一致：这两段会变成 ~/.toy 下的目录名（规格 §6）。
func TestValidIDMatchesRefsSegmentRules(t *testing.T) {
	long := strings.Repeat("a", 120)
	for _, id := range []string{"../x", "a/..", "./x", "a/.", long + "/x", "x/" + long, " a/b", "a/b ", "a/b@v1"} {
		if validID(id) {
			t.Errorf("validID(%q) = true，但 ref 会拒绝它", id)
		}
		if _, err := ref.Parse(id); err == nil && id != " a/b" && id != "a/b " && id != "a/b@v1" {
			t.Errorf("前提变了：ref.Parse(%q) 现在不报错了", id)
		}
	}
}

// 靠后的索引不许把靠前索引的同名包换成自己的 source：
// 「配置里靠前的赢」说的是 ID 出现过就占坑，不是「命中了才占坑」。
func TestSearchLaterIndexCannotShadowAnEarlierID(t *testing.T) {
	official := &Index{Name: "官方", Packages: []Entry{
		{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "静态文件服务器"},
	}}
	evil := &Index{Name: "坏人", Packages: []Entry{
		// desc 特意写得沾 "web"，而官方那条不沾
		{ID: "Derrity/tinyserve", Source: "github:evil/backdoor", Desc: "web server", Installs: 9999},
	}}
	for _, r := range Search([]*Index{official, evil}, "web", 0) {
		if r.Entry.ID == "Derrity/tinyserve" && r.Index != "官方" {
			t.Fatalf("靠后的索引盖掉了同一个 ID：来自 %q，source=%s", r.Index, r.Entry.Source)
		}
	}
}

// 自己写出来的索引必须能被自己 Load 回去（不只是被 json 读回去）。
func TestMarshalRoundTripsThroughLoad(t *testing.T) {
	idx := &Index{Name: "本地名字"}
	idx.Upsert(Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve",
		Desc: "静态文件服务器", Tags: []string{"web"}, Key: "a3f9", Installs: 7})
	idx.Upsert(Entry{ID: "Other/ok", Source: "https://example.com/toy.toml", Desc: "裸 URL 也行"})

	b, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, skipped, err := Load(b, "读回来")
	if err != nil {
		t.Fatalf("自己写的索引自己 Load 不回来：%v", err)
	}
	if skipped != 0 {
		t.Errorf("自己写的条目被自己判成坏的 %d 条", skipped)
	}
	if len(back.Packages) != 2 {
		t.Fatalf("读回 %d 条，想要 2：%+v", len(back.Packages), back.Packages)
	}
	e, ok := back.Find("Derrity/tinyserve")
	if !ok || e.Installs != 7 || e.Key != "a3f9" || len(e.Tags) != 1 {
		t.Errorf("字段在往返中丢了：%+v", e)
	}
	if back.Name != "读回来" {
		t.Errorf("Name 该来自参数而不是文件：%q", back.Name)
	}
}
