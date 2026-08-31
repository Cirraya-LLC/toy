// Package host 把「git 托管方」变成一张配置表而不是三份 API 适配器。
// 规格 §2：绝不用 GitHub/GitLab 的 REST API —— 限流 60 次/小时，而且要写三套。
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var ErrUnknownHost = errors.New("host: 不认识这个托管方")

// Host 是四个 URL 模板。占位符：{owner} {repo} {ref} {path} {tag} {asset}
type Host struct {
	Name    string `toml:"-"`
	Raw     string `toml:"raw"`     // 取仓库里某个文件
	Clone   string `toml:"clone"`   // git ls-remote 用
	Release string `toml:"release"` // release 资产下载地址
	Web     string `toml:"web"`     // 给人看的仓库地址
}

// placeholderOrigin 是内置表里「这是形状不是托管方」那几条用的域名。
// RFC 2606 保留给文档用，永远不会有真机器在后面 —— 所以它同时是标记和保险：
// Lookup 认出它就拒绝，用户不会拿着一个 DNS 错误去猜自己哪写错了。
const placeholderOrigin = "https://gitea.example.com"

// giteaPaths 是 gitea / forgejo 系的路径形状，codeberg 就是它的一个实例。
// 换个域名就是另一家 —— 这就是「host 是配置不是代码」的意思。
func giteaPaths(origin string) Host {
	return Host{
		Raw:     origin + "/{owner}/{repo}/raw/tag/{ref}/{path}",
		Clone:   origin + "/{owner}/{repo}",
		Release: origin + "/{owner}/{repo}/releases/download/{tag}/{asset}",
		Web:     origin + "/{owner}/{repo}",
	}
}

// isPlaceholder 判断一条内置项是不是「形状」而不是真托管方。
func (h Host) isPlaceholder() bool {
	return strings.HasPrefix(h.Clone, placeholderOrigin)
}

// isEmpty 是「这一段 [hosts.x] 一个模板都没配」——通常是 config.toml 里把
// 键名写错了（clone_url 而不是 clone）。
func (h Host) isEmpty() bool {
	return h.Raw == "" && h.Clone == "" && h.Release == "" && h.Web == ""
}

// Builtin 返回内置的托管方表：github / gitlab / codeberg / gitea 通用形态。
// 用户在 config.toml 里加三行就能多一个托管方 —— host 是配置不是代码。
//
// 注意 "gitea" 这一条是**形状**不是托管方（域名是 RFC 2606 的保留域名）：
// Lookup 会直接拒绝它并把下面这段抄给用户，因为自建实例的域名只有用户自己知道。
//
//	[hosts.mygit]
//	raw     = "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}"
//	clone   = "https://git.example.com/{owner}/{repo}"
//	release = "https://git.example.com/{owner}/{repo}/releases/download/{tag}/{asset}"
//	web     = "https://git.example.com/{owner}/{repo}"
func Builtin() map[string]Host {
	m := map[string]Host{
		"github": {
			Raw:     "https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}",
			Clone:   "https://github.com/{owner}/{repo}",
			Release: "https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}",
			Web:     "https://github.com/{owner}/{repo}",
		},
		"gitlab": {
			Raw:     "https://gitlab.com/{owner}/{repo}/-/raw/{ref}/{path}",
			Clone:   "https://gitlab.com/{owner}/{repo}",
			Release: "https://gitlab.com/{owner}/{repo}/-/releases/{tag}/downloads/{asset}",
			Web:     "https://gitlab.com/{owner}/{repo}",
		},
		"codeberg": giteaPaths("https://codeberg.org"),
		"gitea":    giteaPaths(placeholderOrigin),
	}
	for name, h := range m {
		h.Name = name
		m[name] = h
	}
	return m
}

// selfHostedHint 是拒绝一个 host 时给出的那段可以直接抄进 config.toml 的模板。
func selfHostedHint(name string) string {
	return fmt.Sprintf("在 ~/.toy/config.toml 里加一段：\n\n"+
		"\t[hosts.%s]\n"+
		"\traw     = \"https://你的域名/{owner}/{repo}/raw/tag/{ref}/{path}\"\n"+
		"\tclone   = \"https://你的域名/{owner}/{repo}\"\n"+
		"\trelease = \"https://你的域名/{owner}/{repo}/releases/download/{tag}/{asset}\"\n"+
		"\tweb     = \"https://你的域名/{owner}/{repo}\"", name)
}

