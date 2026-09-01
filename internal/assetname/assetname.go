// Package assetname 从 release 资产的文件名反推平台 key 和版本号。
//
// 抽出来是因为有两个调用方要用【一模一样】的推断：`toy init` 认本地 dist/ 里的
// 文件名，批量索引生成器认从仓库读来的资产名列表。两份实现必然漂移，而漂移的
// 症状特别恶心 —— 索引里生成的条目和作者自己跑 toy init 得到的不一样，于是
// "作者接入之后自动让位"那一刻用户装到的东西变了，而没有任何地方会说。
//
// 这个包是纯函数：不碰磁盘、不碰网络、不 exec 任何东西。两个调用方唯一的区别
// 就是"文件名从哪来"。
package assetname

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// osWords / archWords 是文件名里那些词到 GOOS/GOARCH 的映射。大小写不敏感，
// 切词用 - _ .（所以 x86_64 会裂成两段，靠下面的相邻二元组捞回来）。
var osWords = map[string]string{
	"linux":   "linux",
	"darwin":  "darwin",
	"macos":   "darwin",
	"osx":     "darwin",
	"apple":   "darwin",
	"windows": "windows",
	"win":     "windows",
}

var archWords = map[string]string{
	"amd64": "amd64", "x86_64": "amd64", "x64": "amd64",
	"arm64": "arm64", "aarch64": "arm64",
	"armv7": "armv7", "armhf": "armv7",
	"armv6": "armv6",
	// 裸 "arm" 必须在这里：GOARCH=arm 且没设 goarm 时，goreleaser 的默认
	// name_template 产出的就是 `tool_1.2.3_linux_arm.tar.gz`。少了这一行，
	// 树莓派那份构建会被【静默丢掉】—— 而 linux/arm 正是 platform.go 给
	// armv6/armv7 兜底的那个候选（规格 §3.1），丢了就等于 Pi 用户装不上。
	"arm": "arm",
	// i686 是 32 位 Linux 构建里最常见的写法（ripgrep / fd / zoxide 的
	// `i686-unknown-linux-gnu` 都是它），少这一行那份构建会被静默丢掉。
	"386": "386", "i386": "386", "i686": "386",
}

// foreignWords 是「这个词一出现，这个资产就不是给我们支持的系统的」。词 → 系统名。
//
// android 必须单独一张表、而且要【压过】已经认出来的 linux：Rust 的 Android triple
// 长成 `aarch64-linux-android` / `armv7-linux-androideabi`，切完词之后 `linux` 排在
// `android` 前面，osWords 先到先得就把它认成 linux/arm64、linux/armv7。
// 那是一个 bionic 构建，它会**抢走真正 Linux 构建的那个 key**（zoxide 真的这么
// 发布：armv7 那个 key 上本该是 armv7-unknown-linux-musleabihf），而症状是
// 下载地址 200、字节完好、跑起来 `Exec format error` —— 比 404 难查得多。
//
// 几个 BSD 和 illumos/solaris 在这里【不是】为了压过什么（它们的 triple 里没有
// linux），而是为了归类准确：少了这几行它们只会落进"认不出是哪个平台的"，
// 而那句话是在请作者去补一行 [artifact] —— 对一个 toy v1 根本不支持的系统
// （规格 §11），那是一条把人往沟里带的建议。
var foreignWords = map[string]string{
	"android": "android", "androideabi": "android",
	"freebsd": "freebsd", "openbsd": "openbsd", "netbsd": "netbsd",
	"solaris": "solaris", "illumos": "illumos",
}

