package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// audFakeHost 在磁盘上搭一个「托管方」：一个裸 git 仓库负责 tag 发现
// （host.Tags 跑的就是 git ls-remote，本地路径一样能跑），raw/release 用 file://。
type audFakeHost struct{ root string }

func audNewFakeHost(t *testing.T) *audFakeHost {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	return &audFakeHost{root: t.TempDir()}
}

func (h *audFakeHost) tmpl() host.Host {
	return host.Host{
		Raw:     "file://" + h.root + "/{owner}/{repo}/raw/{ref}/{path}",
		Clone:   h.root + "/{owner}/{repo}.git",
		Release: "file://" + h.root + "/{owner}/{repo}/rel/{tag}/{asset}",
		Web:     "https://fake.example/{owner}/{repo}",
	}
}

// publish 造一个 owner/repo 的 tag、toy.toml 和 release 资产（单文件脚本形态）。
func (h *audFakeHost) publish(t *testing.T, owner, repo, tag, body string) {
	t.Helper()
	bare := filepath.Join(h.root, owner, repo+".git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
			"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := filepath.Join(h.root, owner, repo+".work")
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		run(bare, "init", "-q", "--bare", bare)
		os.MkdirAll(work, 0o755)
		os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
		run(work, "init", "-q", work)
		run(work, "add", "f")
		run(work, "commit", "-q", "-m", "c")
		run(work, "push", "-q", bare, "HEAD:refs/heads/main")
	}
	// 打 tag 这几行原来整个包在「仓库还不存在」里面，所以对同一个仓库 publish
	// 第二个版本时只写了 raw/rel、一个 tag 都没推 —— ls-remote 永远只看得到第一版，
	// 而测试里看起来只是「装错了版本」。-f 让它幂等。
	run(work, "tag", "-f", tag)
	run(work, "push", "-q", "-f", bare, "refs/tags/"+tag)
	// raw 上的 toy.toml（按 {ref} 也就是 tag 存）
	rawDir := filepath.Join(h.root, owner, repo, "raw", tag)
	os.MkdirAll(rawDir, 0o755)
	os.WriteFile(filepath.Join(rawDir, "toy.toml"), []byte(`
name    = "audtool"
desc    = "d"
[bin]
audtool = "audtool"
[artifact]
"`+i1Plat()+`" = "audtool"
`), 0o644)
	// release 资产：不是压缩包，走「裸二进制」那条路
	relDir := filepath.Join(h.root, owner, repo, "rel", tag)
	os.MkdirAll(relDir, 0o755)
	os.WriteFile(filepath.Join(relDir, "audtool"), []byte(body), 0o644)
}

// 索引在 `toy update` 时能把一个已装的包换成【另一个仓库】的内容吗？
//
// insUpdateOne 在 m.IndexID != "" 时按索引条目重新解析。对**没有签名**的条目，
// 索引改一行 source 就换掉了用户 PATH 上那个命令的内容，而且一道闸都没有：
//   - insTrustDowngraded 只拦 signed → 非 signed，两边都不签名时它什么都不做
//   - TOFU 的 id 是 insTrustID(res) = "<host>:<owner>/<repo>@<tag>"，source 一换
//     就是一条全新记录 → TrustNew → 不警告、直接钉住
func TestAuditIndexCanRepointAnInstalledPackage(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	fh.publish(t, "evil", "tool", "v2.0.0", "#!/bin/sh\necho EVIL\n")

	_, home, writeIdx := audIndexSetup(t, fh)

	writeIdx("fake:alice/tool")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	link := filepath.Join(home, "bin", "audtool")
	if b, _ := os.ReadFile(link); string(b) != "#!/bin/sh\necho GOOD\n" {
		t.Fatalf("装到的不是 alice 的东西：%q", b)
	}

	// 索引里那一行 source 被改了。
	writeIdx("fake:evil/tool")

	err := runUpdate(context.Background(), nil)
	got, _ := os.ReadFile(link)
	if string(got) == "#!/bin/sh\necho EVIL\n" {
		t.Fatalf("`toy update` 把 PATH 上的 audtool 换成了 evil/tool 的内容 —— "+
			"没有确认、没有 TOFU 警告、信任等级也没变（update err=%v）", err)
	}
	t.Logf("没被换掉（err=%v，内容=%q）", err, got)
}

// audIndexSetup 搭一套「一个本地索引 + 一个本地托管方」的环境：TOY_HOME 指向
// 一个临时目录，索引里只有 alice/tool 一条，source 由返回的钩子随时改写。
func audIndexSetup(t *testing.T, fh *audFakeHost) (*store.Store, string, func(source string)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(t.TempDir(), "index.json")
	cfg := store.DefaultConfig()
	cfg.Indexes = []store.IndexRef{{Name: "local", URL: "file://" + idxPath}}
	cfg.Hosts = map[string]host.Host{"fake": fh.tmpl()}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return st, home, func(source string) {
		b, _ := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
			{ID: "alice/tool", Source: source, Desc: "d"},
		}})
		if err := os.WriteFile(idxPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
