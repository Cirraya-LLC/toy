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
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
	"github.com/Cirraya-LLC/toy/internal/ui"
)

// iniMaxListed 是报错时最多列几个看到的文件名 —— 一个塞了两百个文件的 dist
// 不该把终端刷满。
const iniMaxListed = 12

// iniOS / iniArch 是文件名里那些词到 GOOS/GOARCH 的映射。大小写不敏感，
// 切词用 - _ .（所以 x86_64 会裂成两段，靠下面的相邻二元组捞回来）。
var iniOS = map[string]string{
	"linux":   "linux",
	"darwin":  "darwin",
	"macos":   "darwin",
	"osx":     "darwin",
	"apple":   "darwin",
	"windows": "windows",
	"win":     "windows",
}

var iniArch = map[string]string{
	"amd64": "amd64", "x86_64": "amd64", "x64": "amd64",
	"arm64": "arm64", "aarch64": "arm64",
	"armv7": "armv7", "armhf": "armv7",
	"armv6": "armv6",
	// 裸 "arm" 必须在这里：GOARCH=arm 且没设 goarm 时，goreleaser 的默认
	// name_template 产出的就是 `tool_1.2.3_linux_arm.tar.gz`。少了这一行，
	// 树莓派那份构建会被【静默丢掉】—— 而 linux/arm 正是 platform.go 给
	// armv6/armv7 兜底的那个候选（规格 §3.1），丢了就等于 Pi 用户装不上。
	"arm": "arm",
	"386": "386", "i386": "386",
}

