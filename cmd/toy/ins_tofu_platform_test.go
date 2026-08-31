package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/sign"
)

// TOFU 的钉住记录必须按【资产名】分开记，不能只按 ref@version 记。
//
// 起因：同一个 ref@version 在 musl 机器和 glibc 机器上命中的是不同的候选
// （linux/amd64/musl vs linux/amd64），拿到的是两份完全不同的字节。共享 $HOME
// （NFS home、dotfiles 同步 ~/.toy）时，第二台机器会被报成「内容被人换掉了」——
// 一个吓人的假警报，而且正好出现在最不该狼来了的地方。
//
// 资产名天然就是平台判别符，所以拼它就够了，不用引入「平台」这个新概念。
func TestTOFUPinsPerAssetNotPerVersion(t *testing.T) {
	plat := runtime.GOOS + "/" + runtime.GOARCH
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	asset := "tool_gnu.tar.gz"
	body := audReinstallTGZ(t, []string{"tool"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			fmt.Fprintf(w, "name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = %q\n", plat, asset)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	ref := srv.URL + "/toy.toml"
	if err := runInstall(context.Background(), []string{ref}); err != nil {
		t.Fatalf("第一次安装失败：%v", err)
	}

	// 换一个资产名 + 换掉字节，模拟「同一个包，另一台 libc 不同的机器」。
	asset = "tool_musl.tar.gz"
	body = audReinstallTGZ(t, []string{"tool", "extra"})
	if err := runInstall(context.Background(), []string{"--reinstall", ref}); err != nil {
		if errors.Is(err, sign.ErrChanged) {
			t.Fatalf("换了资产名（等于换了平台）被误报成内容遭篡改 —— "+
				"TOFU 的 id 没有把资产名算进去：%v", err)
		}
		t.Fatalf("安装失败：%v", err)
	}

	// 反过来：资产名不变而字节变了，那才是真的要拦。
	body = audReinstallTGZ(t, []string{"tool", "another"})
	err := runInstall(context.Background(), []string{"--reinstall", ref})
	if !errors.Is(err, sign.ErrChanged) {
		t.Fatalf("同一个资产名换了字节，必须报 ErrChanged，实际是：%v", err)
	}
}
