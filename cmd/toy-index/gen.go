package main

// gen：读一个 owner/repo 列表，逐个产出内联条目，合并进索引 JSON。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

type genConfig struct {
	repos  []string
	out    string
	update bool
	probe  bool
	prov   provider
	host   host.Host // 只有 --probe 会用到：拼 release 下载地址
}

// genResult 是一个仓库的结局。三种：出了条目 / 有意跳过 / 失败。
type genResult struct {
	id      string
	entry   index.Entry
	pack    pack
	skip    string // 非空 = 有意跳过（已在索引里、会降档……）
	err     error
	written bool
}

func runGen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 用法统一由 printUsage 打，不要 flag 包自己再排一版
	repos := fs.String("repos", "", "")
	out := fs.String("out", filepath.Join("index", "toy-index.json"), "")
	update := fs.Bool("update", false, "")
	probe := fs.Bool("probe", false, "")
	token := fs.String("token", "", "")
	api := fs.String("api", "", "") // 托管方 API 根，只给测试和自建实例用
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || strings.TrimSpace(*repos) == "" {
		return errUsage
	}

	list, err := readRepoList(*repos)
	if err != nil {
		return err
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	gh, err := host.Lookup("github", nil)
	if err != nil {
		return err
	}
	return gen(ctx, genConfig{
		repos: list, out: *out, update: *update, probe: *probe,
		prov: newGitHub(*api, tok), host: gh,
	})
}

func gen(ctx context.Context, cfg genConfig) error {
	idx, err := loadIndex(cfg.out)
	if err != nil {
		return err
	}

	results := make([]genResult, 0, len(cfg.repos))
	changed := 0
	for _, line := range cfg.repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := genOne(ctx, cfg, idx, line)
		if r.err == nil && r.skip == "" {
			idx.Upsert(r.entry)
			r.written = true
			changed++
		}
		results = append(results, r)
	}

	if changed > 0 {
		if err := writeIndex(cfg.out, idx); err != nil {
			return err
		}
	}
	report(cfg, results, changed)

	// 一个仓库失败不影响其它，但整批一个都没成的时候不该退 0 —— CI 上那会静默过去。
	if changed == 0 && anyFailed(results) {
		return errors.New("no entries produced (every line above says why)")
	}
	return nil
}

