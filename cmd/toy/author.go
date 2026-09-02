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
		return fmt.Errorf("%w. toy offers no overwrite flag: swapping keys means giving up every name the old "+
			"key claimed, and nobody can re-sign the entries it signed — if you really want to start over, "+
			"move that file aside yourself", err)
	}

	ui.Info("Key pair written to %s (key.sec is mode 0600 — do not copy it anywhere, do not commit it)", st.KeysDir())
	ui.Info("Fingerprint %s — this is the string other people see when they install", sign.Fingerprint(kp.Public))
	ui.Info("Next: paste the public key below into the \"key\" field of an index entry — that is how you claim a name.")
	ui.Info("Running `toy publish` in the project directory writes the whole entry, key included.")
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
//
// --inline 是另一件事，给的是**索引维护者**：为一个自己不拥有的仓库写打包说明。
// 那份 toy.toml 会原文进条目的 toyfile 字段，客户端只在上游仓库确实没有自己那份
// 时才用它，装出来带降档标识（store.TrustIndexed）。索引冷启动靠的就是它 ——
// 现存几千个 CLI 工具一个都没有 toy.toml。
func runPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	source := fs.String("source", "", "the entry's source, like github:owner/repo; inferred from the git origin if omitted")
	inline := fs.Bool("inline", false, "inline this toy.toml verbatim into the entry: packaging instructions for a repository that is NOT yours (it steps aside the moment upstream ships its own toy.toml)")
	// 借 install 那套解析：-h 才会把上面两行说明打出来，而 --inline 和普通 publish
	// 是两件事，那个区别只写在这里。
	if err := insParseFlags(fs, args); err != nil {
		return err
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
		return fmt.Errorf("cannot read %s: %w. publish reads the toy.toml in this directory — "+
			"eight lines is enough (name / [bin] / [artifact]), and nobody has to approve it", path, err)
	}
	// 先校验再签名：签一个自己都装不上的 toy.toml，只是把错误推迟到别人机器上。
	tf, err := toyfile.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not ready to publish: %w", path, err)
	}

	st, cfg, err := openStore()
	if err != nil {
		return err
	}

	src := strings.TrimSpace(*source)
	if src == "" {
		if src, err = autGitSource(ctx, dir, st.BinDir(), cfg.Hosts); err != nil {
			return err
		}
	}
	r, err := ref.Parse(src)
	if err != nil {
		return fmt.Errorf("the source %q does not parse: %w", src, err)
	}
	if r.Kind != ref.KindHost {
		return fmt.Errorf("the source has to look like github:owner/repo (the prefix can be gitlab: / codeberg: "+
			"or any host you added in ~/.toy/config.toml), got %q. A bare owner/repo would send the client back "+
			"to an index lookup, which is a loop; and an entry pointing straight at a toy.toml URL still has to "+
			"be written by hand, because that form carries no owner/repo to use as the id", src)
	}
	if r.Version != "" {
		return fmt.Errorf("leave the @%s off the source: an index entry names a repository, and which version "+
			"gets installed is decided by the git tags. Pin a version here and you have to come back and edit "+
			"the index on every release", r.Version)
	}
	if tf.Desc == "" {
		ui.Warn("toy.toml has no desc, so the entry's description will be empty — and that is mostly what `toy search` searches")
	}

	entry := index.Entry{
		ID:     r.ID(),
		Source: r.String(),
		Desc:   tf.Desc,
	}
	if *inline {
		// 内联条目必须【与版本无关】—— 它住在索引里，靠 git ls-remote 跟着上游最新
		// 的 tag 走，而 [sha256] 钉的是某一个版本的字节。留着它，上游下一次发版
		// 就会让每个用户拿到「哈希对不上，拒绝安装」，读起来完全像投毒警告，
		// 而实际上只是作者正常发了个新版。所以在这里剥掉。
		//
		// 剥必须发生在签名【之前】：签的就是内联进去的这份原文（autSignMessage）。
		// （自己那个仓库不受影响：那份 toy.toml 和资产一起打在同一个 tag 上，
		// 哈希就是对的，那里该留着 —— 只有替别人写的这条不能留。）
		if stripped, ok := autStripSHA256(raw); ok {
			raw = stripped
			ui.Warn("Stripped [sha256] from the inlined toy.toml: it pins the bytes of one specific version,")
			ui.Info("while an inline entry follows whatever tag upstream publishes next. Left in, the author's")
			ui.Info("next release would hand everyone a checksum mismatch. First installs are still pinned by TOFU.")
		}
		// 原文进条目：客户端验签名验的就是这份原文（autSignMessage），
		// 重新序列化一遍等于换了一份文本（同 rsvGetToyfile 那条注释）。
		entry.Toyfile = string(raw)
		ui.Warn("This is an INLINE entry: you are writing packaging instructions for %s, which is not your repository.", entry.Source)
		ui.Info("Whoever installs it sees a reduced-trust line: the binary comes from the author's release, but")
		ui.Info("which asset and what the command is called come from the index — so write the [bin] names")
		ui.Info("the way the author meant them. The day upstream adds its own toy.toml, clients switch to it")
		ui.Info("and this inline entry steps aside on its own.")
	}

	// 签名是可选升级不是入场券（规格 §4）：没有密钥对照样输出条目，照样能被收进索引。
	switch kp, err := sign.LoadKeyPair(st.KeysDir()); {
	case errors.Is(err, sign.ErrNoKey):
		ui.Info("There is no key pair on this machine, so this entry is unsigned — it can still be listed and still installs.")
		ui.Info("Run `toy keygen` to make one and claim the name %s; after that publish adds the key and sig for you.", entry.ID)
	case err != nil:
		return err
	default:
		entry.Key = hex.EncodeToString(kp.Public)
		entry.Sig = sign.Sign(kp.Private, autSignMessage(entry.ID, entry.Source, raw))
		ui.Info("Signed with %s (the signature covers id + source + the verbatim bytes of this toy.toml)", sign.Fingerprint(kp.Public))
		if *inline {
			// 内联那一档永远到不了「✓ 已签名」（规则 2）：签名者是写这条记录的人，
			// 不是作者。这里必须说明白，否则维护者会以为签了名就等于背书。
			ui.Info("A signature on an inline entry only proves this record was not altered. It will NOT make it")
			ui.Info("show up as \"✓ signed\" — that check mark means the author signed, and you wrote these instructions.")
		}
	}

	out, err := json.MarshalIndent(entry, "  ", "  ")
	if err != nil {
		return fmt.Errorf("cannot render the index entry: %w", err)
	}
	// 条目走 stdout，说明走 stderr（规格 §9），所以 `toy publish > entry.json` 拿到的是纯 JSON。
	fmt.Printf("  %s\n", out)
	ui.Info("Paste the block above into the packages array of an index JSON and you have published (add a line")
	ui.Info("like \"tags\": [\"web\"] yourself if you want tags). toy does not go online for you and does not commit")
	ui.Info("for you — an index is just a JSON file, and anyone who forks one has a parallel index.")
	return nil
}

