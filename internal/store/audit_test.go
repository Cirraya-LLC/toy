package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 审计：用 --force 装 B 顶掉 A 的同名命令之后，A 的清单里仍然记着那条软链。
// `toy uninstall A` 会照着自己的清单把 bin/tool 删掉 —— 而那条链现在指着 B。
// B 的清单一个字没动，`toy list` 照样说 B 装着，只是它的命令从 PATH 上消失了，
// 而且 doctor 也发现不了（那不是悬空软链，是根本不存在）。
//
// 修法在 Link：软链换主人的那一刻就把过期记录从旧主人的清单里摘掉。
// （Unlink 只拿得到一条路径，没有「这是谁的包」这个信息 —— 它的签名是别的包
// 按着写的，改不了；而且 insLink 回滚时清单压根还没落盘，靠清单反查会把
// 回滚变成「删不掉」。所以判据只能在换手那一刻。）
func TestUninstallDeletesAnotherPackagesLink(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linkOf := func(id, dir string, force bool) string {
		pkg := filepath.Join(s.Root, dirPkg, dir)
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(pkg, "tool")
		if err := os.WriteFile(target, []byte(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		link, err := s.Link("tool", target, force)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveManifest(Manifest{ID: id, Name: dir, PkgDir: pkg, Links: []string{link}}); err != nil {
			t.Fatal(err)
		}
		return link
	}

	linkOf("local:o/a", "a", false)
	linkB := linkOf("local:o/b", "b", true) // B 用 --force 抢下了 "tool"

	// 卸载 A：照它此刻磁盘上的那份清单删软链（runUninstall 就是这么做的）。
	a, err := s.LoadManifest("local:o/a")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range a.Links {
		if err := s.Unlink(l); err != nil {
			t.Fatal(err)
		}
	}

	dest, err := os.Readlink(linkB)
	if err != nil {
		t.Fatalf("卸载 A 把 B 的命令 %s 删掉了 —— A 的清单在软链换主人的时候就该"+
			"不再记着它：%v", linkB, err)
	}
	if filepath.Base(filepath.Dir(dest)) != "b" {
		t.Fatalf("%s 指向 %s，不是 B 的包目录", linkB, dest)
	}
}
