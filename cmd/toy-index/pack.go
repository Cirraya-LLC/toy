package main

// 从「一个仓库的发布事实」拼出一份内联 toyfile（规格 §5.1）。

import (
	"fmt"
	"strings"

	"github.com/Cirraya-LLC/toy/internal/archive"
	"github.com/Cirraya-LLC/toy/internal/assetname"
	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// descMax 是 desc 的长度上限（rune）。desc 是远端内容，会被原样写进索引 JSON
// 再打进 `toy search` 的结果里；一条几 KB 的描述能把整屏搜索结果顶掉。
const descMax = 160

// pack 是一个仓库认出来的全部结果。
type pack struct {
	owner, repo string
	info        repoInfo
	res         assetname.Result
	name        string // 工具名（= toy.toml 的 name，也是包目录名）
	binName     string // [bin] 左边那个命令名。默认同 name，--probe 探到真文件时改成它的基名
	binPath     string // [bin] 右边：解压后包内的相对路径
	binProbed   bool   // true = 解开真包看过的，false = 猜的
	bare        bool   // true = 资产是裸二进制，[bin] 只对一个平台成立
	notes       []string
}

// installable 报告这个资产 toy 真的装得动。
//
// 压缩包，加上**认得出平台的裸二进制**（`jq-linux-amd64` 这种 —— install 那边
// `archive.IsArchive` 为 false 时就是下载 + chmod +x，见 insUnpack 的 default
// 分支）。把裸二进制整个拒掉，jqlang/jq 这种只发单文件的仓库会被判成
// 「一个能装的资产都没有」，整个仓库进不了索引。
//
// 认不出平台的裸文件（checksums.txt、签名、sha256sum.txt）在这里就被挡住：
// 判据借 assetname.Classify，不在这里另抄一份词表。
//
// 唯一硬拒的是 **.tar.xz**：`internal/archive` 明确回 ErrUnsupported（要外部
// xz），放进去就是一条生成得出来、装不下去的条目。
func installable(name string) bool {
	l := strings.ToLower(name)
	if strings.HasSuffix(l, ".tar.xz") || strings.HasSuffix(l, ".txz") {
		return false
	}
	if archive.IsArchive(name) {
		return true
	}
	return assetname.Classify(name).Platform != ""
}

// allBareBinaries 报告这个包的资产【全都】是裸二进制（没有压缩后缀）。
// 只用来决定 [bin] 上面那句注释怎么写 —— 右边的路径两种形态都是 <名字>，
// 因为 install 把单文件落成 [bin] 指定的路径而不是资产名。
func allBareBinaries(res assetname.Result) bool {
	if len(res.Assets) == 0 {
		return false
	}
	for _, a := range res.Assets {
		if archive.IsArchive(a.Template) {
			return false
		}
	}
	return true
}

// makePack 把远端事实变成一个待渲染的包。第二个返回值非 nil 表示这个仓库产不出条目。
func makePack(owner, repo string, info repoInfo) (pack, error) {
	p := pack{owner: owner, repo: repo, info: info}

	var names []string
	for _, a := range info.Assets {
		if installable(a) {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return p, noUsableAssets(info)
	}

	// 版本推断和 `toy init` 走的是同一个函数（internal/assetname），这不是巧合：
	// 两份实现必然漂移，而漂移意味着「作者接入之后自动让位」那一刻用户装到的
	// 东西变了，且没有任何地方会说。
	//
	// 这里比 toy init 多知道一件事：release 的 tag。所以走 InferTag ——
	// tag 在资产名里真的出现时按字面替换，正则只在拿不到 tag 时才上场
	// （`15.2.0-aarch64` 会被正则整个当成版本号，见 assetname.preWords）。
	p.res = assetname.InferTag(names, info.Tag)
	if len(p.res.Assets) == 0 {
		return p, fmt.Errorf("none of the %d installable assets in the release has a recognisable os/arch "+
			"in its filename, so [artifact] cannot be written. Saw: %s", len(names), strings.Join(head(names, 6), ", "))
	}

	p.name, p.binPath = pickName(repo, p.res)
	p.binName = p.name
	// 裸二进制不再需要特判：install 把单文件落成 [bin] 右边那个路径而不是
	// 资产名（cmd/toy/install.go 的 insUnpack），所以 <名字> = "<名字>" 对每个
	// 平台都成立。以前照资产名写，除了恰好写对的那一个平台其余全是断掉的软链 ——
	// 索引里 mikefarah/yq 就是这么在 6 个平台里坏了 5 个。
	p.bare = allBareBinaries(p.res)
	for _, s := range p.res.Skipped {
		p.notes = append(p.notes, s.File+": "+s.Skip)
	}
	if v := p.res.Unreplaced; v != "" {
		// 模板里还留着一串版本号 = 这条被钉死在这一版：上游一发新版，装的人
		// 拿到的是一个 404 的下载地址。这条必须人来看。
		//
		// 判据是"还留着版本号"而不是"没有 {version}"：资产名本来就不带版本号的
		// 项目（eza、yq）跨版本天然稳定，对它们报警是误报，而一条每次都响的
		// 警告会把这一条真的淹掉。
		p.notes = append(p.notes, "the [artifact] template still spells out the version "+v+" instead of "+
			"{version} -- the next upstream release makes it point at a file that does not exist; fix it by hand")
	}
	return p, nil
}

// noUsableAssets 把「产不出条目」分成几种，各说各的人话。
// 一句笼统的「没有可用资产」会让人挨个去仓库页面上看到底是哪种。
func noUsableAssets(info repoInfo) error {
	if len(info.Assets) == 0 {
		return fmt.Errorf("the latest release (%s) has no assets at all -- most likely a source-only release. "+
			"GitHub's auto-attached source tarballs do not count, and toy never builds from source (rule 2)", info.Tag)
	}
	return fmt.Errorf("the latest release (%s) has %d assets and toy can install none of them. "+
		"Saw: %s. Archives and bare binaries with a recognisable platform are both accepted; the rest "+
		"(.tar.xz cannot be unpacked; checksums, signatures, .deb/.rpm and installers are not executables) "+
		"need a hand-written entry",
		info.Tag, len(info.Assets), strings.Join(head(info.Assets, 6), ", "))
}

// pickName 定工具名和 [bin] 右边的默认值。
//
// **资产名前缀优先于仓库名**：仓库叫 cli/cli 而二进制叫 gh 是常见情况，
// 而资产名是作者的构建脚本自己写出来的，比仓库名更接近真名。
func pickName(repo string, res assetname.Result) (name, binPath string) {
	name = repo
	if n := res.Name; cleanName(n) {
		name = n
	}
	// 右边是【解压之后】包内的相对路径。goreleaser 默认不套目录，所以「和命令
	// 同名的一个文件」是最好的猜测 —— 但它就是个猜测，--probe 才能定死。
	return name, name
}

// cleanName 判断这个名字当命令名和目录名都不别扭。真正的校验在 toyfile.ParseFrom
// （这份 toyfile 渲染完一定会过一遍），这里只是挑一个更像样的候选。
func cleanName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '+':
		default:
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// renderToyfile 拼出内联的 toy.toml 原文。
//
// **绝不写 [sha256]**：内联条目跟着上游最新的 tag 走（客户端用 git ls-remote 挑
// 最高 semver），而 [sha256] 钉的是某一个版本的字节。留着它，上游一发新版所有人
// 都会拿到「哈希对不上，拒绝安装」—— 读起来完全像投毒警告，而实际上只是作者正常
// 发了个版。首次安装仍有 TOFU 钉住（铁律 4 的后半句）。
// 这条刚在 cmd/toy/author.go 的 --inline 上修过一次，别在这里重新引入。
func renderToyfile(p pack) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by toy-index gen from the asset names of %s/%s's latest release (%s).\n",
		p.owner, p.repo, tomlSafe(p.info.Tag, 64))
	b.WriteString("# This is an INLINE packaging description (spec §5.1): the bytes come from the author's own\n")
	b.WriteString("# release, but which asset to install and what to name the command is the index talking --\n")
	b.WriteString("# so the client downgrades the trust level to index-toyfile.\n")
	b.WriteString("# There is deliberately no [sha256]: inline entries follow the upstream's newest tag, so\n")
	b.WriteString("# pinning one version's bytes would turn the next release into a checksum mismatch that reads\n")
	b.WriteString("# like a poisoning warning. First install is still pinned by trust on first use.\n")
	fmt.Fprintf(&b, "name    = %s\n", quote(p.name))
	fmt.Fprintf(&b, "desc    = %s\n", quote(tomlSafe(p.info.Desc, descMax)))
	if s := tomlSafe(p.info.License, 64); s != "" {
		fmt.Fprintf(&b, "license = %s\n", quote(s))
	}
	if s := tomlSafe(p.info.Homepage, 200); s != "" {
		fmt.Fprintf(&b, "homepage = %s\n", quote(s))
	}

	b.WriteString("\n[bin]\n")
	if p.bare {
		b.WriteString("# This repo ships bare binaries, not archives: install lands the file at the path on the\n")
		b.WriteString("# right, so this one line holds for every platform.\n")
	} else if p.binProbed {
		b.WriteString("# The path on the right was read out of a real archive that was unpacked and inspected.\n")
	} else {
		b.WriteString("# The path on the right is a GUESS: it is wrong if the archive has subdirectories (bin/ etc).\n")
		b.WriteString("# A single wrapping top-level directory does not count -- that layer is stripped on extract.\n")
		b.WriteString("# Re-run with --probe to settle it, or look for yourself with `tar tzf`.\n")
	}
	fmt.Fprintf(&b, "%s = %s\n", tomlKey(p.binName), quote(p.binPath))

	b.WriteString("\n[artifact]\n")
	for _, a := range p.res.Assets {
		fmt.Fprintf(&b, "%s = %s\n", tomlKey(a.Platform), quote(a.Template))
	}

	data := []byte(b.String())
	// 自己生成的东西也要过一遍真正的解析器，而且过的是【索引那一档】那个入口 ——
	// index.validEntry 用的就是它，坏条目在那里会被整条判掉。在这里就炸掉的话
	// 报错还指得出是哪个仓库；留到 Load 的时候只剩一句「跳过了 N 条坏记录」。
	if _, err := toyfile.ParseFrom(data, toyfile.OriginIndex); err != nil {
		return nil, fmt.Errorf("the generated toy.toml does not parse: %w", err)
	}
	return data, nil
}

// tomlSafe 清掉控制字符并按 rune 截断。这几个字段全是远端内容，会被原样打进
// 终端 —— 一串 "\r\x1b[32m✓ 已签名" 能把信任等级那一行盖掉重写（铁律 8）。
// toyfile.checkPrintable 会兜住漏网的，这里先清是为了不把一条本来能用的条目
// 因为描述里有个 \t 就整条判死。
func tomlSafe(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		if n >= max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// quote 拼一个 toml 基本字符串。**不能用 strconv.Quote**：Go 会把非打印字节写成
// \xNN，而 toml 根本没有 \x 这种转义 —— 生成出来的是一份语法错误的文件。
// 控制字符已经被 tomlSafe 清掉了，这里只剩两个字符要转义。
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// tomlKey：裸键只能是 [A-Za-z0-9_-]，别的一律加引号（平台 key 带斜杠，必须引）。
func tomlKey(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return quote(s)
		}
	}
	if s == "" {
		return quote(s)
	}
	return s
}

func head(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("... (%d more)", len(s)-n))
}