// autGitSource 从 git 的 origin 推出索引条目的 source。
// binDir 是 toy 自己的 bin 目录，autGitPath 要用它把那个目录排除掉。
func autGitSource(ctx context.Context, dir, binDir string, overrides map[string]host.Host) (string, error) {
	remote, err := autGitRemote(ctx, dir, binDir)
	if err != nil {
		return "", err
	}
	domain, owner, repo, err := autParseRemote(remote)
	if err != nil {
		return "", err
	}
	name := autHostName(domain, overrides)
	if name == "" {
		return "", fmt.Errorf("origin points at %s, and that domain is not in toy's host table. "+
			"Either name it directly (--source github:%s/%s), or add a [hosts.<name you pick>] section "+
			"in ~/.toy/config.toml with the four templates: raw / clone / release / web",
			domain, owner, repo)
	}
	return name + ":" + owner + "/" + repo, nil
}

// autGitPath 找 git 的绝对路径，**跳过 toy 自己的 bin 目录**。
//
// 裸 "git" 交给 exec 就是执行 PATH 上的第一个 git，而 toy 的 bin 目录按文档就在
// PATH 上 —— 一个装上来的包只要提供一条叫 git 的命令，`toy publish` 就会执行它。
// 那是铁律 1 的洞，不是不方便。
//
// 这是 internal/host 里 lookupGit 的第二份实现（那边还没导出可用的入口）。
// **两处的规则必须一字不差地保持一致**：走 PATH、跳过相对项、跳过 ~/.toy/bin、
// 取第一个能跑的普通文件。这边松一格，publish 就成了绕过去的那条路。
// internal/host 哪天把入口导出来，这里和 qryCheckGit 一起换过去，把这份删掉。
func autGitPath(binDir string) (string, error) {
	var inToyBin string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// 相对项（"." 和 PATH 里那个到处都是的空项）指的是进程当前所在的目录。
		// 在一个别人写得进去的目录里跑 toy，不该等于把铁律 1 交出去。
		if !filepath.IsAbs(dir) {
			continue
		}
		cand := filepath.Join(dir, "git")
		// 跟随软链：bin/ 里全是软链，pkg 里的真身才是那个普通文件。
		if fi, err := os.Stat(cand); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		if binDir != "" && autSameDir(dir, binDir) {
			inToyBin = cand // 只为报错时说得清楚，继续往后找真的 git
			continue
		}
		return cand, nil
	}
	if inToyBin != "" {
		return "", fmt.Errorf("refusing to run it: the only git on PATH is %s, which lives in toy's own bin "+
			"directory — meaning it came from a package somebody installed. Running it would hand that package "+
			"execution, and toy never executes anything from a package. This is very likely an attack. "+
			"Use `toy list` to find which package put it there, remove it, and install a real git; "+
			"or pass --source github:owner/repo, which does not touch git at all", inToyBin)
	}
	return "", errors.New("no git command found, so the source cannot be inferred. Pass --source github:owner/repo instead")
}

