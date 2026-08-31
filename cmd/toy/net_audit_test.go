package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// 原本怀疑：一个远程的 toy.toml 能把 [artifact] 写成 file:///…，于是 toy 去读
// 本机的任意文件，把它当二进制装进 ~/.toy/pkg、软链进 PATH，还顺手缓存进
// ~/.toy/cache/<sha256>（那正是 `toy serve` 对外提供的目录）。
//
// 这条现在到不了 —— 另一条修复（toyfile 拒绝把 artifact/script 的值写成 URL）
// 把它顺手堵上了：地址只能由 toy 按托管方自己拼，写死 URL 一律在解析期就拒。
// 翻成回归测试：这个拒绝必须发生，而且错误要说清楚作者该怎么改。
func TestArtifactValueCannotBeAURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	toml := fmt.Sprintf("name = \"pwn\"\n[bin]\npwn = \"id_rsa\"\n[artifact]\n%q = \"file://%s\"\n",
		runtime.GOOS+"/"+runtime.GOARCH, secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, toml)
	}))
	defer srv.Close()

	err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"})
	if err == nil {
		t.Fatalf("远程 toy.toml 把 [artifact] 写成 file://%s，toy 照读不误 —— "+
			"值只能是 release 里的资产名，地址由 toy 自己拼", secret)
	}
	if !errors.Is(err, toyfile.ErrUnsafeTarget) {
		t.Fatalf("拒是拒了，但不是因为那个值不安全（错误里认不出 ErrUnsafeTarget），"+
			"换个 toy.toml 就可能又漏过去：%v", err)
	}
	// 作者会看到这句话，它得说清楚问题在哪、该写什么。
	for _, want := range []string{"URL", "资产名"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q，作者看不出该改什么：%v", want, err)
		}
	}
	// 什么都不该落地。
	if _, err := os.Stat(filepath.Join(home, "bin", "pwn")); !os.IsNotExist(err) {
		t.Errorf("拒绝了却还是建出了软链：%v", err)
	}
}

// 原本怀疑：裸地址里的用户名密码会被原样写进安装清单和 TOFU 记录（明文落盘），
// 还会出现在 `toy list` 的「来源」一列上。
//
// 这个装法现在同样到不了 —— 它靠的也是把 artifact 值写成 file://（见上一条）。
// 翻成回归测试：带凭据的裸地址必须在解析期就被拒，那串密码既不落盘、
// 也不出现在用户看到的任何一行输出里（错误信息会进日志、进 issue，比落盘更容易扩散）。
func TestCredentialsInBareURLNeverLandOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	payload := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = \"file://%s\"\n",
		runtime.GOOS+"/"+runtime.GOARCH, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, toml)
	}))
	defer srv.Close()

	u := strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1) + "/toy.toml"
	err := runInstall(context.Background(), []string{u})
	if err == nil {
		t.Fatal("这份 toy.toml 把 [artifact] 写成了 file://，本该在解析期就被拒")
	}
	// 凭据也不许出现在用户看到的输出里。抹掉是 internal/ui 那一层的活
	// （safe() → redactURLs，"堵源头只要堵一次"），这里走 main 走的那条路验一遍：
	// 在 cmd 里再抹一次只会多一份会跟它分叉的实现。
	out := captureStderr(t, func() { ui.Error("%v", err) })
	if strings.Contains(out, "hunter2") {
		t.Errorf("裸地址上的密码被原样打给用户了（会进日志、进 issue）：\n%s", out)
	}

	var hits []string
	filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, e := os.ReadFile(p); e == nil && strings.Contains(string(b), "hunter2") {
			hits = append(hits, p)
		}
		if strings.Contains(p, "hunter2") {
			hits = append(hits, p+"（文件名里）")
		}
		return nil
	})
	if len(hits) > 0 {
		t.Errorf("密码明文落在这些文件里：%v", hits)
	}
}

// 审计：toy serve 的路径处理和 Range。结论应该是全部 404 / 416，不越界。
func TestAuditMirrorHandlerPaths(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644)
	name := strings.Repeat("ab", 32)
	os.WriteFile(filepath.Join(dir, name), []byte("hello-mirror"), 0o644)
	h := autMirrorHandler(dir)

	for _, p := range []string{
		"/../secret.txt", "/%2e%2e/secret.txt", "/", "//etc/passwd", "/secret.txt",
		"/" + strings.ToUpper(name), "/" + name + "/../" + name,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://x"+p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%q → %d %q", p, w.Code, w.Body.String())
		}
	}
	for _, rng := range []string{"bytes=0-99999999999999999999", "bytes=-9223372036854775808", "bytes=18446744073709551615-"} {
		req := httptest.NewRequest(http.MethodGet, "http://x/"+name, nil)
		req.Header.Set("Range", rng)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK && w.Code != http.StatusRequestedRangeNotSatisfiable && w.Code != http.StatusPartialContent {
			t.Errorf("Range %q → %d", rng, w.Code)
		}
		if strings.Contains(w.Body.String(), "nope") {
			t.Errorf("Range %q 读到了别的文件", rng)
		}
	}
}
