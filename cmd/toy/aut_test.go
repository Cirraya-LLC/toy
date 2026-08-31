package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
)

func TestAutParseRemote(t *testing.T) {
	cases := []struct {
		remote              string
		domain, owner, repo string
		wantErr             bool
	}{
		{remote: "https://github.com/Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "https://github.com/Derrity/tinyserve", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "https://user@gitlab.com/o/r.git", domain: "gitlab.com", owner: "o", repo: "r"},
		{remote: "ssh://git@github.com:22/Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "git@github.com:Derrity/tinyserve.git", domain: "github.com", owner: "Derrity", repo: "tinyserve"},
		{remote: "git@Codeberg.ORG:o/r", domain: "codeberg.org", owner: "o", repo: "r"},
		{remote: "  https://github.com/o/r.git\n", domain: "github.com", owner: "o", repo: "r"},
		// 本地路径的 origin 没有域名，推不出托管方 —— 必须报错而不是猜一个。
		{remote: "/srv/git/tinyserve.git", wantErr: true},
		// GitLab 子组是三层，而 ref 只有 owner/repo 一层。
		{remote: "https://gitlab.com/group/sub/repo.git", wantErr: true},
		{remote: "https://github.com/onlyowner", wantErr: true},
		{remote: "", wantErr: true},
	}
	for _, c := range cases {
		domain, owner, repo, err := autParseRemote(c.remote)
		if c.wantErr {
			if err == nil {
				t.Errorf("autParseRemote(%q) 该报错，却给出 %s/%s/%s", c.remote, domain, owner, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("autParseRemote(%q) 报错了：%v", c.remote, err)
			continue
		}
		if domain != c.domain || owner != c.owner || repo != c.repo {
			t.Errorf("autParseRemote(%q) = %s %s %s，想要 %s %s %s",
				c.remote, domain, owner, repo, c.domain, c.owner, c.repo)
		}
	}
}

// TestAutSignMessageIsFrozen 把签名覆盖的字节钉死。改了它，所有已经发出去的
// 签名会一起验不过，而用户看到的报错读起来像作者的私钥泄漏了。
func TestAutSignMessageIsFrozen(t *testing.T) {
	got := string(autSignMessage("Derrity/tinyserve", "github:Derrity/tinyserve", []byte("name = \"tinyserve\"\n")))
	want := "Derrity/tinyserve\ngithub:Derrity/tinyserve\nname = \"tinyserve\"\n"
	if got != want {
		t.Fatalf("签名消息变了：\n拿到 %q\n想要 %q", got, want)
	}
}

func TestAutHostName(t *testing.T) {
	if got := autHostName("github.com", nil); got != "github" {
		t.Errorf("github.com 应该反查成 github，拿到 %q", got)
	}
	if got := autHostName("CODEBERG.org", nil); got != "codeberg" {
		t.Errorf("域名比较该忽略大小写，拿到 %q", got)
	}
	// 用户在 config.toml 里配的自建实例，不用改代码就该认得。
	mine := map[string]host.Host{"mygit": {Clone: "https://git.example.com/{owner}/{repo}"}}
	if got := autHostName("git.example.com", mine); got != "mygit" {
		t.Errorf("自建托管方该被认出来，拿到 %q", got)
	}
	if got := autHostName("bitbucket.org", nil); got != "" {
		t.Errorf("不认识的域名该返回空串让调用方去要 --source，拿到 %q", got)
	}
}

func TestAutMirrorHandler(t *testing.T) {
	dir := t.TempDir()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, sum), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目录列表和任意路径都不该被暴露出去，所以特意放一个非 hash 名字的文件。
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(autMirrorHandler(dir))
	defer srv.Close()

	body, code := autGet(t, srv.Client(), srv.URL+"/"+sum)
	if code != http.StatusOK || body != "payload" {
		t.Errorf("按 sha256 取文件 = %d %q，想要 200 \"payload\"", code, body)
	}
	for _, path := range []string{"/", "/secret.txt", "/" + strings.ToUpper(sum), "/../../etc/passwd", "/" + sum + "x"} {
		if _, code := autGet(t, srv.Client(), srv.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d，想要 404（只认 64 位小写 hex）", path, code)
		}
	}
	resp, err := srv.Client().Post(srv.URL+"/"+sum, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d，想要 405", resp.StatusCode)
	}
}

func autGet(t *testing.T, c *http.Client, url string) (string, int) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s：%v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), resp.StatusCode
}

func TestAutWildcard(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		if !autWildcard(addr) {
			t.Errorf("%q 是对全网监听，该提醒用户", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if autWildcard(addr) {
			t.Errorf("%q 只听本机，不该报警", addr)
		}
	}
}

// TestAutSignedEntryVerifies 走一遍真链路：签出来的东西必须能用条目里那把公钥验过，
// 而且改一个字节的 toy.toml 就该验不过 —— 否则 publish 发出去的签名什么都没保护。
func TestAutSignedEntryVerifies(t *testing.T) {
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const id, source = "Derrity/tinyserve", "github:Derrity/tinyserve"
	raw := []byte("name = \"tinyserve\"\n")
	sig := sign.Sign(kp.Private, autSignMessage(id, source, raw))
	pub := hex.EncodeToString(kp.Public)

	if err := sign.Verify(pub, autSignMessage(id, source, raw), sig); err != nil {
		t.Fatalf("自己签的自己验不过：%v", err)
	}
	tampered := []byte("name = \"tinyserve\"\n# 偷偷加一行\n")
	if err := sign.Verify(pub, autSignMessage(id, source, tampered), sig); err == nil {
		t.Fatal("改过的 toy.toml 竟然验过了")
	}
	if err := sign.Verify(pub, autSignMessage("someone/else", source, raw), sig); err == nil {
		t.Fatal("换了 id 竟然验过了 —— 签名必须把名字绑住")
	}
}

// TestAutPublishedEntryLoadsIntoAnIndex 钉住那句「粘进索引不会让它坏掉」：
// publish 打出来的条目要能被 index.Load 原样读回来，sig 也不能在路上丢掉
// —— 客户端就是靠它决定敢不敢打「✓ 已签名」。
func TestAutPublishedEntryLoadsIntoAnIndex(t *testing.T) {
	entry := index.Entry{
		ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Desc: "小", Sig: "AAAA",
	}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	idx, skipped, err := index.Load([]byte(`{"version":1,"packages":[`+string(body)+`]}`), "test")
	if err != nil || skipped != 0 || len(idx.Packages) != 1 {
		t.Fatalf("粘进索引读不回来：err=%v skipped=%d packages=%d", err, skipped, len(idx.Packages))
	}
	got, ok := idx.Find("Derrity/tinyserve")
	if !ok {
		t.Fatal("条目进了索引却搜不到")
	}
	if got.Sig != "AAAA" {
		t.Fatalf("sig 在路上丢了：%q", got.Sig)
	}
}

// autGitPath 必须跳过 toy 自己的 bin 目录。
//
// 裸 "git" 交给 exec 就是执行 PATH 上的第一个 git，而按 README / `toy doctor`
// 的说法 ~/.toy/bin 就在 PATH 上 —— 一个装上来的包只要提供一条叫 git 的命令，
// `toy publish` 就会执行它。铁律 1 的洞，不是不方便。
func TestGitPathSkipsToyOwnBinDir(t *testing.T) {
	planted, real := t.TempDir(), t.TempDir()
	put := func(dir string) string {
		p := filepath.Join(dir, "git")
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fake, want := put(planted), put(real)

	// toy 自己的 bin 排在最前面 —— 用户按文档就是这么加进 PATH 的。
	t.Setenv("PATH", planted+string(os.PathListSeparator)+real)
	got, err := autGitPath(planted)
	if err != nil {
		t.Fatalf("PATH 上明明有一个真的 git，却报错了：%v", err)
	}
	if got == fake {
		t.Fatalf("挑中了 toy 自己 bin 目录里的 %s —— publish 会执行它（铁律 1）", fake)
	}
	if got != want {
		t.Fatalf("挑中了 %s，想要 %s", got, want)
	}

	// 只剩那一个的时候必须拒绝，而且要指名道姓，用户才知道去卸谁。
	t.Setenv("PATH", planted)
	switch _, err := autGitPath(planted); {
	case err == nil:
		t.Fatal("PATH 上唯一的 git 来自一个装上来的包，autGitPath 还是把它交出去了")
	case !strings.Contains(err.Error(), fake):
		t.Fatalf("拒是拒了，但没说清楚是哪个文件，用户无从下手：%v", err)
	}
}

// PATH 里的相对项（"." 和那个到处都是的空项）指的是进程**当前所在的目录**。
// 在一个别人写得进去的目录里跑 toy，不该等于把铁律 1 交出去。
func TestGitPathIgnoresRelativePathEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", "."+string(os.PathListSeparator))
	if got, err := autGitPath(""); err == nil {
		t.Fatalf("PATH 上只有相对项，autGitPath 却把当前目录里的 %s 交出去了", got)
	}
}

// 走完整条路：一个装上来的包占住了 git 这个名字，`toy publish` 不该执行它。
func TestPublishDoesNotRunGitFromToyBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	if err := os.WriteFile(filepath.Join(bin, "git"),
		[]byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 用户按 README / `toy doctor` 说的把 ~/.toy/bin 加进 PATH，
	// 而这台机器上还没装真的 git。
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "toy.toml"),
		[]byte("name = \"t\"\n[script]\nt = \"t.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runPublish(t.Context(), []string{dir})
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatalf("`toy publish` 执行了 ~/.toy/bin 里那个 git（%s 出现了）—— 铁律 1 破了", marker)
	}
	if err == nil {
		t.Fatal("PATH 上没有能用的 git，source 推不出来，publish 不该成功")
	}
	// doctor 报的必须是同一个判断，不然它会指着那个包说「git 在：…」。
	if c := qryCheckGit(bin); c.ok {
		t.Fatalf("`toy doctor` 把一个装上来的包当成了健康的 git：%q", c.title)
	}
}
