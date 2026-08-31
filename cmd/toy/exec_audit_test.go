package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 审计（端到端）：一个包能把 ~/.toy/bin/git 占下来，而 toy 自己 exec 的就是
// PATH 上的 "git"。装的那一刻确实一行都没跑（铁律 1 的字面意思成立），
// 但下一条 `toy install github:…` / `toy update` / `toy doctor` 就会执行它。
//
// 前提只有一个：安装的时候机器上还没有 git（LookPath 找不到 → 遮蔽检查放行）。
// 而裸地址那条路压根不需要 git，所以这个前提是自洽的。
func TestBareURLPackageCanClaimTheGitCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	// 一台还没装 git 的机器。BinDir 按 doctor 教的放在 PATH 最前面。
	t.Setenv("PATH", filepath.Join(home, "bin")+string(os.PathListSeparator)+filepath.Join(t.TempDir(), "empty"))

	payload := "#!/bin/sh\necho pwned\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/toy.toml":
			w.Write([]byte("name = \"helper\"\ndesc = \"d\"\n[script]\ngit = \"payload.sh\"\n"))
		case "/payload.sh":
			w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		return // 被拒了就是好事
	}
	link := filepath.Join(home, "bin", "git")
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("没建出来？%v", err)
	}
	if string(got) == payload {
		t.Fatalf("装完之后 %s 就是包里的脚本，而 toy 的 host.Tags / autGitRemote / doctor "+
			"都从 PATH 解析 \"git\" —— 下一条 toy 命令就会执行它。安装过程本身没有任何警告", link)
	}
}
