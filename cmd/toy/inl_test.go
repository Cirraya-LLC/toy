package main

// 索引内联 toyfile（index.Entry.Toyfile）在命令层的六条边界，一条一个测试。
// 全部靠 audFakeHost 那个假托管方跑真链路：ls-remote 拿 tag → 取 toy.toml →
// 下载资产 → 软链，只是 raw 上那份 toy.toml 有时候故意不存在。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

const inlBody = "#!/bin/sh\necho FROM-RELEASE\n"

type inlEnv struct {
	fh   *audFakeHost
	home string
	idx  string // 索引文件的路径，条目由 write 随时改写
}

func inlSetup(t *testing.T) *inlEnv {
	t.Helper()
	fh := audNewFakeHost(t)
	_, home, _ := audIndexSetup(t, fh)
	return &inlEnv{fh: fh, home: home, idx: sgnSignedIndexPath(t, home)}
}

func (e *inlEnv) write(t *testing.T, entry index.Entry) {
	t.Helper()
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.idx, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *inlEnv) rawToyfile(owner, repo, tag string) string {
	return filepath.Join(e.fh.root, owner, repo, "raw", tag, "toy.toml")
}

// publishBare 造一个【没有 toy.toml】的上游 —— 也就是现存那几千个 CLI 工具的样子。
func (e *inlEnv) publishBare(t *testing.T, owner, repo, tag string) {
	t.Helper()
	e.fh.publish(t, owner, repo, tag, inlBody)
	if err := os.Remove(e.rawToyfile(owner, repo, tag)); err != nil {
		t.Fatal(err)
	}
}

// inlToyfile 是索引维护者写的那份打包说明。资产名 audtool 就是 audFakeHost
// 放进 release 里的那个文件。
func inlToyfile(bin string) string {
	return fmt.Sprintf("name = \"audtool\"\ndesc = \"索引写的\"\n[bin]\n%s = \"audtool\"\n[artifact]\n%q = \"audtool\"\n",
		bin, i1Plat())
}

func inlManifest(t *testing.T, home, id string) store.Manifest {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.LoadManifest(id)
	if err != nil {
		t.Fatalf("读不到 %s 的安装清单：%v", id, err)
	}
	return m
}

// 规则 3：上游没有 toy.toml 时，索引内联那份顶上，装得成，
// 而且信任等级必须落在 store.TrustIndexed 上并如实说出来（铁律 8）。
func TestInlineToyfileInstallsWhenUpstreamHasNone(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})

	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
			t.Fatalf("上游没有 toy.toml，索引给了一份，却装不上：%v", err)
		}
	})

	link := filepath.Join(e.home, "bin", "audtool")
	if b, err := os.ReadFile(link); err != nil || string(b) != inlBody {
		t.Fatalf("装到的不是 release 里那个字节：%q（err=%v）", b, err)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustIndexed {
		t.Fatalf("清单里的信任等级 = %q，想要 %q", got, store.TrustIndexed)
	}
	// 铁律 8：这一行必须说清楚哪一半不是作者的，而且不许长得像已签名。
	for _, want := range []string{"索引", "作者"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出里没说清楚打包说明的来历（缺 %q）：\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ 已签名") {
		t.Fatalf("内联那一档打出了「✓ 已签名」：\n%s", out)
	}
}

// 规则 1：上游有自己的 toy.toml 时，内联那份看都不看 —— 否则一条恶意条目
// 就能把作者声明的命令名换掉，那是劫持。
func TestUpstreamToyfileBeatsTheInlineOne(t *testing.T) {
	e := inlSetup(t)
	e.fh.publish(t, "alice", "tool", "v1.0.0", inlBody) // 这一版 raw 上有 toy.toml，声明的命令叫 audtool
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("hijacked")})

	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("装不上：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "hijacked")); err == nil {
		t.Fatal("索引内联那份盖过了作者自己的 toy.toml —— PATH 上多出了一条作者没声明过的命令")
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "audtool")); err != nil {
		t.Fatalf("作者声明的命令没装上：%v", err)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustHost {
		t.Fatalf("用的是作者自己那份，信任等级却是 %q", got)
	}
}

