package main

// 铁律 8 的审计：`toy list` 的信任列是用户唯一的判断依据。
// 这一条不需要任何控制字符 —— 光靠"name 没有长度上限"就能让它说谎。

import (
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/store"
)

// 一个包能用自己的 name 在 `toy list` 里伪造出一整行，
// 并且把【所有】包的真实信任列顶到屏幕外面去。
//
// qryTable 用 tabwriter，列宽 = 该列最宽那格 + 2，而 name 一路来自 toy.toml
// （toyfile.checkName 只拒控制字符和路径分隔符：没有长度上限，也不管里面有
// 多少连续空格），经 store.Manifest.Name 到这一格，全程没有截断 ——
// `toy search` 那边的 qryTrunc(qryDescWidth) 只截描述列，list 一列都不截。
//
// 后果：name 里塞一段自己排好的 "版本 / ✓ 已签名 / 来源"，后面补空格补到超过
// 终端宽度，于是在任何窄于 name 的终端上：
//   - 这个包显示成 "cowsay  v3.2.1  ✓ 已签名 9f3c1a20  Derrity/cowsay"
//   - 它真正的 "⚠ 未签名" 被推到换行之后（`toy list | less -S`、tmux 里直接截掉）
//   - 顺带把同一张表里 jq 那种正经包的信任列也一起顶出去
func TestListNameCanForgeItsOwnRowAndHideEveryTrustColumn(t *testing.T) {
	const width = 100 // 一个很常见的终端宽度

	forged := "cowsay    v3.2.1  " + qryTrustLabel(store.TrustSigned, "9f3c1a20") + "  Derrity/cowsay"
	name := forged + strings.Repeat(" ", 3*width) // 补白到超出终端宽度

	rows := [][]string{
		{"名字", "版本", "信任", "来源"},
		{"jq", "1.7.1", qryTrustLabel(store.TrustSigned, "aabb1122"), "github:jqlang/jq"},
		{name, "0.0.1", qryTrustLabel(store.TrustUnverified, ""), "https://evil.example/toy.toml"},
	}

	out := captureStderr(t, func() { qryTable(rows) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("表格应该是 3 行，得到 %d 行：%q", len(lines), lines)
	}
	legit, evil := lines[1], lines[2]

	// 断言的是【应该有的】行为，所以今天它是红的：一行表格里，信任列必须落在
	// 一个常规终端看得见的地方，而 name 不许把它顶出去。
	head := visibleCols(evil, width)
	if !strings.Contains(head, "未签名") {
		t.Errorf("恶意包的真实信任等级被顶出了第 %d 列，第一屏看到的是一条伪造的已签名记录：\n  %s", width, head)
	}
	if strings.Contains(head, "✓ 已签名 9f3c1a20") {
		t.Errorf("name 里塞的假信任等级被当成正文渲染出来了：\n  %s", head)
	}
	// 附带伤害：同一张表里正经包的信任列也一起被顶出去了 —— 一个恶意包
	// 就能让【所有】包的信任等级从屏幕上消失。
	if !strings.Contains(visibleCols(legit, width), "aabb1122") {
		t.Errorf("jq 是正经包，它的信任列也被那个 name 顶出了第 %d 列：\n  %s", width, visibleCols(legit, width))
	}
}

// visibleCols 取一行在宽度 width 的终端上第一屏看得到的部分（按 rune 近似，
// 中日韩字符实际占两格，所以真实情况只会比这更糟）。
func visibleCols(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

// captureStderr 换掉 fd 2。ui.out 在 init 时就绑住了 os.Stderr 这个 *os.File，
// 所以只能从文件描述符这一层截，赋值 os.Stderr 是没用的。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(int(w.Fd()), 2, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	_ = syscall.Dup3(saved, 2, 0)
	_ = syscall.Close(saved)
	w.Close()
	s := <-done
	r.Close()
	return s
}