// autSameDir / autCanonDir：先转绝对路径再解软链（/usr/bin 常常是 /bin 的软链），
// 解不动就退回字符串比较。和 internal/host、internal/store 里的同名函数是一份东西。
func autSameDir(a, b string) bool { return autCanonDir(a) == autCanonDir(b) }

func autCanonDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// autGitRemote 取这个目录的 origin 地址。
// 只读一行配置，不联网、不执行仓库里的任何东西（铁律 1）。
func autGitRemote(ctx context.Context, dir, binDir string) (string, error) {
	gitPath, err := autGitPath(binDir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, gitPath, "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%s is not a git repository, or it has no remote called origin, so there is no "+
			"way to tell where this package lives. Pass --source github:owner/repo instead", dir)
	}
	remote := strings.TrimSpace(string(out))
	if remote == "" {
		return "", errors.New("the origin URL is empty. Pass --source github:owner/repo instead")
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
			return "", "", "", fmt.Errorf("the origin %q is not a valid URL: %w. Pass --source github:owner/repo instead", s, perr)
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
		return "", "", "", fmt.Errorf("the origin is %q, which does not say which host it is — a remote that is "+
			"a local path has no domain. Pass --source github:owner/repo instead", s)
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segs := strings.Split(path, "/")
	if hostPart == "" || len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		// GitLab 的子组（a/b/c）会落到这里：ref 只有 owner/repo 一层，装不了，
		// 与其推出一个错的 source，不如说清楚（铁律 8 的同一个道理）。
		return "", "", "", fmt.Errorf("cannot make out owner/repo from the origin %q — a toy package reference "+
			"has exactly one level of owner/repo, and deeper paths like GitLab subgroups are not supported yet. "+
			"Pass --source github:owner/repo instead", s)
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
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on. The default is local-only; use 0.0.0.0:8080 to serve others")
	if err := insParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
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
		return fmt.Errorf("cannot listen on %s: %w", *addr, err)
	}
	shown := ln.Addr().String()
	if autWildcard(*addr) {
		ui.Warn("Serving on every interface (%s). Every file in the cache is named after its own sha256, so "+
			"the contents cannot be swapped — but you no longer control who downloads what. Think about that "+
			"before running this on a public network", shown)
		if _, port, perr := net.SplitHostPort(shown); perr == nil {
			shown = "your-machine-address:" + port
		}
	}

	ui.Info("Mirror is up, serving %s", st.CacheDir())
	ui.Info("Other people use it by putting this in their ~/.toy/config.toml:")
	ui.Info("")
	ui.Info("    mirrors = [\"http://%s\"]", shown)
	ui.Info("")
	ui.Info("A mirror is only ever consulted when toy.toml declares [sha256] — without a hash there is no way")
	ui.Info("to check that a mirror handed back the right thing.")
	ui.Info("Ctrl-C to stop.")

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
		return fmt.Errorf("the mirror stopped: %w", err)
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), autShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			ui.Warn("downloads were still in flight and got cut off: %v", err)
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
			http.Error(w, "this mirror only supports GET and HEAD", http.StatusMethodNotAllowed)
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

// autStripSHA256 从 toy.toml 原文里去掉整个 [sha256] 段，其余字节一字不动。
// 只在 --inline 那条路上用，理由见调用点。返回是否真的去掉了东西。
//
// 手写而不是 Unmarshal 再 Marshal：客户端验签名验的是这份原文，
// 重新序列化一遍会换掉注释、顺序和空白，等于换了一份文本。
func autStripSHA256(raw []byte) ([]byte, bool) {
	lines := strings.Split(string(raw), "\n")
	head := -1 // [sha256] 那一行本身
	for i, l := range lines {
		if strings.TrimSpace(l) == "[sha256]" {
			head = i
			break
		}
	}
	if head < 0 {
		return raw, false
	}
	start := head
	// 往前吞掉紧挨着的注释和空行 —— 那几行讲的就是 sha256，留下来会变成
	// 一段指着不存在的段落的说明。遇到别的内容就停。
	for start > 0 {
		prev := strings.TrimSpace(lines[start-1])
		if prev == "" || strings.HasPrefix(prev, "#") {
			start--
			continue
		}
		break
	}
	end := len(lines)
	for i := head + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	return []byte(strings.Join(out, "\n")), true
}
