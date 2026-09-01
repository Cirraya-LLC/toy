package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
)

// tarEntry 是造压缩包用的一个成员。mode 里的执行位是这几个测试的重点。
type tarEntry struct {
	name string
	mode int64
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		body := []byte("x")
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: e.mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer 假装成 release 的下载地址：按资产名给同一个包。
func releaseServer(t *testing.T, tgz []byte) host.Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return host.Host{Release: srv.URL + "/{owner}/{repo}/releases/download/{tag}/{asset}"}
}

func probeGen(t *testing.T, tgz []byte, out string) ([]genResult, error) {
	t.Helper()
	api := fakeAPI(t, map[string]fakeRepo{"Derrity/tinyserve": tinyserve})
	cfg := genConfig{
		repos: []string{"Derrity/tinyserve"}, out: out, probe: true,
		prov: newGitHub(api.URL, ""), host: releaseServer(t, tgz),
	}
	idx := &index.Index{Version: index.Version}
	r := genOne(context.Background(), cfg, idx, "Derrity/tinyserve")
	return []genResult{r}, r.err
}

// --probe 的全部意义：把 [bin] 右边从「猜的」变成「解开真包看出来的」。
func TestProbeFindsTheRealBinPath(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"bin/tinyserve", 0o755},
		{"README.md", 0o644},
		{"completions/tinyserve.bash", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	p := rs[0].pack
	if !p.binProbed {
		t.Fatal("报告里说 [bin] 是猜的，但明明探过了")
	}
	if p.binPath != "bin/tinyserve" {
		t.Fatalf("[bin] = %q，包里那个可执行文件在 bin/tinyserve", p.binPath)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `tinyserve = "bin/tinyserve"`) {
		t.Fatalf("探到的路径没写进 toyfile：\n%s", rs[0].entry.Toyfile)
	}
}

// cargo-dist 的形状：整棵树套在一层「版本号 + target triple」的目录里。
// archive 现在会把那一层剥掉（唯一顶层条目且是目录），所以探到的路径是稳定的
// `tinyserve` —— 这条以前是【整条判失败】，而 ripgrep / fd / bat / delta / eza
// 全是这个形状，也就是说现代 Rust CLI 那一整块以前一个都收不了。
//
// 探到的路径必须和 install 那边实际会得到的布局一致：两边都走 archive.Extract，
// 所以这里探到什么，装的人的 pkg 目录里就是什么。
func TestProbeSeesThroughACargoDistTopLevelDir(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"tinyserve_1.2.3_linux_amd64/tinyserve", 0o755},
		{"tinyserve_1.2.3_linux_amd64/README.md", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("探测失败（那层目录该被剥掉）：%v", err)
	}
	if p := rs[0].pack; p.binPath != "tinyserve" || !p.binProbed {
		t.Fatalf("[bin] = %q（probed=%v），想要 tinyserve", p.binPath, p.binProbed)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `tinyserve = "tinyserve"`) {
		t.Fatalf("探到的路径没写进 toyfile：\n%s", rs[0].entry.Toyfile)
	}
}

// 剥不掉的时候（顶层不止一个条目）那条守卫仍然要拦住：[bin] 右边是逐字用的、
// 不展开 {version}，一条钉死在 1.2.3 上的路径今天能装、上游一发新版就装不上。
func TestProbeRefusesAVersionPinnedBinPath(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"tinyserve_1.2.3_linux_amd64/tinyserve", 0o755},
		{"LICENSE", 0o644}, // 顶层第二个条目 → 不剥
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err == nil {
		t.Fatal("应该失败：这个路径钉死在 1.2.3 上")
	}
	msg := rs[0].err.Error()
	// 报错里要把探到的真实路径给出来 —— 人要手写的时候答案就在眼前。
	if !strings.Contains(msg, "tinyserve_1.2.3_linux_amd64/tinyserve") {
		t.Errorf("报错没给出探到的路径：%s", msg)
	}
	if !strings.Contains(msg, "版本号") {
		t.Errorf("报错没说清是为什么：%s", msg)
	}
}

// 目录名里带平台的同理：一份 toy.toml 只有一行 [bin]，所有平台共用。
func TestUnstableBinPathCatchesPlatformDirs(t *testing.T) {
	for _, tc := range []struct{ path, version, want string }{
		{"tinyserve", "1.2.3", ""},
		{"bin/tinyserve", "1.2.3", ""},
		{"tinyserve_1.2.3_linux_amd64/tinyserve", "1.2.3", "版本号"},
		{"tinyserve-linux-amd64/tinyserve", "", "平台名"},
		// 版本号认不出来时（version 为空）不能拿空串去 Contains —— 那会命中一切。
		{"bin/tinyserve", "", ""},
	} {
		got := unstableBinPath(tc.path, tc.version)
		if (tc.want == "") != (got == "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Errorf("unstableBinPath(%q, %q) = %q，想要含 %q", tc.path, tc.version, got, tc.want)
		}
	}
}

