package main

// toy init：从 ./dist 里的文件名反推一份 toy.toml。
//
// 作者路径的第一步是"手写 [artifact] 的映射"，而各家资产命名习惯乱得很
// （x86_64 vs amd64、darwin vs macos vs apple-darwin、.tar.gz vs .zip），
// 抄错一个词的下场是别人装的时候一句「没有适配当前平台的构建」。
// 这条命令把它变成"跑一下然后改两个字"。
//
// 两件事故意不做：
//
//   - 不联网、不碰任何托管方的 API（规格 §2）。dist 目录就是 goreleaser 的输出布局，
//     文件名里该有的信息全都有。
//   - 不跑 `git describe`。版本号是从文件名里认出来的 —— 文件名里没有版本的话，
//     也就没有东西需要替换成 {version}，git 一点忙都帮不上。而"按名字 exec git"
//     要连带把 §14.1 那套「跳过 ~/.toy/bin」的防御再抄一份到 cmd/toy 里，
//     多一份就多一处会静默失效的地方，换不来任何东西。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// iniMaxListed 是报错时最多列几个看到的文件名 —— 一个塞了两百个文件的 dist
// 不该把终端刷满。
const iniMaxListed = 12

// iniAsset 是 dist 里认出来的一个资产。
type iniAsset struct {
	file string // 原文件名
	key  string // toy.toml 里 [artifact] 的 key
	tmpl string // 把版本号换成 {version} 之后的资产名
	sum  string // sha256
}

// ctx 用不上：这条命令不联网，也不 exec 任何东西（见文件头）。
func runInit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dist := fs.String("dist", "", "directory holding the release assets (defaults to dist/ under the project directory)")
	name := fs.String("name", "", "tool name; inferred from the directory or asset names if omitted")
	version := fs.String("version", "", "version of this batch of assets; read from the file names if omitted")
	force := fs.Bool("force", false, "overwrite an existing toy.toml")
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
	distDir := *dist
	if distDir == "" {
		distDir = filepath.Join(dir, "dist")
	}

	files, err := iniListFiles(distDir)
	if err != nil {
		return err
	}

	// 推断本身在 internal/assetname 里 —— 批量索引生成器（cmd/toy-index）用的是
	// 同一个函数。两份实现必然漂移，而漂移的症状是索引里生成的条目和作者自己跑
	// toy init 得到的不一样，且没有任何地方会说。
	res := assetname.Infer(files, *version)
	skipped := iniSkipLines(res.Skipped)
	if len(res.Assets) == 0 {
		return iniNothingRecognized(distDir, files, skipped)
	}
	assets := make([]iniAsset, len(res.Assets))
	for i, a := range res.Assets {
		assets[i] = iniAsset{file: a.File, key: a.Platform, tmpl: a.Template}
	}

	// 文件就在本地，算一遍哈希几乎不要钱，而这把包从"首次安装 TOFU 钉住"
	// 升到"强制校验"（铁律 4）—— 少这一步就等于让每个装它的人都当一次小白鼠。
	for i := range assets {
		sum, err := iniSum(filepath.Join(distDir, assets[i].file))
		if err != nil {
			return fmt.Errorf("cannot compute the sha256 of %s: %w", assets[i].file, err)
		}
		assets[i].sum = sum
	}

	pkg, guessedName := iniName(*name, dir, res.Name)
	data, err := iniRender(pkg, assets)
	if err != nil {
		return err
	}
	// 自己生成的东西也要过一遍真正的解析器：文件名里的怪字符（引号、方括号、
	// 中文）都能把 toml 写歪，而写歪的 toy.toml 是作者下一个小时的谜题。
	if _, err := toyfile.Parse(data); err != nil {
		return fmt.Errorf("the toy.toml generated from the file names in %s does not parse: %w. "+
			"Most likely a name contains characters toml cannot take — pass --name with a clean one and try again", distDir, err)
	}

	out := filepath.Join(dir, "toy.toml")
	if _, err := os.Stat(out); err == nil && !*force {
		return fmt.Errorf("%s already exists and will not be overwritten. Add --force to replace it — but check first that nothing in it was edited by hand", out)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", out, err)
	}

	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		ui.Info("%s", line)
	}
	ui.Info("")
	ui.Info("wrote %s", out)
	iniReport(pkg, assets, skipped, guessedName, res.Name)
	return nil
}