// genOne 处理一个仓库。任何失败都只落在这一条上（返回值里），不打断整批。
func genOne(ctx context.Context, cfg genConfig, idx *index.Index, line string) genResult {
	res := genResult{id: line}

	r, err := ref.Parse(line)
	// 形态校验借 ref 而不是自己再抄一份白名单：这两段最后会拼成
	// ~/.toy/pkg/<host>/<owner>/<name>/，ref 已经在那儿拦了 ".." 和超长段，
	// 而 index.validID 用的也是它 —— 抄一份的下场就是慢慢分叉。
	switch {
	case err != nil:
		res.err = fmt.Errorf("cannot parse: %w", err)
		return res
	case r.Version != "":
		res.err = errors.New("drop the @version: an index entry names a repo, and which version gets installed is decided by git tags (spec §5)")
		return res
	case r.Kind != ref.KindIndex && !(r.Kind == ref.KindHost && r.Host == "github"):
		res.err = errors.New("only owner/repo or github:owner/repo is accepted here")
		return res
	}
	res.id = r.ID()

	// 已经在索引里的先判掉，省一次 API 调用。
	if old, ok := idx.Find(res.id); ok {
		switch {
		case old.Key != "" || old.Sig != "":
			// 重新生成必然换掉 toyfile 原文，而签名签的就是那份原文 —— 覆盖等于
			// 把一个「✓」悄悄弄没。规格 §5 说签名消失要停下来让人决定，那这里就
			// 别替人决定。--update 也不放行。
			res.skip = "the entry in the index is signed; leaving it alone (regenerating would invalidate the signature)"
			return res
		case old.Toyfile == "":
			// 指针条目 = 上游自己有 toy.toml，客户端装出来是 host 档。换成内联
			// 就是 TrustHost → TrustIndexed 的降档（§5.1 第 6 条），不是刷新。
			res.skip = "the entry in the index is a pointer (upstream ships its own toy.toml); inlining it would downgrade trust, so leaving it alone"
			return res
		case !cfg.update:
			res.skip = "already in the index, skipping (pass --update to overwrite)"
			return res
		}
	}

	info, err := cfg.prov.Fetch(ctx, r.Owner, r.Repo)
	if err != nil {
		res.err = err
		return res
	}
	// 客户端靠 `git ls-remote --tags` + semver 挑版本（规格 §2），所以 release 的
	// tag 必须是 v?X.Y.Z。jqlang/jq 那种 `jq-1.8.2` 的 tag 一个都解析不出来 ——
	// 条目照样生成得出来，而 `toy install` 第一步就死在「这些 tag 里没有 semver
	// 版本」，报错里连"是因为 tag 的形状"都看不出来。
	// 这和 unstableBinPath 是同一类判断：装不下去的条目不该进索引。
	if _, verr := semver.Parse(info.Tag); verr != nil {
		res.err = fmt.Errorf("the latest release is tagged %q, which is not semver (it has to look like v1.2.3). "+
			"The client picks versions with `git ls-remote --tags` + semver, so it would not recognise a "+
			"single version here -- listing this repo produces an entry nobody can install", tomlSafe(info.Tag, 64))
		return res
	}

	p, err := makePack(r.Owner, r.Repo, info)
	if err != nil {
		res.err = err
		return res
	}

	if cfg.probe {
		switch hit, cands, err := probeBin(ctx, cfg.host, p); {
		case err != nil:
			// 探不到不算失败：条目照出，只是 [bin] 仍然是猜的，报告里会说。
			p.notes = append(p.notes, "probe failed: "+err.Error())
		case hit != "":
			if why := unstableBinPath(hit, p.res.Version); why != "" {
				// 探到的路径靠不住时【整条判失败】，不产出条目：一条今天能装、
				// 上游一发新版就装不上的记录，和留着 [sha256] 是同一类错误。
				// 报错里把探到的路径原样给出来，人要手写的时候答案就在眼前。
				// 唯一那层顶层目录解压时已经被剥掉了，所以走到这里的是剥不掉的那种：
				// 顶层不止一个条目，或者路径本身就带着版本号 / 平台名。
				res.err = fmt.Errorf("the probed in-package path is %q: %s. "+
					"Write this entry by hand (or get the author to add their own toy.toml)", hit, why)
				return res
			}
			p.binPath, p.binProbed = hit, true
			// 探到的那个文件的**基名**才是作者真正发出来的命令名。默认值来自
			// 资产名前缀（ripgrep-15.2.0-….tar.gz → ripgrep），而包里那个文件
			// 叫 rg —— 照默认值写就会往 PATH 上放一个全世界的文档里都没有的
			// 名字（`ripgrep --version` 不存在），而 [bin] 右边又是对的，
			// 所以装完能跑、只是没人叫得出它。探到了就以探到的为准。
			if base := path.Base(hit); base != p.binName && cleanName(base) {
				p.notes = append(p.notes, fmt.Sprintf(
					"command name taken from the probed file: %q (the asset-name prefix was %q)", base, p.binName))
				p.binName = base
			}
		default:
			p.notes = append(p.notes, "the archive holds several executables, so [bin] cannot be settled "+
				"automatically -- pick one: "+strings.Join(head(cands, 8), ", "))
		}
	}

	data, err := renderToyfile(p)
	if err != nil {
		res.err = err
		return res
	}
	res.pack = p
	res.entry = index.Entry{
		ID:      res.id,
		Source:  "github:" + r.Owner + "/" + r.Repo,
		Desc:    tomlSafe(info.Desc, descMax),
		Stars:   info.Stars,
		Toyfile: string(data),
	}
	return res
}