// verRe 认文件名里的版本段。只认三段式：两段式（1.2）在文件名里和别的数字对
// 分不开，认错的下场是把不该动的字符串换成 {version}。
//
// `[-+]` 后面那一段是【候选】预发布标签，不是结论 —— 见 preWords。
var verRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?`)

// preWords 是真的预发布标签才会用的词。
//
// 这是这个包里最贵的一条：`ripgrep-15.2.0-aarch64-apple-darwin.tar.gz` 里
// `-aarch64` 长得和 `-rc.1` 一模一样，于是正则把 `15.2.0-aarch64` 整个当成
// 版本号，替换之后模板变成 `ripgrep-{version}-apple-darwin.tar.gz` ——
// **架构名不见了，拼出来的地址每个用户都 404**。而我们自己的合成测试数据
// （`tool_1.2.0_linux_amd64.tar.gz`，版本后面紧跟 `_`）永远碰不到这个形状。
//
// 判据用白名单而不是"排除掉 os/arch 词"：世界上的架构名太多
// （aarch64 / riscv64gc / s390x / loong64 / i686 / armv7 / x86…），漏一个就是
// 一个静默换错版本号的资产名；预发布标签的词汇量小得多，而且认错的方向是
// 安全的 —— 把真的预发布标签当成平台词，最多是少替一小段，模板仍然是对的。
var preWords = map[string]bool{
	"alpha": true, "beta": true, "rc": true, "pre": true, "preview": true,
	"dev": true, "devel": true, "next": true, "canary": true,
	"nightly": true, "snapshot": true, "unstable": true, "edge": true,
}

// trimFalsePre 把 `15.2.0-aarch64` 削回 `15.2.0`，但让 `1.0.0-rc.1` 原样留着。
//
// 只管 '-'：`1.2.3+20240101` 那个构建元数据是 tag 的一部分，install 那边
// `{version}` 展开的就是整串（semver.V.Number 只剥 'v'），削掉会让模板对不上。
func trimFalsePre(v string) string {
	i := strings.IndexByte(v, '-')
	if i < 0 {
		return v
	}
	word := v[i+1:]
	if j := strings.IndexByte(word, '.'); j >= 0 {
		word = word[:j]
	}
	// rc1 / beta2 这种连写的也要认出来。
	word = strings.TrimRight(strings.ToLower(word), "0123456789")
	if preWords[word] {
		return v
	}
	return v[:i]
}

// findVersions 是 verRe 的唯一出口：正则的每个命中都要过一遍 trimFalsePre。
func findVersions(s string) []string {
	all := verRe.FindAllString(s, -1)
	for i, v := range all {
		all[i] = trimFalsePre(v)
	}
	return all
}

// companionSuffixes 是「和真资产同名同前缀、但 toy 永远装不了」的那些东西：
// 校验和、签名、发行版包、各家安装器、证明文件。
//
// 它们必须在认平台【之前】就被挑掉：`ripgrep-15.2.0-x86_64-apple-darwin.tar.gz.sha256`
// 和 `bat-musl_0.26.1_musl-linux-amd64.deb` 里该有的 os/arch 词一个不少，认出来
// 之后会去和真资产抢同一个平台 key —— 排在前面的那个赢，而 .deb 常常排在前面。
// 就算没抢赢，它们也会各报一行「认不出/重复」，把真正值得看的那条淹掉（§14.10）。
var companionSuffixes = []string{
	".sha256", ".sha512", ".sha1", ".md5", ".sum", ".sig", ".asc", ".pem",
	".sbom", ".spdx", ".intoto", ".json", ".txt", ".sha256sum",
	".deb", ".rpm", ".apk", ".pkg", ".msi", ".dmg", ".snap", ".appimage",
}

// foreignOS 报告 detect 返回的这个 goos 是不是外来系统。android 由 Classify 单独
// 出文案（它要解释"名字里带 linux"那件事），所以这里排掉它。
func foreignOS(goos string) bool {
	if goos == "" || goos == "android" {
		return false
	}
	for _, v := range foreignWords {
		if v == goos {
			return true
		}
	}
	return false
}

func looksLikeCompanion(f string) bool {
	l := strings.ToLower(f)
	for _, ext := range companionSuffixes {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// 跳过的原因。文案是给人看的，会被原样打进 toy init 的输出里。
const (
	SkipWindows      = "认出来是 Windows，但 toy v1 不支持 Windows（规格 §11），跳过"
	SkipUnrecognized = "认不出是哪个平台的（os 或 arch 没认出来），跳过。" +
		"确实是个发布资产的话，自己往 [artifact] 里补一行"
	SkipNotAnAsset = "不像发布资产（后缀不是压缩包），跳过"
	SkipAndroid    = "认出来是 Android（bionic）构建 —— 名字里带 linux，但它在 Linux 机器上跑不起来，跳过"
)

// SkipForeignOS 是「认出来了，但那不是 toy 支持的系统」。和 SkipUnrecognized
// 分开：后者是在请作者手写一行 [artifact]，而这些系统写了也装不上。
func SkipForeignOS(goos string) string {
	return "认出来是 " + goos + " 构建，toy v1 只支持 Linux 和 macOS（规格 §11），跳过"
}

// Asset 是一个 release 资产被认出来的样子。
type Asset struct {
	File     string // 原始文件名
	Platform string // "linux/amd64" / "linux/amd64/musl" / "linux/armv7"，认不出为空
	Version  string // 从这个文件名里认出的版本串，认不出为空
	Template string // 版本号换成 {version} 之后的资产名，只有 Infer 会填
	Skip     string // 非空表示该跳过，内容是给人看的原因
	// Notable 表示这条跳过值得告诉作者。checksums.txt / LICENSE 这些本来就不是
	// 资产，为它们各报一行会把真正要看的那条（比如认不出的 freebsd_riscv64）淹掉。
	Notable bool
}

// Classify 认一个文件名。纯函数，不碰磁盘也不碰网络。
func Classify(name string) Asset {
	a := Asset{File: name, Version: firstVersion(name)}
	if looksLikeCompanion(name) {
		// 静默：这些东西每个真资产旁边都躺着一个，出声就是把真的那条淹掉。
		a.Skip = SkipNotAnAsset
		return a
	}
	key, goos := detect(name)
	switch {
	case goos == "windows":
		a.Skip, a.Notable = SkipWindows, true
	case goos == "android":
		a.Skip, a.Notable = SkipAndroid, true
	case foreignOS(goos):
		a.Skip, a.Notable = SkipForeignOS(goos), true
	case key == "" && looksLikeArchive(name):
		a.Skip, a.Notable = SkipUnrecognized, true
	case key == "":
		a.Skip = SkipNotAnAsset
	default:
		a.Platform = key
	}
	return a
}

// Result 是认一批文件名的结果。
type Result struct {
	// Assets 是认出来的资产，按平台 key 排序，Template 已经填好。
	Assets []Asset
	// Artifact 是 toy.toml 里 [artifact] 那一段：平台 key → 资产名模板。
	Artifact map[string]string
	// Version 是这批资产的版本串（forceVersion 或从文件名里认出来的）。
	Version string
	// Skipped 只收值得告诉作者的那些（Notable）。
	Skipped []Asset
	// Name 是从资产名公共前缀推出来的包名，推不出为空。
	Name string
	// Unreplaced 非空 = 生成出来的模板里【还留着】一串版本号，没能换成
	// {version}。这种条目被钉死在这一版：上游一发新版，装的人拿到的是一个
	// 404 的地址。
	//
	// 判据是"模板里还有版本号"，不是"模板里没有 {version}"：资产名本来就不带
	// 版本号（`eza_x86_64-unknown-linux-gnu.tar.gz`、`yq_linux_amd64.tar.gz`）
	// 的项目一大把，那种模板跨版本天然稳定，对它们报警是误报 —— 而一条每次
	// 都响的警告等于没有警告。
	Unreplaced string
}

// InferTag 和 Infer 一样，但多知道一件事：这批资产是哪个 tag 发出来的
// （批量索引生成器从托管方 API 拿得到，`toy init` 拿不到）。
//
// tag 在资产名里真的出现时，**它说了算**：正则只能猜，而
// `ripgrep-15.2.0-aarch64-apple-darwin.tar.gz` 里 `-aarch64` 和 `-rc.1` 长得
// 一模一样（见 preWords）。已知答案的时候不该去猜。
//
// 替换用的一定是 `TrimPrefix(tag, "v")` 而不是别的什么变体 —— 那正是 install
// 那边 {version} 会展开成的东西（semver.V.Number）。拿 `jq-1.8.2` 这种带包名
// 的 tag 去替换，展开回来就少了 `jq-` 那一截。
func InferTag(files []string, tag string) Result {
	v := tagVersion(tag)
	if v != "" && anyContainsVersion(files, v) {
		return Infer(files, v)
	}
	res := Infer(files, "")
	if res.Version == "" && v != "" {
		// 资产名里没有版本号（cargo-dist 那类）。替换不了任何东西，但版本号
		// 本身仍然要记下来 —— probe 那边拿它判 [bin] 路径稳不稳。
		res = Infer(files, v)
	}
	return res
}

// tagVersion 把 tag 变成"能安全地在资产名里做字面替换"的那一串，不安全就返回空。
//
// 两道闸：必须以数字开头（`jq-1.8.2` 这种带包名前缀的 tag 展开回来对不上），
// 必须含一个点（tag 是裸的 `2` 时，替换会咬进 `tool-2.0.1-…` 的中间）。
func tagVersion(tag string) string {
	v := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if v == "" || v[0] < '0' || v[0] > '9' || !strings.Contains(v, ".") {
		return ""
	}
	return v
}

// anyContainsVersion 报告 v 是不是【作为一个完整的数字段】出现在某个文件名里。
// 边界检查不能省：tag `2.0` 会在 `tool-2.0.1-linux.tar.gz` 里命中，
// 替换出来的 `tool-{version}.1-linux.tar.gz` 是一条谁都装不上的模板。
func anyContainsVersion(files []string, v string) bool {
	for _, f := range files {
		for i := 0; i+len(v) <= len(f); i++ {
			if f[i:i+len(v)] != v {
				continue
			}
			if i > 0 && isVerByte(f[i-1]) {
				continue
			}
			if j := i + len(v); j < len(f) && isVerByte(f[j]) {
				continue
			}
			return true
		}
	}
	return false
}

func isVerByte(c byte) bool { return c == '.' || (c >= '0' && c <= '9') }

// Infer 认一批文件名。forceVersion 非空时它说了算 —— 文件名里认不出三段式版本
// （2024.09 这种）时那是唯一的出路。
//
// 同一个平台有多个文件时留排序最前的那一个（.tar.gz 和 .zip 同时发的很常见），
// 并把这件事记进 Skipped 让作者自己挑。
func Infer(files []string, forceVersion string) Result {
	res := Result{Artifact: map[string]string{}}

	var hits []Asset
	for _, f := range files {
		a := Classify(f)
		if a.Skip != "" {
			if a.Notable {
				res.Skipped = append(res.Skipped, a)
			}
			continue
		}
		hits = append(hits, a)
	}
	if len(hits) == 0 {
		return res
	}

	res.Version = strings.TrimPrefix(strings.TrimSpace(forceVersion), "v")
	if res.Version == "" {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.File
		}
		res.Version = commonVersion(names)
	}

	seen := map[string]string{}
	for _, h := range hits {
		if first, dup := seen[h.Platform]; dup {
			h.Skip = "和 " + first + " 同属 " + h.Platform +
				"，只能留一个，留了排前面的那个。要换就自己改 [artifact] 里那一行"
			h.Notable = true
			h.Platform = ""
			res.Skipped = append(res.Skipped, h)
			continue
		}
		seen[h.Platform] = h.File
		// 版本号不换成 {version}，作者每次发版都要回来改 toy.toml ——
		// 而"打个 tag 就是发新版"是这个产品的卖点。
		h.Template = h.File
		if res.Version != "" {
			h.Template = strings.ReplaceAll(h.Template, res.Version, "{version}")
		}
		res.Assets = append(res.Assets, h)
		res.Artifact[h.Platform] = h.Template
	}
	sort.Slice(res.Assets, func(i, j int) bool { return res.Assets[i].Platform < res.Assets[j].Platform })
	res.Name = commonPrefix(res.Assets)
	for _, a := range res.Assets {
		if left := findVersions(a.Template); len(left) > 0 {
			res.Unreplaced = left[0]
			break
		}
	}
	return res
}

// looksLikeArchive 判断一个文件像不像发布资产，只用来决定「认不出来时要不要
// 出声」。宁可漏报不可错报：checksums.txt、LICENSE、README.md 都会落在 dist 里，
// 为它们各报一行警告，等于把真正需要作者去看的那一条淹掉。
func looksLikeArchive(f string) bool {
	l := strings.ToLower(f)
	for _, ext := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2", ".tar.zst", ".zip"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// detect 从一个文件名里认出平台 key。认不出返回空 key；第二个返回值是认出来的
// GOOS（Windows 要单独说明才跳过，不能静默）。
//
// Rust 的 target triple（x86_64-unknown-linux-gnu / aarch64-apple-darwin）不用
// 特判：切完词之后每一段该有的词都在，unknown / pc 这类多余的段本来就不认。
func detect(file string) (key, goos string) {
	toks := tokens(file)
	var arch, libc string
	for i, tok := range toks {
		// 先判外来系统：android 要压过同一个文件名里已经认出来的 linux，而不是排队。
		if v, ok := foreignWords[tok]; ok {
			return "", v
		}
		if v, ok := osWords[tok]; ok && goos == "" {
			goos = v
		}
		if v, ok := archWords[tok]; ok && arch == "" {
			arch = v
		}
		// x86_64 被 '_' 切成了两段，用相邻二元组捞回来。
		if i+1 < len(toks) {
			if v, ok := archWords[tok+"_"+toks[i+1]]; ok && arch == "" {
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

func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
}

// firstVersion 取这个文件名里出现的第一个版本串。
func firstVersion(file string) string {
	if all := findVersions(file); len(all) > 0 {
		return all[0]
	}
	return ""
}

// commonVersion 挑出现次数最多的那个版本串。goreleaser 一批资产的版本是同一个，
// 所以"最多的那个"就是它；平手时取字典序最小的，好让同一批文件名每次跑出同样的结果。
func commonVersion(files []string) string {
	count := map[string]int{}
	for _, f := range files {
		// 一个文件名里同一个版本可能出现两次（目录前缀 + 包名），只算一次。
		for _, v := range slices.Compact(slices.Sorted(slices.Values(findVersions(f)))) {
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

// commonPrefix 从资产名里抠出工具名：取公共前缀，砍掉版本号那一段往后的部分，
// 再削掉尾巴上的分隔符。`hello_1.2.0_linux_amd64.tar.gz` 这一批得到 `hello`。
func commonPrefix(assets []Asset) string {
	if len(assets) == 0 {
		return ""
	}
	p := assets[0].File
	for _, a := range assets[1:] {
		for !strings.HasPrefix(a.File, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	if loc := verRe.FindStringIndex(p); loc != nil {
		p = p[:loc[0]]
	}
	p = strings.TrimRight(p, "-_.")
	// `fd-v10.5.0-x86_64-…` 这种 `-v` 连写的，砍掉版本号之后前缀是 `fd-v`。
	// 那个 v 是 tag 的前缀不是名字的一部分 —— 留着就会拿 `fd-v` 当命令名。
	if n := len(p); n > 1 && (p[n-1] == 'v' || p[n-1] == 'V') && strings.ContainsRune("-_.", rune(p[n-2])) {
		p = strings.TrimRight(p[:n-1], "-_.")
	}
	return p
}