// iniListFiles 列出 dist 顶层的普通文件。子目录不进 —— goreleaser 把打好的包
// 放在顶层，子目录里是没打包的裸构建产物，收进来会和顶层的包重复。
func iniListFiles(distDir string) ([]string, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w. toy init works backwards from the FILE NAMES of release "+
			"assets that already exist, so build them first (goreleaser writes to dist/ by default), "+
			"or point --dist somewhere else", distDir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// iniSkipLines 把跳过说明拼成给人看的一行。
func iniSkipLines(skipped []assetname.Asset) []string {
	out := make([]string, len(skipped))
	for i, a := range skipped {
		out[i] = a.File + ": " + a.Skip
	}
	return out
}

// iniName 定工具名：--name > 项目目录名 > 资产名的公共前缀。
// 第二个返回值说的是"这个名字是猜出来的"，作者得核对。
func iniName(flagName, dir, prefix string) (name string, guessed bool) {
	if n := strings.TrimSpace(flagName); n != "" {
		return n, false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if base := filepath.Base(abs); base != "." && base != string(filepath.Separator) && base != "" {
			return base, true
		}
	}
	return prefix, true
}

// iniRender 拼出 toy.toml。手写而不是用 toml 编码器：这个文件是给人改的，
// 注释和字段顺序才是它的主要内容，而编码器一条注释都写不出来。
func iniRender(name string, assets []iniAsset) ([]byte, error) {
	var b strings.Builder
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
	// toml 的裸键只能是 [A-Za-z0-9_-]，别的一律加引号。平台 key 带斜杠，必须引；
	// 工具名通常不用 —— 给一个本来就干净的名字套引号只会让人以为那是必须的。
	key := func(s string) string {
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r == '-' || r == '_' || (r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) < 0 {
			return s
		}
		return q(s)
	}

	fmt.Fprintf(&b, "# Written by toy init from the release asset file names. Installing runs no scripts at all,\n")
	fmt.Fprintf(&b, "# which is why there is no install/build key here.\n")
	fmt.Fprintf(&b, "# Pushing a new tag is releasing a new version: {version} becomes that tag (without the v),\n")
	fmt.Fprintf(&b, "# so this file never has to change again.\n")
	fmt.Fprintf(&b, "name    = %s\n", q(name))
	fmt.Fprintf(&b, "desc    = \"\"  # <- one sentence saying what it does; this is what search results show\n")
	fmt.Fprintf(&b, "license = \"\"  # <- e.g. \"MIT\"\n")
	b.WriteString("\n[bin]\n")
	b.WriteString("# Left = the command name on PATH after installing. Right = the path inside the package\n")
	b.WriteString("# AFTER unpacking. When the whole tree sits in one wrapper directory (the cargo-dist style\n")
	b.WriteString("# " + name + "-1.2.3-x86_64-.../), that layer is stripped automatically, so leave it off the\n")
	b.WriteString("# right hand side. Write a subdirectory only if the package really has one, e.g. \"bin/" + name + "\".\n")
	b.WriteString("# Run `tar tzf dist/<one of the archives>` to see what is actually in there.\n")
	fmt.Fprintf(&b, "%s = %s\n", key(name), q(name))

	b.WriteString("\n[artifact]\n")
	for _, a := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(a.key), q(a.tmpl))
	}

	b.WriteString("\n# With a sha256 the checksum is enforced, and a mismatch always refuses the install.\n")
	b.WriteString("# toy init computed these lines just now, so they describe only this batch of files —\n")
	b.WriteString("# recompute them after every release (run toy init --force again).\n")
	b.WriteString("[sha256]\n")
	for i := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(assets[i].key), q(assets[i].sum))
	}
	if strings.ContainsAny(name, "\"\\") {
		return nil, fmt.Errorf("the tool name %q contains a quote or a backslash, which is awkward as both a toml key and a value. Pass --name with a different one", name)
	}
	return []byte(b.String()), nil
}