// 规则 1 的另一半，也是这条设计的安全边界：只有【明确不存在】才让位。
// 超时 / 5xx / 读不动一律照旧报错 —— 一次网络抖动不该悄悄把用户从
// 「作者的声明」切到「索引的声明」，而且那件事没有任何地方会说。
func TestTransientFailureNeverFallsBackToInline(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	// 把 toy.toml 换成一个目录：打开得开、读不动（EISDIR）—— 这不是「不存在」。
	if err := os.Mkdir(e.rawToyfile("alice", "tool", "v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})

	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("取 toy.toml 只是没成功（不是 404），却回落到了索引内联的那份")
	}
	if _, err := os.Lstat(filepath.Join(e.home, "bin", "audtool")); err == nil {
		t.Fatal("这次安装应该整个失败，却已经把命令链上去了")
	}
}

// rsvIsNotFound 是上面那条边界的全部判据，单独钉一遍。
//
// 判据是【类型】不是文本：错误里那句 "HTTP 500 …" 的原因短语是服务器原样发来的，
// 按子串认 "HTTP 404" 的话，一台上游回一句 `HTTP/1.1 500 HTTP 404 Not Found`
// 就能把一次 500 认成"不存在"。fetch 那边有一条同源的回归测试。
func TestRsvIsNotFound(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"file:// 打不开", fmt.Errorf("取不到：%w", fs.ErrNotExist), true},
		{"fetch 包过的 404", fmt.Errorf("取不到：%w：%w", fs.ErrNotExist, errors.New("HTTP 404 Not Found")), true},
		{"HTTP 500 不算", errors.New("https://x/toy.toml：HTTP 500 Internal Server Error"), false},
		{"原因短语里写着 HTTP 404 的 500 也不算",
			errors.New("https://x/toy.toml：HTTP 500 HTTP 404 Not Found"), false},
		{"HTTP 403 不算", errors.New("https://x/toy.toml：HTTP 403 Forbidden"), false},
		{"超时不算", errors.New("context deadline exceeded"), false},
		{"DNS 失败不算", errors.New("dial tcp: lookup x: no such host"), false},
		{"读不动不算", errors.New("read /x/toy.toml: is a directory"), false},
	} {
		if got := rsvIsNotFound(c.err); got != c.want {
			t.Errorf("%s：rsvIsNotFound = %v，想要 %v", c.name, got, c.want)
		}
	}
}

// 规则 2：内联那份有签名也到不了「✓ 已签名」。签名覆盖的是【提交这条索引记录
// 的人】写的内容，签名者不是作者 —— 那个勾会说过头（§14.3）。
// 指纹仍然要留下，否则换一把钥匙 TOFU 就拦不住了（规格 §4）。
func TestInlineToyfileNeverReachesSignedTrust(t *testing.T) {
	kp := sgnKey(t)
	const id, source = "alice/tool", "fake:alice/tool"
	raw := []byte(inlToyfile("audtool"))
	tf, err := toyfile.ParseFrom(raw, toyfile.OriginIndex)
	if err != nil {
		t.Fatal(err)
	}
	res := &resolved{Toyfile: tf, Raw: raw, Trust: store.TrustIndexed}
	e := index.Entry{ID: id, Source: source, Toyfile: string(raw),
		Key: hex.EncodeToString(kp.Public), Sig: sign.Sign(kp.Private, autSignMessage(id, source, raw))}

	rsvApplySignature("local", e, res)
	if res.Trust != store.TrustIndexed {
		t.Fatalf("内联那份被签名抬到了 %q —— 签名者不是作者，这个勾说过头了", res.Trust)
	}
	if res.Signer == "" {
		t.Fatal("指纹丢了：换一把钥匙就没人拦得住了")
	}

	// 对照：同一条签名放在上游那份 toyfile 上，仍然要打勾 —— 别把这条路一起改坏。
	up, err := toyfile.ParseFrom(raw, toyfile.OriginUpstream)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &resolved{Toyfile: up, Raw: raw, Trust: store.TrustHost}
	rsvApplySignature("local", e, upstream)
	if upstream.Trust != store.TrustSigned {
		t.Fatalf("作者自己那份验过签名却没打勾：%q", upstream.Trust)
	}
}

