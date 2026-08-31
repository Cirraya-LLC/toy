package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func audReinstallTGZ(t *testing.T, names []string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: 4})
		tw.Write([]byte("BODY"))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// 审计：insPerform 里那条「下载 + TOFU 全部做完再动磁盘上的包目录：中途拒绝的话，
// 已经装着的那一版必须原封不动」只覆盖了下载和 TOFU 两步。
//
// 真正动手的顺序是：insRemovePkgDir(pkgDir) → MkdirAll → insUnpack。
// pkgDir 在 --force 重装（以及裸地址那条永远是 unversioned 的路）上和已装的那一版
// 是同一个目录，所以解压一失败，已经在 PATH 上跑着的那一版就没了 ——
// 而清单还在、`toy list` 照样列它、bin/ 里那条软链变成悬空。
func TestFailedReinstallWipesTheWorkingCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	good := audReinstallTGZ(t, []string{"tool"})
	bad := audReinstallTGZ(t, []string{"tool", "../../../../pwned"}) // 解压会 ErrTraversal
	payload := good
	plat := runtime.GOOS + "/" + runtime.GOARCH
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = \"tool.tar.gz\"\n", plat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			w.Write([]byte(toml))
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		t.Fatalf("第一次安装就失败了：%v", err)
	}
	link := filepath.Join(home, "bin", "tool")
	if b, err := os.ReadFile(link); err != nil || string(b) != "BODY" {
		t.Fatalf("第一次装完就不对：%v %q", err, b)
	}

	payload = bad
	err := runInstall(context.Background(), []string{"--force", srv.URL + "/toy.toml"})
	t.Logf("重装的结果：%v", err)
	if b, rerr := os.ReadFile(link); rerr != nil {
		t.Fatalf("一次失败的重装把已经装好的那一版毁了：%s 现在读不出来（%v）"+
			"，而清单还在、toy list 照样列它", link, rerr)
	} else {
		t.Logf("还在：%q", b)
	}
}