// iniReport 明说它猜了什么、要作者去核对哪几处。
// 一个不说自己猜过的生成器，作者就会把猜出来的东西当成事实提交上去。
func iniReport(name string, assets []iniAsset, skipped []string, guessedName bool, prefix string) {
	ui.Info("")
	ui.Info("Recognized %d platform(s):", len(assets))
	for _, a := range assets {
		ui.Info("    %-22s ← %s", a.key, a.file)
	}
	for _, s := range skipped {
		ui.Warn("skipped %s", s)
	}

	ui.Info("")
	ui.Info("The following were guessed — check them before you commit:")
	ui.Info("    the path on the right of [bin] — fix it if the package has subdirectories (a bin/ and so on). " +
		"A single wrapper directory around the whole tree needs no fixing; that layer is stripped when unpacking " +
		"(`tar tzf` shows you which case you have)")
	if guessedName {
		hint := ""
		// 目录名和资产名前缀对不上时，后者多半才是真名（仓库目录常被改名、
		// clone 成别的名字，而资产名是构建脚本自己写出来的）。直接把命令给出来。
		if p := prefix; p != "" && p != name {
			hint = "; the asset names suggest " + p + ": `toy init --force --name " + p + "`"
		}
		ui.Info("    name = %s — taken from the directory name or the asset name prefix%s", name, hint)
	}
	ui.Info("    desc / license — left empty, fill them in")
	if len(assets) > 0 && !strings.Contains(assets[0].tmpl, "{version}") {
		// 没有 {version} 的映射每发一版都要手改，那正是这条命令要消灭的事。
		ui.Warn("No version was recognized in the asset names, so there is no {version} placeholder — " +
			"which means every release makes you come back and edit this file. Pass --version with the " +
			"version of this batch and run it again")
	}
}

// iniNothingRecognized 在一个都没认出来时报错。绝不写一个空壳 toy.toml ——
// 那会变成一个"看起来生成好了、实际谁也装不了"的文件。
func iniNothingRecognized(distDir string, files, skipped []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no file name in %s says which os/arch it is for, so [artifact] cannot be written", distDir)
	switch {
	case len(files) == 0:
		b.WriteString(" (there is not a single regular file in that directory)")
	default:
		fmt.Fprintf(&b, ". What it saw: %s", strings.Join(iniHead(files, iniMaxListed), ", "))
	}
	// 这里**不能**用 \n 分行：它最后是经 ui.Error 打出去的，而 ui 会把控制字符
	// （\n 也在内）转义成看得见的 \xNN —— 那是防有人用外来文本伪造一行
	// 「✓ 已签名」的护栏，不能为了排版把它松掉。所以全篇拿标点断句，和这个项目
	// 里其它报错一样是一长句。
	for _, s := range skipped {
		b.WriteString("; skipped " + s)
	}
	b.WriteString(". A file name has to carry both the operating system and the architecture, e.g. " +
		"tool_1.2.3_linux_amd64.tar.gz / tool-1.2.3-darwin-arm64.zip. " +
		"Recognized spellings: linux / darwin (macos, osx) / windows; " +
		"amd64 (x86_64, x64) / arm64 (aarch64) / armv7 (armhf) / armv6 / 386 (i386); " +
		"a musl or gnu in the name adds a libc segment to the key. " +
		"If none of that fits, write [artifact] by hand — one line per platform.")
	return fmt.Errorf("%s", b.String())
}

func iniHead(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("… (%d more)", len(s)-n))
}

// iniSum 算一个文件的 sha256。
func iniSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