// Lookup 先查 overrides 再查内置表。
func Lookup(name string, overrides map[string]Host) (Host, error) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return Host{}, fmt.Errorf("%w：托管方名字是空的（应该写成 github:owner/repo）", ErrUnknownHost)
	}
	// overrides 的 key 是用户自己起的名字，先按原样找，再退回小写。
	// Name 一律落成小写：它会变成 ~/.toy/pkg/<host>/… 的目录名（规格 §6），
	// 大小写两种写法必须指向同一个目录，否则同一个包会被装两遍。
	for _, key := range []string{name, lower} {
		if h, ok := overrides[key]; ok {
			if h.isEmpty() {
				return Host{}, fmt.Errorf("%w：config.toml 里的 [hosts.%s] 一个模板都没配"+
					"（四个键是 raw / clone / release / web，写错键名不会报错、只会变成空的），"+
					"而它盖住了同名的内置托管方。%s", ErrUnknownHost, key, selfHostedHint(key))
			}
			h.Name = lower
			return h, nil
		}
	}
	builtin := Builtin()
	if h, ok := builtin[lower]; ok {
		// 形状不是托管方：直接用会打到一个不存在的域名上，报出来是一句 DNS 错误，
		// 用户完全看不出自己该去配什么。在这里拦住并把模板给他。
		if h.isPlaceholder() {
			return Host{}, fmt.Errorf("%w：%q 是 gitea/forgejo 的**形状**，不是一家托管方 —— "+
				"自建实例的域名只有你自己知道。%s", ErrUnknownHost, name, selfHostedHint(lower))
		}
		return h, nil
	}
	// 内置和 overrides 可能撞名（[hosts.github] 覆盖内置），列出来只能有一份。
	seen := make(map[string]bool, len(builtin)+len(overrides))
	known := make([]string, 0, len(builtin)+len(overrides))
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			known = append(known, k)
		}
	}
	for k, h := range builtin {
		if !h.isPlaceholder() { // 拒绝的东西不能出现在「现在认识的是」里
			add(k)
		}
	}
	for k := range overrides {
		add(strings.ToLower(k))
	}
	sort.Strings(known)
	return Host{}, fmt.Errorf(
		"%w：%q。现在认识的是 %s；自建实例%s",
		ErrUnknownHost, name, strings.Join(known, "、"), selfHostedHint(lower))
}

// expand 把模板里的占位符换掉。模板为空返回空串，调用方据此判断这家没配这个能力。
// NewReplacer 只扫一遍，所以替换进去的值里再带 {owner} 也不会被二次展开。
func expand(tmpl string, kv ...string) string {
	if tmpl == "" {
		return ""
	}
	return strings.NewReplacer(kv...).Replace(tmpl)
}

func (h Host) RawURL(owner, repo, ref, path string) string {
	return expand(h.Raw, "{owner}", owner, "{repo}", repo, "{ref}", ref, "{path}", path)
}

func (h Host) CloneURL(owner, repo string) string {
	return expand(h.Clone, "{owner}", owner, "{repo}", repo)
}

// ReleaseURL 拼 release 资产的下载地址。
//
// asset 是四个入参里唯一来自**包作者**的（toy.toml 的 [artifact] 值），而且它在
// 四家托管方上都是一个扁平文件名 —— 所以整段做 path 转义：
//   - "../../../x" 会被转义掉，否则作者能把下载指到同一个域名下别人的 release，
//     而安装完那一行仍然如实地写着「来源 github.com/他自己」（违反铁律 8）
//   - 带空格 / ? / # 的资产名不再拼出一个不合法或被截断的 URL
//
// owner/repo/tag 不转义：它们由 internal/ref 和 internal/semver 限成
// [A-Za-z0-9._-]，而 ref/path 那两个位置上的斜杠是合法的（子目录、带斜杠的分支名）。
func (h Host) ReleaseURL(owner, repo, tag, asset string) string {
	return expand(h.Release, "{owner}", owner, "{repo}", repo, "{tag}", tag,
		"{asset}", url.PathEscape(asset))
}

func (h Host) WebURL(owner, repo string) string {
	return expand(h.Web, "{owner}", owner, "{repo}", repo)
}

// maxStderrLine 是从 git 那儿转述给用户的错误信息长度上限（字节）。
const maxStderrLine = 300

// maxStderrBytes 是 stderr 缓冲区的上限。git 的 `remote: …` 行是**远端服务器**
// 写的，不封顶就等于让对面决定我们吃多少内存 —— 而我们只会用到第一行。
const maxStderrBytes = 8 << 10