// 定不下来就说出来，绝不瞎挑一个：挑错的症状是用户装完之后 PATH 上那个命令
// 是个补全脚本，而报告里写着「已探测」。
func TestPickExecutableRefusesToGuess(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, mode os.FileMode) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
	write("tinyserve", 0o755)
	write("helper", 0o755)
	write("README.md", 0o644)
	files := []string{"tinyserve", "helper", "README.md"}

	// 有一个正好叫这个命令名 → 用它。
	if hit, cands := pickExecutable(dir, files, "tinyserve"); hit != "tinyserve" || cands != nil {
		t.Errorf("同名的那个应该赢：hit=%q cands=%v", hit, cands)
	}
	// 一个都不同名 → 交出候选，不挑。
	hit, cands := pickExecutable(dir, files, "somethingelse")
	if hit != "" {
		t.Errorf("定不下来却挑了 %q", hit)
	}
	if len(cands) != 2 {
		t.Errorf("候选应该是那两个可执行文件，得到 %v", cands)
	}
	// 只有一个可执行文件 → 就是它，名字对不上也认（作者给二进制起别的名字很常见）。
	if hit, _ := pickExecutable(dir, []string{"helper", "README.md"}, "tinyserve"); hit != "helper" {
		t.Errorf("只有一个可执行文件时应该直接用它，得到 %q", hit)
	}
	// 一个可执行的都没有。
	if hit, cands := pickExecutable(dir, []string{"README.md"}, "tinyserve"); hit != "" || cands != nil {
		t.Errorf("没有可执行文件时不该给出任何东西：%q %v", hit, cands)
	}
}

// 探不到不算失败：条目照出，只是 [bin] 仍然是猜的 —— 但报告里必须说。
func TestProbeFailureStillProducesAnEntryButSaysSo(t *testing.T) {
	// 服务器只认 .tar.gz，这里给一个空包让解压失败。
	rs, err := probeGen(t, []byte("not a tarball"), filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("探测失败不该让整条失败：%v", err)
	}
	p := rs[0].pack
	if p.binProbed {
		t.Fatal("探测没成却说成探到了")
	}
	if !strings.Contains(strings.Join(p.notes, "\n"), "探测没成") {
		t.Fatalf("报告里没说探测失败：%v", p.notes)
	}
}

func TestProbePickPrefersLinuxAmd64(t *testing.T) {
	art := map[string]string{"linux/arm64": "a", "linux/amd64": "b", "darwin/arm64": "c"}
	if plat, tmpl := probePick(art); plat != "linux/amd64" || tmpl != "b" {
		t.Errorf("挑了 %q/%q", plat, tmpl)
	}
	if plat, _ := probePick(map[string]string{"darwin/arm64": "c"}); plat != "" {
		t.Errorf("没有 linux 构建时应该放弃探测，挑了 %q", plat)
	}
}

// 探到的那个文件的【基名】才是作者真正发出来的命令名。
//
// 默认值来自资产名前缀，而那两者经常不是一回事：BurntSushi/ripgrep 的资产叫
// ripgrep-15.2.0-….tar.gz，包里那个文件叫 rg。照默认值写出来的是
// `[bin] ripgrep = "rg"` —— 装完能跑，只是 PATH 上那个名字全世界的文档里都没有
// （没有人写过 `ripgrep --version`）。这个坏法状态码看不见、--probe 的报告里
// 也说「已探测」，只有把两边都读一遍才发现。
func TestProbedBasenameWinsOverTheAssetNamePrefix(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{
		{"ts", 0o755}, // 包里的真名和资产名前缀 tinyserve 不一样
		{"README.md", 0o644},
	})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	p := rs[0].pack
	if p.binName != "ts" {
		t.Fatalf("[bin] 左边 = %q，想要 ts（包里那个文件的基名）", p.binName)
	}
	// 包名/目录名仍然跟着仓库和资产名走 —— 改的只是往 PATH 上放的那个名字。
	if p.name != "tinyserve" {
		t.Errorf("name = %q，不该跟着变（那是包目录名）", p.name)
	}
	if !strings.Contains(rs[0].entry.Toyfile, `ts = "ts"`) {
		t.Fatalf("toyfile 里的 [bin] 不对：\n%s", rs[0].entry.Toyfile)
	}
	// 改了名字必须在报告里说一声：§14.9 第 2 条留给人的那一眼看的就是这一列。
	var said bool
	for _, n := range p.notes {
		if strings.Contains(n, "ts") && strings.Contains(n, "tinyserve") {
			said = true
		}
	}
	if !said {
		t.Errorf("换了命令名却没在 notes 里说：%v", p.notes)
	}
}

// 反过来：基名和默认值一样时不要产出噪音。一条每次都响的说明等于没有说明。
func TestProbedBasenameSaysNothingWhenItMatches(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{{"bin/tinyserve", 0o755}})
	rs, err := probeGen(t, tgz, filepath.Join(t.TempDir(), "idx.json"))
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	p := rs[0].pack
	if p.binName != "tinyserve" {
		t.Fatalf("binName = %q，想要 tinyserve", p.binName)
	}
	for _, n := range p.notes {
		if strings.Contains(n, "命令名按探到的文件取成") {
			t.Errorf("名字没变却报了一句：%s", n)
		}
	}
}