// 规则 6 的降档方向：上游那份 toy.toml 没了、改由索引内联的顶上，
// 那是别人替作者做了主 —— 停下来要 --force，放行时必须说放行了什么。
func TestUpstreamDisappearingIsADowngrade(t *testing.T) {
	e := inlSetup(t)
	e.fh.publish(t, "alice", "tool", "v1.0.0", inlBody)
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("第一次装不上：%v", err)
	}

	// 新版本上游没有 toy.toml 了，只剩索引那份。
	e.publishBare(t, "alice", "tool", "v2.0.0")
	if err := runInstall(context.Background(), []string{"alice/tool"}); err == nil {
		t.Fatal("信任从「托管方账号」掉到「打包说明来自索引」，却一声不吭地装上了")
	}
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--force", "alice/tool"}); err != nil {
			t.Fatalf("确认之后还是装不上：%v", err)
		}
	})
	if !strings.Contains(out, "--force 放行") || !strings.Contains(out, "索引") {
		t.Fatalf("--force 放行了一次降档，输出里却看不出降到了哪一档：\n%s", out)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustIndexed {
		t.Fatalf("装完的信任等级 = %q", got)
	}
}

// 规则 6 的升级方向：作者后来自己加了 toy.toml —— 那是好事，正常装，
// 但要说一句，因为「装哪个资产、命令叫什么名字」这次换了个人说了算。
func TestUpstreamTakingOverIsAnnounced(t *testing.T) {
	e := inlSetup(t)
	e.publishBare(t, "alice", "tool", "v1.0.0")
	e.write(t, index.Entry{ID: "alice/tool", Source: "fake:alice/tool", Desc: "d", Toyfile: inlToyfile("audtool")})
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("第一次装不上：%v", err)
	}

	e.fh.publish(t, "alice", "tool", "v2.0.0", inlBody) // 这一版作者自己写了 toy.toml
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
			t.Fatalf("作者接入之后反而装不上了：%v", err)
		}
	})
	if !strings.Contains(out, "不再由索引提供") {
		t.Fatalf("打包说明的提供方换了人，输出里却没说：\n%s", out)
	}
	if got := inlManifest(t, e.home, "fake:alice/tool").Trust; got != store.TrustHost {
		t.Fatalf("换成作者那份之后信任等级 = %q，想要 %q", got, store.TrustHost)
	}
}

// `toy publish --inline` 把 toy.toml 原文放进条目，而且那条条目要能被索引读回来。
func TestPublishInlineEmbedsTheToyfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOY_HOME", t.TempDir())
	body := inlToyfile("audtool")
	if err := os.WriteFile(filepath.Join(dir, "toy.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := inlPublish(t, "--inline", "--source", "github:alice/tool", dir)
	if entry.Toyfile != body {
		t.Fatalf("条目里的 toyfile 和本地那份对不上：\n%q\n%q", entry.Toyfile, body)
	}
	idx, skipped, err := index.Load([]byte(`{"version":1,"packages":[`+inlJSON(t, entry)+`]}`), "test")
	if err != nil || skipped != 0 || len(idx.Packages) != 1 {
		t.Fatalf("内联条目粘进索引读不回来：err=%v skipped=%d", err, skipped)
	}

	// 不带 --inline 就还是原来那种只存指针的条目（规格 §5）。
	if plain := inlPublish(t, "--source", "github:alice/tool", dir); plain.Toyfile != "" {
		t.Fatalf("没给 --inline 却内联了：%q", plain.Toyfile)
	}
}

func inlJSON(t *testing.T, e index.Entry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// inlPublish 跑一次 runPublish 并把它写到 stdout 的那条条目读回来
// （条目走 stdout、说明走 stderr，规格 §9）。
func inlPublish(t *testing.T, args ...string) index.Entry {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := runPublish(context.Background(), args)
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("publish 失败：%v\n%s", runErr, out)
	}
	var e index.Entry
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("publish 打出来的不是一条能用的条目：%v\n%s", err, out)
	}
	return e
}