// cappedBuffer 只留开头那一小段，多的照收不误但直接丢掉。
// Write 必须谎报「全写完了」：返回短写会让 os/exec 的拷贝协程报错、
// 提前关掉管道，git 那边收到 EPIPE，本来能读到的原因就没了。
type cappedBuffer struct{ b bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if n := maxStderrBytes - c.b.Len(); n > 0 {
		c.b.Write(p[:min(n, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.b.String() }

// Tags 用 `git ls-remote --tags <clone-url>` 拿全部 tag 名。
// 一条命令，所有 git 托管方行为一致，不用 API、不用 token、不限流。
// 必须带 context 超时；剥掉 refs/tags/ 前缀和 ^{} 后缀（annotated tag 的解引用行）。
func Tags(ctx context.Context, cloneURL string) ([]string, error) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return nil, errors.New("host: clone 地址是空的 —— 这家托管方没配 clone 模板，没法发现版本")
	}
	// 铁律 1：toy 任何时候都不执行别人的代码。而 `git ls-remote <url>` 里的 url
	// 只要以 '-' 开头，git 就把它当成选项 —— `--upload-pack=<命令>` 是一条现成的
	// 任意命令执行（实测 git 真的会跑它）。下面既传 `--` 分隔符、这里也直接拒绝：
	// 一个以 '-' 开头的字符串无论如何都不是合法的 clone 地址。
	if strings.HasPrefix(cloneURL, "-") {
		return nil, fmt.Errorf("host: 拒绝执行 —— clone 地址 %q 以 '-' 开头，"+
			"git 会把它当成命令行选项（比如 --upload-pack=<命令>，那就是一条任意命令执行）", cloneURL)
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--refs", "--", cloneURL)
	// GIT_TERMINAL_PROMPT=0：私有仓库或打错的名字会让 git 停在交互式密码提示上永不返回，
	// 那时候 context 超时也只能杀掉 git 本身，用户看到的是一次莫名其妙的挂起。
	// 另外两个 ASKPASS 是防它转头去弹图形界面的密码框。
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	// 不在当前目录下跑 git：git 会读**当前目录所属仓库**的 .git/config，而那里的
	// url.<base>.insteadOf / credential.helper 都能让 git 去执行别的东西。在一个
	// 别人写得进去的目录里跑 toy 不该等于把铁律 1 交出去。用 "/" 而不是 os.TempDir()
	// —— 共享机器上谁都能在 /tmp 里 git init，那样只是换了个人来投毒。
	cmd.Dir = "/"
	// git 会自己再拉起 git-remote-https 之类的帮手进程。context 到期时 Go 只杀 git 本身，
	// 帮手还攥着 stderr 管道的话 Output() 会一直等下去 —— 超时就白设了。
	cmd.WaitDelay = 2 * time.Second
	// stdout 没有封顶：那是 git 自己的 ref 广播，条数由远端的 tag 数决定，
	// 一个 tag 多到能撑爆内存的仓库也没法拿来装东西。stderr 是纯文本、且对面
	// 想写多少写多少，所以那一头必须封。
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("host: 找不到 git 命令。toy 靠 `git ls-remote` 发现版本，请先装 git" +
				"（Debian/Ubuntu: sudo apt install git；macOS: xcode-select --install）")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("host: 读 %s 的 tag 超时了（%v）—— 可能是网络不通，或者这个仓库不存在/需要登录", cloneURL, ctxErr)
		}
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("host: `git ls-remote --tags %s` 失败：%s", cloneURL, msg)
		}
		return nil, fmt.Errorf("host: `git ls-remote --tags %s` 失败：%w", cloneURL, err)
	}
	return parseLsRemote(string(out)), nil
}

// parseLsRemote 把 ls-remote 的输出变成 tag 名列表。
// 每行形如 "<sha>\trefs/tags/v1.2.3"；annotated tag 还会多一行 "refs/tags/v1.2.3^{}"，
// 带 --refs 时 git 已经滤掉了，但输出是别人给的，自己再剥一次并去重。
func parseLsRemote(out string) []string {
	var tags []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name, ok := strings.CutPrefix(fields[len(fields)-1], "refs/tags/")
		if !ok { // refs/heads/… 之类，不是 tag
			continue
		}
		name = strings.TrimSuffix(name, "^{}")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tags = append(tags, name)
	}
	return tags
}

// firstLine 取 git stderr 的第一行转述给用户 —— 那句话通常就是原因。
//
// 但 `remote: …` 那些行是**远端服务器写的**，会一路进到我们的报错里、印在用户的
// 终端上。所以控制字符（ANSI 转义能改颜色和光标、\r 能重画整行把前面的话盖掉）
// 一律剥掉，长度也封顶：一个能往别人终端上任意写字的错误信息不叫错误信息。
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= maxStderrLine {
			b.WriteString("…")
			break
		}
		if r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
