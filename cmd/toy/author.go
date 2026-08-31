package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// runKeygen 生成密钥对存进 ~/.toy/keys，打印公钥和指纹。已存在时拒绝覆盖。
func runKeygen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}
	kp, err := sign.Generate()
	if err != nil {
		return err
	}
	if err := sign.SaveKeyPair(st.KeysDir(), kp); err != nil {
		// SaveKeyPair 已经说清楚是哪个文件挡住了，这里补的是「为什么不给 --force」：
		// 覆盖私钥等于永久丢掉这个名字 —— 旧钥匙签过的条目再也签不出来，
		// 索引里那个 key 字段一换，所有认得旧钥匙的人看到的就是一次密钥变更。
		return fmt.Errorf("%w。toy 不提供覆盖选项：换一把钥匙就等于放弃用旧钥匙认领过的名字，"+
			"旧钥匙签过的条目谁也再签不出来 —— 真要重来只能自己动手把那个文件挪走", err)
	}

	ui.Info("密钥对已生成：%s（私钥 key.sec 是 0600，别拷给任何人、也别提交进仓库）", st.KeysDir())
	ui.Info("指纹 %s —— 别人安装时打印的就是这一串", sign.Fingerprint(kp.Public))
	ui.Info("下一步：把下面这行公钥贴进索引条目的 \"key\" 字段，那就是认领这个名字；")
	ui.Info("在项目目录里跑 `toy publish` 会直接生成好带 key 的整条条目。")
	// 机器可读的走 stdout（规格 §9），这样 `toy keygen | pbcopy` 拿到的就是纯公钥。
	fmt.Println(hex.EncodeToString(kp.Public))
	return nil
}

// autSignMessage 是作者签名覆盖的字节，**这个定义定死了不能再改**：
//
//	id + "\n" + source + "\n" + toy.toml 的原始字节
//
// 改一个字节，所有已经发出去的签名会在同一天全部验不过，而用户看到的是
// 「签名不对」—— 那句话读起来像作者的私钥泄漏了，不像我们改了格式。
// 真要换定义只能是加一个带版本号的新字段，让两种签名并存一段时间。
//
// 用原始字节而不是解析后的 Toyfile：签名要覆盖的是「用户真正会下载到的那份文件」，
// 重新序列化一遍等于给了一个能在两份不同文本上算出同一个签名的机会。
func autSignMessage(id, source string, toyTOML []byte) []byte {
	msg := make([]byte, 0, len(id)+len(source)+len(toyTOML)+2)
	msg = append(msg, id...)
	msg = append(msg, '\n')
	msg = append(msg, source...)
	msg = append(msg, '\n')
	return append(msg, toyTOML...)
}

// runPublish 读目录里的 toy.toml，校验，用本机私钥签名，
// 打印一条可以直接粘进索引 JSON 的条目。
// 不联网、不提交 —— v1 索引是本地文件，粘过去就行（规格 §5）。
func runPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	source := fs.String("source", "", "索引条目的 source，形如 github:owner/repo；不写就从 git 的 origin 推")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() > 1 {
		return errUsage
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	path := filepath.Join(dir, "toy.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读不到 %s：%w。publish 读的是这个目录里的 toy.toml —— "+
			"8 行就够（name / [bin] / [artifact]），不用求任何人批准", path, err)
	}
	// 先校验再签名：签一个自己都装不上的 toy.toml，只是把错误推迟到别人机器上。
	tf, err := toyfile.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s 还不能发布：%w", path, err)
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	src := strings.TrimSpace(*source)
	if src == "" {
		if src, err = autGitSource(ctx, dir, cfg.Hosts); err != nil {
			return err
		}
	}
	r, err := ref.Parse(src)
	if err != nil {
		return fmt.Errorf("source %q 解析不了：%w", src, err)
	}
	if r.Kind != ref.KindHost {
		return fmt.Errorf("source 要写成 github:owner/repo 这种形态（前缀可以是 gitlab: / codeberg: "+
			"或者你在 ~/.toy/config.toml 里自己加的托管方），拿到的是 %q。"+
			"裸 owner/repo 会让客户端回头再查一次索引（那是个环）；"+
			"直接指向一个 toy.toml 地址的条目现在只能手写，因为那种写法里没有 owner/repo 可以当 id", src)
	}
	if r.Version != "" {
		return fmt.Errorf("source 里不要带 @%s：索引条目指的是仓库，装哪一版由 git tag 决定（规格 §5）。"+
			"钉死版本的话，你以后每发一版都得回来改索引", r.Version)
	}
	if tf.Desc == "" {
		ui.Warn("toy.toml 里没有 desc，条目的描述会是空的 —— 而 `toy search` 主要就搜它")
	}

	entry := index.Entry{
		ID:     r.ID(),
		Source: r.String(),
		Desc:   tf.Desc,
	}

	// 签名是可选升级不是入场券（规格 §4）：没有密钥对照样输出条目，照样能被收进索引。
	switch kp, err := sign.LoadKeyPair(st.KeysDir()); {
	case errors.Is(err, sign.ErrNoKey):
		ui.Info("本机还没有密钥对，这条是未签名的条目 —— 它一样能被收进索引、一样能装。")
		ui.Info("跑 `toy keygen` 生成一把钥匙就能认领 %s 这个名字，之后 publish 会自动带上 key 和 sig。", entry.ID)
	case err != nil:
		return err
	default:
		entry.Key = hex.EncodeToString(kp.Public)
		entry.Sig = sign.Sign(kp.Private, autSignMessage(entry.ID, entry.Source, raw))
		ui.Info("已用 %s 签名（签的是 id + source + 这份 toy.toml 的原文）", sign.Fingerprint(kp.Public))
	}

	out, err := json.MarshalIndent(entry, "  ", "  ")
	if err != nil {
		return fmt.Errorf("生成索引条目失败：%w", err)
	}
	// 条目走 stdout，说明走 stderr（规格 §9），所以 `toy publish > entry.json` 拿到的是纯 JSON。
	fmt.Printf("  %s\n", out)
	ui.Info("把上面这段粘进索引 JSON 的 packages 数组里就发布完了（想加 tags 就自己补一行 \"tags\": [\"web\"]）。")
	ui.Info("toy 不替你联网、不替你提交 —— 索引就是一个 JSON 文件，谁 fork 一份谁就有一个平行索引。")
	return nil
}