// iniVerRe 认文件名里的版本段。只认三段式：两段式（1.2）在文件名里和
// 别的数字对分不开，认错的下场是把不该动的字符串换成 {version}。
var iniVerRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?`)

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

	assets, skipped := iniScan(files, strings.TrimPrefix(strings.TrimSpace(*version), "v"))
	if len(assets) == 0 {
		return iniNothingRecognized(distDir, files, skipped)
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

	pkg, guessedName := iniName(*name, dir, assets)
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
	iniReport(pkg, assets, skipped, guessedName)
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

// iniScan 把文件名列表变成资产映射。第二个返回值是「认出来了但用不了」的说明，
// 现在只有 Windows 一种（规格 §11：v1 不做 Windows）。
//
// 同一个 key 有多个文件时留排序最前的那一个（.tar.gz 和 .zip 同时发的很常见），
// 并把这件事记进 skipped 让作者自己挑。
func iniScan(files []string, forceVersion string) (assets []iniAsset, skipped []string) {
	type hit struct {
		file, key string
	}
	var hits []hit
	for _, f := range files {
		key, goos := iniDetect(f)
		if goos == "windows" {
			skipped = append(skipped, f+"：认出来是 Windows，但 toy v1 不支持 Windows（规格 §11），跳过")
			continue
		}
		if key == "" {
			// 认不出来的【资产】要说一声。静默丢掉的下场是：作者发了六个平台，
			// toy.toml 里只有四个，而 init 打印的「认出了 N 个平台」看起来一切正常，
			// 症状要等到那个平台的用户来报「没有适配当前平台的构建」才出现。
			// 只对像发布包的文件出声 —— checksums.txt / LICENSE 这些本来就不是资产，
			// 为它们报一行会把真正要看的那条淹掉。
			if iniLooksLikeArchive(f) {
				skipped = append(skipped, f+"：认不出是哪个平台的（os 或 arch 没认出来），跳过。"+
					"确实是个发布资产的话，自己往 [artifact] 里补一行")
			}
			continue
		}
		hits = append(hits, hit{f, key})
	}
	if len(hits) == 0 {
		return nil, skipped
	}

	ver := forceVersion
	if ver == "" {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.file
		}
		ver = iniVersion(names)
	}

	seen := map[string]string{}
	for _, h := range hits {
		if first, dup := seen[h.key]; dup {
			skipped = append(skipped, h.file+"：和 "+first+" 同属 "+h.key+
				"，只能留一个，留了排前面的那个。要换就自己改 [artifact] 里那一行")
			continue
		}
		seen[h.key] = h.file
		tmpl := h.file
		if ver != "" {
			tmpl = strings.ReplaceAll(tmpl, ver, "{version}")
		}
		assets = append(assets, iniAsset{file: h.file, key: h.key, tmpl: tmpl})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].key < assets[j].key })
	return assets, skipped
}

// iniLooksLikeArchive 判断一个文件像不像发布资产，只用来决定「认不出来时要不要
// 出声」。宁可漏报不可错报：checksums.txt、LICENSE、README.md 都会落在 dist 里，
// 为它们各报一行警告，等于把真正需要作者去看的那一条淹掉。
func iniLooksLikeArchive(f string) bool {
	l := strings.ToLower(f)
	for _, ext := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2", ".tar.zst", ".zip"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// iniDetect 从一个文件名里认出平台 key。认不出返回空 key；
// 第二个返回值是认出来的 GOOS（Windows 要单独说明才跳过，不能静默）。
func iniDetect(file string) (key, goos string) {
	toks := iniTokens(file)
	var arch, libc string
	for i, tok := range toks {
		if v, ok := iniOS[tok]; ok && goos == "" {
			goos = v
		}
		if v, ok := iniArch[tok]; ok && arch == "" {
			arch = v
		}
		// x86_64 被 '_' 切成了两段，用相邻二元组捞回来。
		if i+1 < len(toks) {
			if v, ok := iniArch[tok+"_"+toks[i+1]]; ok && arch == "" {
				arch = v
			}
		}
		switch tok {
		case "musl", "gnu":
			libc = tok
		}
	}
	if goos == "" || arch == "" {
		return "", goos
	}
	if goos == "windows" {
		return "", goos
	}
	switch {
	// armv6/armv7 本身就是代次，再挂一层 libc 没有意义（也没人这么发布）。
	case strings.HasPrefix(arch, "armv"):
		return goos + "/" + arch, goos
	case goos == "linux" && libc != "":
		return goos + "/" + arch + "/" + libc, goos
	default:
		return goos + "/" + arch, goos
	}
}

func iniTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
}

// iniVersion 挑出现次数最多的那个版本串。goreleaser 一批资产的版本是同一个，
// 所以"最多的那个"就是它；平手时取字典序最小的，好让同样的 dist 每次跑出同样的结果。
func iniVersion(files []string) string {
	count := map[string]int{}
	for _, f := range files {
		// 一个文件名里同一个版本可能出现两次（目录前缀 + 包名），只算一次。
		for _, v := range slices.Compact(slices.Sorted(slices.Values(iniVerRe.FindAllString(f, -1)))) {
			count[v]++
		}
	}
	best := ""
	for v, n := range count {
		if best == "" || n > count[best] || (n == count[best] && v < best) {
			best = v
		}
	}
	return best
}

// iniName 定工具名：--name > 项目目录名 > 资产名的公共前缀。
// 第二个返回值说的是"这个名字是猜出来的"，作者得核对。
func iniName(flagName, dir string, assets []iniAsset) (name string, guessed bool) {
	if n := strings.TrimSpace(flagName); n != "" {
		return n, false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if base := filepath.Base(abs); base != "." && base != string(filepath.Separator) && base != "" {
			return base, true
		}
	}
	return iniCommonPrefix(assets), true
}

// iniCommonPrefix 从资产名里抠出工具名：取公共前缀，砍掉版本号那一段往后的部分，
// 再削掉尾巴上的分隔符。`hello_1.2.0_linux_amd64.tar.gz` 这一批得到 `hello`。
func iniCommonPrefix(assets []iniAsset) string {
	if len(assets) == 0 {
		return ""
	}
	p := assets[0].file
	for _, a := range assets[1:] {
		for !strings.HasPrefix(a.file, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	if loc := iniVerRe.FindStringIndex(p); loc != nil {
		p = p[:loc[0]]
	}
	return strings.TrimRight(p, "-_.")
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
	b.WriteString("# ← 压缩包里套了一层目录的话右边要改，例如 \"" + name + "_1.2.3_linux_amd64/" + name + "\"。\n")
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
func iniReport(name string, assets []iniAsset, skipped []string, guessedName bool) {
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
	ui.Info("    [bin] 右边的包内路径 —— 压缩包里套了目录就得改（`tar tzf` 看一眼）")
	if guessedName {
		hint := ""
		// 目录名和资产名前缀对不上时，后者多半才是真名（仓库目录常被改名、
		// clone 成别的名字，而资产名是构建脚本自己写出来的）。直接把命令给出来。
		if p := iniCommonPrefix(assets); p != "" && p != name {
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