func loadIndex(path string) (*index.Index, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		ui.Info("%s does not exist yet; a new one will be created", path)
		return &index.Index{Version: index.Version}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read index %s: %w", path, err)
	}
	idx, skipped, err := index.Load(data, path)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		// Load 会把坏记录丢掉，而我们马上要把整份写回去 —— 不说一声就等于
		// 悄悄删了别人的条目。
		ui.Warn("%s holds %d invalid records; they were dropped, and writing the file back removes them for good", path, skipped)
	}
	return idx, nil
}

func writeIndex(path string, idx *index.Index) error {
	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("cannot write index %s: %w", path, err)
	}
	return nil
}

func anyFailed(rs []genResult) bool {
	for _, r := range rs {
		if r.err != nil {
			return true
		}
	}
	return false
}

// report 打一份给人过目的报告。
//
// 表格先用 tabwriter 排进内存缓冲、再逐行交给 ui —— desc、资产名、错误里的
// 服务器文本全是远端内容，直接 print 的话一串 "\r\x1b[32m✓" 能把上面的行盖掉
// 重写（§14.4）。ui 会把控制字符转义成看得见的 \xNN。
func report(cfg genConfig, rs []genResult, changed int) {
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rs {
		switch {
		case r.err != nil:
			fmt.Fprintf(tw, "✗\t%s\t%v\n", r.id, r.err)
		case r.skip != "":
			fmt.Fprintf(tw, "-\t%s\t%s\n", r.id, r.skip)
		default:
			how := "guessed"
			if r.pack.binProbed {
				how = "probed"
			}
			fmt.Fprintf(tw, "✓\t%s\t%d platforms, bin %s -> %s (%s)\n",
				r.id, len(r.pack.res.Assets), r.pack.name, r.pack.binPath, how)
		}
	}
	tw.Flush()
	ui.Info("")
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		ui.Info("%s", l)
	}

	for _, r := range rs {
		for _, n := range r.pack.notes {
			ui.Warn("%s: %s", r.id, n)
		}
	}

	// §14.9 第 2 条：[bin] 左边那些名字是这个功能里唯一还需要人过一眼的东西。
	// reservedLinkNames 挡住了 git、PATH 遮蔽检查挡住了已存在的命令，但一个
	// PATH 上还不存在的名字是能被占的。
	//
	// 这里【不】重抄一份保留名单：那份名单在 internal/store 里，抄第二份就多一处
	// 会静默失效的地方（§14.1 的教训就是这个形状）。客户端安装时一定会拒，
	// 这里只负责让人看见。
	names := reportBinNames(rs)
	if len(names) > 0 {
		ui.Info("")
		ui.Info("These entries will put the following command names on PATH -- check every one before committing:")
		var nb strings.Builder
		ntw := tabwriter.NewWriter(&nb, 0, 0, 2, ' ', 0)
		for _, n := range names {
			fmt.Fprintf(ntw, "    %s\n", n)
		}
		ntw.Flush()
		for _, l := range strings.Split(strings.TrimRight(nb.String(), "\n"), "\n") {
			ui.Info("%s", l)
		}
	}

	ui.Info("")
	if changed > 0 {
		ui.Info("Wrote %d entries to %s. Every one is an inline entry (spec §5.1): whoever installs it sees", changed, cfg.out)
		ui.Info("the downgraded trust level, and there is no [sha256] -- inline entries follow the upstream's")
		ui.Info("newest tag, so pinning one version's bytes turns the next release into a checksum mismatch.")
	} else {
		ui.Info("Nothing changed; %s was left untouched.", cfg.out)
	}
	if q := cfg.prov.Quota(); q != "" {
		ui.Info("%s", q)
	}
}

// reportBinNames 是报告最后那一列：这批条目会往 PATH 上放哪些命令名。
//
// 印的必须是 binName（真正会落到 PATH 上的那个），不是 name（包目录名）——
// ripgrep 那种仓库上两者不一样。这一列是 §14.9 第 2 条留给人的唯一关口。
func reportBinNames(rs []genResult) []string {
	var names []string
	for _, r := range rs {
		if r.written {
			names = append(names, fmt.Sprintf("%s\t← %s", r.pack.binName, r.id))
		}
	}
	return names
}