// autGitSource 从 git 的 origin 推出索引条目的 source。
func autGitSource(ctx context.Context, dir string, overrides map[string]host.Host) (string, error) {
	remote, err := autGitRemote(ctx, dir)
	if err != nil {
		return "", err
	}
	domain, owner, repo, err := autParseRemote(remote)
	if err != nil {
		return "", err
	}
	name := autHostName(domain, overrides)
	if name == "" {
		return "", fmt.Errorf("origin 指向 %s，但 toy 的托管方表里没有这个域名。"+
			"要么用 --source 直接指定（如 --source github:%s/%s），"+
			"要么在 ~/.toy/config.toml 里加一段 [hosts.你起的名字]（四个模板：raw / clone / release / web）",
			domain, owner, repo)
	}
	return name + ":" + owner + "/" + repo, nil
}

// autGitRemote 取这个目录的 origin 地址。
// 只读一行配置，不联网、不执行仓库里的任何东西（铁律 1）。
func autGitRemote(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("找不到 git 命令，推不出 source。用 --source github:owner/repo 直接指定")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%s 不是 git 仓库，或者它没有叫 origin 的 remote —— "+
			"推不出这个包在哪。用 --source github:owner/repo 直接指定", dir)
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return "", errors.New("origin 的地址是空的。用 --source github:owner/repo 直接指定")
	}
	return remote, nil
}

// autParseRemote 把 origin 地址拆成域名 + owner + repo。认两种形态：
//
//	https://github.com/owner/repo.git   （scheme 形态，ssh:// git:// 也走这条）
//	git@github.com:owner/repo.git       （scp 形态，没有 scheme）
//
// 只做拆分不做字符校验 —— 拼出来的 source 会过 ref.Parse，那里才是白名单的家。
func autParseRemote(remote string) (domain, owner, repo string, err error) {
	s := strings.TrimSpace(remote)
	var hostPart, path string
	switch {
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", "", fmt.Errorf("origin %q 不是合法的地址：%w。用 --source github:owner/repo 直接指定", s, perr)
		}
		hostPart, path = u.Hostname(), u.Path
	case strings.Contains(s, ":") && !strings.HasPrefix(s, "/"):
		// scp 形态：冒号左边是 [user@]host，右边是路径。
		hostPart, path, _ = strings.Cut(s, ":")
		if _, after, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = after
		}
	default:
		// 本地路径的 origin（/srv/git/x.git 之类）没有域名，推不出托管方。
		return "", "", "", fmt.Errorf("origin 是 %q，看不出是哪家托管方（本地路径的 remote 没有域名）。"+
			"用 --source github:owner/repo 直接指定", s)
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := strings.Split(path, "/")
	if hostPart == "" || len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		// GitLab 的子组（a/b/c）会落到这里：ref 只有 owner/repo 一层，装不了，
		// 与其推出一个错的 source，不如说清楚（铁律 8 的同一个道理）。
		return "", "", "", fmt.Errorf("从 origin %q 里认不出 owner/repo —— "+
			"toy 的包引用只有 owner/repo 一层（GitLab 子组这种多层路径现在不支持）。"+
			"用 --source github:owner/repo 直接指定", s)
	}
	return strings.ToLower(hostPart), segs[0], segs[1], nil
}

