package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 裸二进制（没有压缩后缀的 release 资产，比如 jq-linux-amd64 / yq_darwin_arm64）
// 必须落成 [bin] 指定的那个路径，不能落成资产名。
//
// 资产名按平台变，而 [bin] 只有一行、不按平台变。照资产名落盘的话，除了 [bin]
// 恰好写对的那一个平台，其余每个平台都会软链到一个不存在的文件 —— 而且是在
// 下载和校验都成功之后才炸。索引里 mikefarah/yq 就是这么在 6 个平台里坏了 5 个。
func TestBareBinaryLandsAtTheBinTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))

	// 资产名故意和 [bin] 左右两边都不一样，模拟「在别的平台上下下来的那个名字」。
	const asset = "tool_someotherplatform_v9"
	plat := runtime.GOOS + "/" + runtime.GOARCH
	toml := fmt.Sprintf("name = \"tool\"\n[bin]\ntool = \"tool\"\n[artifact]\n%q = %q\n", plat, asset)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/toy.toml" {
			w.Write([]byte(toml))
			return
		}
		w.Write([]byte("#!/bin/sh\necho ok\n"))
	}))
	defer srv.Close()

	if err := runInstall(context.Background(), []string{srv.URL + "/toy.toml"}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	link := filepath.Join(home, "bin", "tool")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("软链没建出来：%v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("软链指向一个不存在的文件 %s —— 裸二进制还是照资产名落的盘：%v", target, err)
	}
	if filepath.Base(target) != "tool" {
		t.Fatalf("落盘的名字应该是 [bin] 右边那个 tool，实际是 %s", filepath.Base(target))
	}
}
