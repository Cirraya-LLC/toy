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
	dist := fs.String("dist", "", "放 release 资产的目录（默认是项目目录下的 dist）")
	name := fs.String("name", "", "工具名，不给就从目录名或资产名推")
	version := fs.String("version", "", "这批资产的版本号，不给就从文件名里认")
	force := fs.Bool("force", false, "已经有 toy.toml 了也覆盖")
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
			return fmt.Errorf("算不出 %s 的 sha256：%w", assets[i].file, err)
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
		return fmt.Errorf("按 %s 里的文件名生成出来的 toy.toml 自己解析不了：%w\n"+
			"多半是名字里有 toml 认不了的字符，用 --name 指一个干净的名字再试", distDir, err)
	}

	out := filepath.Join(dir, "toy.toml")
	if _, err := os.Stat(out); err == nil && !*force {
		return fmt.Errorf("%s 已经存在了，不覆盖。要覆盖加 --force（先确认里面没有你手改过的东西）", out)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("写不出 %s：%w", out, err)
	}

	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		ui.Info("%s", line)
	}
	ui.Info("")
	ui.Info("已写出 %s", out)
	iniReport(pkg, assets, skipped, guessedName, res.Name)
	return nil
}

// iniListFiles 列出 dist 顶层的普通文件。子目录不进 —— goreleaser 把打好的包
// 放在顶层，子目录里是没打包的裸构建产物，收进来会和顶层的包重复。
func iniListFiles(distDir string) ([]string, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return nil, fmt.Errorf("读不了 %s：%w\n"+
			"toy init 是从已经打好的 release 资产的【文件名】反推映射的，"+
			"先把它们构建出来（goreleaser 默认就输出到 dist/），或者用 --dist 指别的目录", distDir, err)
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
		out[i] = a.File + "：" + a.Skip
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

	fmt.Fprintf(&b, "# 由 toy init 从资产文件名反推。装的时候一行脚本都不跑，所以这里没有 install/build 这类键。\n")
	fmt.Fprintf(&b, "# 打个新 tag 就是发新版：{version} 会被换成那个 tag（不带 v），这个文件一个字都不用改。\n")
	fmt.Fprintf(&b, "name    = %s\n", q(name))
	fmt.Fprintf(&b, "desc    = \"\"  # ← 一句话说清楚它是干嘛的，搜索结果里显示的就是这句\n")
	fmt.Fprintf(&b, "license = \"\"  # ← 例如 \"MIT\"\n")
	b.WriteString("\n[bin]\n")
	b.WriteString("# 左边 = 装完之后 PATH 上的命令名，右边 = 【解压之后】包内的相对路径。\n")
	b.WriteString("# 整棵树套在一层目录里（cargo-dist 那种 " + name + "-1.2.3-x86_64-.../）时那一层会被自动剥掉，\n")
	b.WriteString("# 所以右边不要带它；真的分了子目录才要写上，例如 \"bin/" + name + "\"。\n")
	b.WriteString("# 用 `tar tzf dist/<某个包>` 看一眼里面到底长什么样。\n")
	fmt.Fprintf(&b, "%s = %s\n", key(name), q(name))

	b.WriteString("\n[artifact]\n")
	for _, a := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(a.key), q(a.tmpl))
	}

	b.WriteString("\n# 有 sha256 就强制校验，对不上一律拒绝安装。这几行是 toy init 现算的，\n")
	b.WriteString("# 所以它们只对上面这一批文件成立 —— 发新版之后要重新算（再跑一次 toy init --force）。\n")
	b.WriteString("[sha256]\n")
	for i := range assets {
		fmt.Fprintf(&b, "%s = %s\n", key(assets[i].key), q(assets[i].sum))
	}
	if strings.ContainsAny(name, "\"\\") {
		return nil, fmt.Errorf("工具名 %q 里有引号或反斜杠，当 toml 的键和值都很别扭。用 --name 换一个", name)
	}
	return []byte(b.String()), nil
}

// iniReport 明说它猜了什么、要作者去核对哪几处。
// 一个不说自己猜过的生成器，作者就会把猜出来的东西当成事实提交上去。
func iniReport(name string, assets []iniAsset, skipped []string, guessedName bool, prefix string) {
	ui.Info("")
	ui.Info("认出了 %d 个平台：", len(assets))
	for _, a := range assets {
		ui.Info("    %-22s ← %s", a.key, a.file)
	}
	for _, s := range skipped {
		ui.Warn("跳过 %s", s)
	}

	ui.Info("")
	ui.Info("下面这几处是猜的，提交之前核对一遍：")
	ui.Info("    [bin] 右边的包内路径 —— 包里分了子目录（bin/ 之类）就得改；" +
		"整棵树套在一层目录里的那种不用管，那一层解压时会被剥掉（`tar tzf` 看一眼）")
	if guessedName {
		hint := ""
		// 目录名和资产名前缀对不上时，后者多半才是真名（仓库目录常被改名、
		// clone 成别的名字，而资产名是构建脚本自己写出来的）。直接把命令给出来。
		if p := prefix; p != "" && p != name {
			hint = "；资产名看起来是 " + p + "：`toy init --force --name " + p + "`"
		}
		ui.Info("    name = %s —— 从目录名或资产名前缀推的%s", name, hint)
	}
	ui.Info("    desc / license —— 空着的，填上")
	if len(assets) > 0 && !strings.Contains(assets[0].tmpl, "{version}") {
		// 没有 {version} 的映射每发一版都要手改，那正是这条命令要消灭的事。
		ui.Warn("资产名里没认出版本号，所以没有 {version} 占位符 —— " +
			"这样每次发版都得回来改这个文件。用 --version 指一下这批资产的版本号再跑一次")
	}
}

// iniNothingRecognized 在一个都没认出来时报错。绝不写一个空壳 toy.toml ——
// 那会变成一个"看起来生成好了、实际谁也装不了"的文件。
func iniNothingRecognized(distDir string, files, skipped []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s 里没有一个文件名能认出 os/arch，写不出 [artifact]", distDir)
	switch {
	case len(files) == 0:
		b.WriteString("（这个目录里一个普通文件都没有）")
	default:
		fmt.Fprintf(&b, "。看到的是：%s", strings.Join(iniHead(files, iniMaxListed), "、"))
	}
	// 这里**不能**用 \n 分行：它最后是经 ui.Error 打出去的，而 ui 会把控制字符
	// （\n 也在内）转义成看得见的 \xNN —— 那是防有人用外来文本伪造一行
	// 「✓ 已签名」的护栏，不能为了排版把它松掉。所以全篇拿标点断句，和这个项目
	// 里其它报错一样是一长句。
	for _, s := range skipped {
		b.WriteString("；跳过 " + s)
	}
	b.WriteString("。文件名里要同时出现操作系统和架构，例如 " +
		"tool_1.2.3_linux_amd64.tar.gz / tool-1.2.3-darwin-arm64.zip。" +
		"认这些写法：linux / darwin(macos, osx) / windows；" +
		"amd64(x86_64, x64) / arm64(aarch64) / armv7(armhf) / armv6 / 386(i386)；" +
		"名字里带 musl 或 gnu 会多出一段 libc key。" +
		"实在对不上就自己手写 [artifact]，一个平台一行，格式见 `toy info` 里任何一个包。")
	return fmt.Errorf("%s", b.String())
}

func iniHead(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(slices.Clone(s[:n]), fmt.Sprintf("…（还有 %d 个）", len(s)-n))
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