// autHostName 按域名反查托管方前缀（github.com → github）。
// 查的是 host 表本身，所以用户在 config.toml 里加的自建实例自动也认得
// —— 托管方是配置不是代码，这里也不该再抄一份域名清单。
func autHostName(domain string, overrides map[string]host.Host) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return ""
	}
	// 用户自己配的优先（他可能覆盖了内置的 github）。同一域名有多条时按名字排序取第一条：
	// map 的遍历顺序是随机的，不排的话同一个仓库两次 publish 能给出两个不同的 source。
	for _, table := range []map[string]host.Host{overrides, host.Builtin()} {
		for _, name := range slices.Sorted(maps.Keys(table)) {
			if autTemplateHost(table[name].Clone) == domain {
				return strings.ToLower(name)
			}
		}
	}
	return ""
}

// autTemplateHost 取一个 URL 模板里的域名。模板里的 {owner} 之类不影响主机名那一段。
func autTemplateHost(tmpl string) string {
	u, err := url.Parse(strings.TrimSpace(tmpl))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// autShutdownGrace 是 Ctrl-C 之后等在途请求收尾的时间。
// 有人正在下一个 200 MB 的包时，不给这几秒就是把他的下载剪断。
const autShutdownGrace = 5 * time.Second

// runServe 把 ~/.toy/cache 当静态目录起个 HTTP server（文件名就是 sha256）。
// 因为内容是 hash 寻址的，这个镜像天然不需要被信任。规格 §8。
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "监听地址。默认只听本机，要给别人用就写 0.0.0.0:8080")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	st, _, err := openStore()
	if err != nil {
		return err
	}

	// 先 Listen 再 Serve：端口被占用时要当场报错，而不是让用户对着一句
	// "已启动" 去 curl 一个不存在的服务。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败：%w", *addr, err)
	}
	shown := ln.Addr().String()
	if autWildcard(*addr) {
		ui.Warn("正在对整个网络提供服务（%s）。缓存里的每个文件名都是它自己的 sha256，"+
			"所以内容没法被换掉；但谁在下什么你就看不住了，公网上跑之前想一下这件事", shown)
		if _, port, perr := net.SplitHostPort(shown); perr == nil {
			shown = "你这台机器的地址:" + port
		}
	}

	ui.Info("镜像已启动，服务的是 %s", st.CacheDir())
	ui.Info("别人这样用它（写进他的 ~/.toy/config.toml）：")
	ui.Info("")
	ui.Info("    mirrors = [\"http://%s\"]", shown)
	ui.Info("")
	ui.Info("镜像只在 toy.toml 写了 [sha256] 时才会被用到 —— 没有 hash 就没法验证镜像给的是不是那个东西。")
	ui.Info("Ctrl-C 停止。")

	srv := &http.Server{
		Handler: autMirrorHandler(st.CacheDir()),
		// 只给读头设死线：正文是几百 MB 的包，给 WriteTimeout 等于给慢速客户端判死刑。
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("镜像挂了：%w", err)
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), autShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			ui.Warn("有下载还没结束，直接断掉了：%v", err)
		}
		return ctx.Err() // main 把它打成「已取消」并 exit 130
	}
}

// autMirrorHandler 只做一件事：GET/HEAD 一个 64 位 hex 的路径，回缓存里那个文件。
//
// 故意不用 http.FileServer：它会列目录、会跟着任意路径走，而这个服务可能开在公网上，
// 缓存目录里将来也未必只有 hash 命名的文件。路径精确匹配 sha256，别的一律 404。
func autMirrorHandler(cacheDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "这个镜像只支持 GET 和 HEAD", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !autIsSHA256(name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(cacheDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		// 自己定 Content-Type，ServeContent 就不会去嗅探内容 —— 缓存里全是
		// 二进制包，被嗅成 text/html 只会让浏览器和中间层各自发挥。
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", fi.ModTime(), f) // 顺带白捡 Range 支持
	})
}

// autIsSHA256 判断是不是 64 位小写 hex。缓存文件名就是这个形状（fetch 那边写的）。
func autIsSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// autWildcard 判断这个监听地址是不是对所有网卡开着。
func autWildcard(addr string) bool {
	h, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		h = strings.TrimSpace(addr)
	}
	h = strings.Trim(h, "[]")
	switch h {
	case "", "0.0.0.0", "::", "*":
		return true
	}
	return false
}
