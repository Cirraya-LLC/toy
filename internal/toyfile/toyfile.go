// Package toyfile 解析并校验作者写的 toy.toml。规格 §3。
package toyfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	ErrNoArtifact   = errors.New("toyfile: 这个仓库没有发布二进制")
	ErrBothForms    = errors.New("toyfile: [artifact] 和 [script] 只能有一个")
	ErrNoPlatform   = errors.New("toyfile: 没有适配当前平台的构建")
	ErrUnsafeTarget = errors.New("toyfile: bin 目标路径不安全")
)

type Toyfile struct {
	Name     string            `toml:"name"`
	Desc     string            `toml:"desc"`
	License  string            `toml:"license"`
	Homepage string            `toml:"homepage"`
	Bin      map[string]string `toml:"bin"`      // PATH 上的名字 -> 包内相对路径
	Artifact map[string]string `toml:"artifact"` // "linux/amd64" -> 资产名模板
	SHA256   map[string]string `toml:"sha256"`   // "linux/amd64" -> hex，可选但写了就强制
	Script   map[string]string `toml:"script"`   // 单文件形态：PATH 名字 -> 资产名模板
}

// Parse 解析并校验。校验必须拒绝：
//   - artifact 和 script 同时存在或都不存在
//   - name 为空或含路径分隔符
//   - bin/script 的 key 含 '/'，或 value 是绝对路径 / 含 ".."（铁律 5、7）
//   - sha256 的值不是 64 位 hex
//
// 报错要说人话，并告诉作者该怎么改。
func Parse(data []byte) (*Toyfile, error) {
	var t Toyfile
	md, err := toml.Decode(string(data), &t)
	if err != nil {
		return nil, fmt.Errorf("toyfile: toy.toml 语法有问题：%w", err)
	}
	// 认不出来的键一律拒，不能静默忽略：把 [sha256] 打成 [sha265] 的后果是
	// 「作者以为写了 hash、客户端从头到尾没校验过」，正是铁律 4 要防的那件事。
	// 顺带也拦住 install/postinstall/hook 这类键 —— toy 不执行包里的任何代码（铁律 1），
	// 静默忽略会让作者以为它们生效了。
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("toyfile: toy.toml 里有 toy 不认识的键：%s。"+
			"只有 name / desc / license / homepage 和 [bin] [artifact] [sha256] [script] 这几项有效，"+
			"多出来的键不会生效 —— 而 [sha256] 少写一个字母就等于完全没有校验，所以这里直接拒绝。"+
			"另外 toy 从不执行包里的代码，install / build / hook 这类键在 toy.toml 里不存在",
			describeKeys(un))
	}

	if err := checkName(t.Name); err != nil {
		return nil, err
	}
	for what, val := range map[string]string{"desc": t.Desc, "license": t.License, "homepage": t.Homepage} {
		if err := checkPrintable(what, val); err != nil {
			return nil, err
		}
	}

	hasArtifact, hasScript := len(t.Artifact) > 0, len(t.Script) > 0
	switch {
	case hasArtifact && hasScript:
		return nil, fmt.Errorf("%w：%q 两个段都写了。发布的是 release 二进制就只留 [artifact]，"+
			"是单文件脚本就只留 [script]", ErrBothForms, t.Name)
	case !hasArtifact && !hasScript:
		return nil, fmt.Errorf("%w：%q 的 toy.toml 里既没有 [artifact] 也没有 [script]。"+
			"发布了 release 资产就加 [artifact]（key 是 GOOS/GOARCH，value 是资产名，"+
			"可以用 {version} 和 {tag} 占位）；如果它就是一个单文件脚本，改用 [script]。"+
			"toy 不从源码构建，所以这两个都没有就装不了", ErrNoArtifact, t.Name)
	}

	// bin：PATH 上的名字 -> 解压后包内的相对路径。
	for name, target := range t.Bin {
		if err := checkBinName("bin", name); err != nil {
			return nil, err
		}
		if err := checkRelPath("bin", name, target); err != nil {
			return nil, err
		}
	}

	// script 段的 key 也会变成 PATH 上的名字，value 是要下载的资产名。
	for name, asset := range t.Script {
		if err := checkBinName("script", name); err != nil {
			return nil, err
		}
		if err := checkRelPath("script", name, asset); err != nil {
			return nil, err
		}
	}

	if hasArtifact && len(t.Bin) == 0 {
		return nil, fmt.Errorf("toyfile: %q 有 [artifact] 却没有 [bin]，"+
			"装完之后没有任何命令会出现在 PATH 上。加一行 [bin] 段，"+
			"形如 %s = \"%s\"，等号右边是解压后包内的相对路径", t.Name, t.Name, t.Name)
	}

	for plat, asset := range t.Artifact {
		if err := checkPlatformKey("artifact", plat); err != nil {
			return nil, err
		}
		// 资产名会被原样拼到 release 下载地址的末尾。含 ".." 或绝对路径的话，
		// URL 归一化之后指向的是另一个仓库 —— 客户端却仍然照着 toy.toml 打印
		// 「来源 github.com/<作者>」，那句话就成了假话（铁律 8）。空值同理：
		// 地址会以 / 结尾，下回来一张网页，没有 sha256 时还会被 TOFU 钉住当二进制。
		if err := checkRelPath("artifact", plat, asset); err != nil {
			return nil, err
		}
	}

	for plat, sum := range t.SHA256 {
		if err := checkPlatformKey("sha256", plat); err != nil {
			return nil, err
		}
		if !isHex64(sum) {
			return nil, fmt.Errorf("toyfile: [sha256] 里 %q 的值 %q 不是 64 位小写 hex。"+
				"用 sha256sum 算出来的那一串原样贴过来，不要带文件名、不要大写", plat, sum)
		}
		// key 打错一个字母，校验就会被静默跳过 —— 而铁律 4 说有 sha256 就必须强制校验，
		// 所以这里宁可拒绝，也不要装一个没人验过的二进制。
		if _, ok := t.Artifact[plat]; !ok {
			if hasScript {
				return nil, fmt.Errorf("toyfile: %q 是 [script] 形态，没有按平台区分的资产，"+
					"所以 [sha256] 里的 %q 永远不会被用到。单文件脚本靠首次安装钉住哈希（TOFU），"+
					"把整个 [sha256] 段删掉即可", t.Name, plat)
			}
			return nil, fmt.Errorf("toyfile: [sha256] 里有 %q，但 [artifact] 里没有这个平台。"+
				"两边的 key 必须一模一样，否则这条 hash 永远不会被用到，等于没写", plat)
		}
	}

	return &t, nil
}

// IsScript 表示这是单文件脚本形态：下载 + chmod +x + 软链，零解压零构建。
func (t *Toyfile) IsScript() bool { return len(t.Script) > 0 }

// Asset 按 goos/goarch 挑出资产名并展开 {version} / {tag} 占位符，
// 同时返回该平台声明的 sha256（没有就是空串）。
// 找不到当前平台返回 ErrNoPlatform，并在 error 里列出它支持哪些平台。
//
// 只试 goos/goarch 一个 key，没有回退。要 musl / Rosetta / arm 代次那几层回退
// 就用 AssetFor（internal/platform.Candidates 生成候选）。
func (t *Toyfile) Asset(goos, goarch, version, tag string) (asset, sha256 string, err error) {
	a, sum, _, err := t.AssetFor([]string{goos + "/" + goarch}, version, tag)
	return a, sum, err
}

// AssetFor 按候选顺序找第一个命中的平台，返回资产名、声明的 sha256、
// 以及真正命中的那个候选 key —— 调用方要用它告诉用户走的是回退
// （装 linux/amd64 的包到 Alpine 上、或在 M 系列 Mac 上装 darwin/amd64，
// 都得说出来，铁律 8）。
func (t *Toyfile) AssetFor(candidates []string, version, tag string) (asset, sha256, matched string, err error) {
	if t.IsScript() {
		return "", "", "", fmt.Errorf("toyfile: %q 是单文件脚本形态（[script]），没有按平台区分的资产。"+
			"调用方应当先看 IsScript()，再直接用 Script 里的每一项", t.Name)
	}

	for _, plat := range candidates {
		tmpl, ok := t.Artifact[plat]
		if !ok {
			continue
		}
		// {version} / {tag} 是远端给的（git ls-remote 拿到的 tag、或调用方的 --version），
		// 展开之后这一串才是真正会被拼进下载地址的东西，所以在这个唯一的出口再验一次。
		expanded := Expand(tmpl, version, tag)
		if err := checkRelPath("artifact", plat, expanded); err != nil {
			return "", "", "", err
		}
		return expanded, t.SHA256[plat], plat, nil
	}

	// 两边都列出来：用户要知道我们试过什么，作者要知道该补哪个 key。
	// 建议补的那个 key 要取底座（两段），不能直接用首选候选：linux 上的首选候选
	// 是我们自己合成的 linux/amd64/gnu，而没有任何人会去发布一个叫 gnu 的资产 ——
	// 让用户拿着这句话去给作者提 issue，提的就是一个错的要求。
	want := "当前平台"
	if len(candidates) > 0 {
		want = baseKey(candidates[0])
	}
	return "", "", "", fmt.Errorf("%w：试过 %s，这个 toy.toml 只声明了 %s。"+
		"想用的话去上游提 issue，请作者在 release 里补一个 %s 的资产并在 toy.toml 里加一行",
		ErrNoPlatform, describePlatforms(candidates), describePlatforms(t.Platforms()), want)
}

// baseKey 把平台 key 截到前两段（goos/goarch）。三段的 linux/amd64/musl → linux/amd64；
// 本来就两段的原样返回。
func baseKey(plat string) string {
	if i := strings.Index(plat, "/"); i >= 0 {
		if j := strings.Index(plat[i+1:], "/"); j >= 0 {
			return plat[:i+1+j]
		}
	}
	return plat
}

// Platforms 返回它声明支持的平台列表，排序稳定，报错时给用户看。
func (t *Toyfile) Platforms() []string {
	out := make([]string, 0, len(t.Artifact))
	for plat := range t.Artifact {
		out = append(out, plat)
	}
	sort.Strings(out)
	return out
}

// Expand 展开资产名里的两个占位符：{version} 不带 v 前缀，{tag} 原样。
// 传进来的 version 带不带 v 都行 —— 这里统一剥掉，免得每个调用方各剥一次。
func Expand(tmpl, version, tag string) string {
	return strings.NewReplacer(
		"{version}", strings.TrimPrefix(version, "v"),
		"{tag}", tag,
	).Replace(tmpl)
}

func describePlatforms(plats []string) string {
	if len(plats) == 0 {
		return "一个平台都没有"
	}
	return strings.Join(plats, "、")
}

// checkName：name 会变成 ~/.toy/pkg 下的一级目录名，所以不能是路径。
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New(`toyfile: toy.toml 缺少 name。加一行 name = "你的工具名"，` +
			`它就是装完之后出现在 ~/.toy 里的目录名`)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: name %q 不能含路径分隔符，它只是一个名字，不是路径", name)
	case name == "." || name == ".." || strings.Contains(name, ".."):
		return fmt.Errorf(`toyfile: name %q 不能含 ".."，它会被当成目录名用`, name)
	}
	return checkPrintable("name", name)
}

// checkBinName：这个 key 会变成 ~/.toy/bin 下的软链名，同样不能是路径。
func checkBinName(section, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("toyfile: [%s] 里有一个空的名字。等号左边是它在 PATH 上的命令名", section)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: [%s] 里的名字 %q 不能含路径分隔符 —— "+
			"等号左边是 PATH 上的命令名，右边才是路径", section, name)
	case name == "." || name == "..":
		return fmt.Errorf("toyfile: [%s] 里的名字 %q 不是一个能用的命令名", section, name)
	case strings.ContainsAny(name, " \t"):
		return fmt.Errorf("toyfile: [%s] 里的名字 %q 含空白字符，"+
			"它是要软链进 ~/.toy/bin 的命令名，带空格的话在 shell 里根本敲不出来", section, name)
	}
	return checkPrintable(fmt.Sprintf("[%s] 里的名字 %q", section, name), name)
}

// checkRelPath：等号右边必须是包内的相对路径，逃不出去（铁律 5、7）。
func checkRelPath(section, name, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("%w：[%s] 里 %q 的路径是空的，写上解压后包内的相对路径",
			ErrUnsafeTarget, section, name)
	case strings.HasPrefix(target, "/") || filepath.IsAbs(target):
		return fmt.Errorf("%w：[%s] 里 %q 指向绝对路径 %q。"+
			"这里只能写包内的相对路径 —— 绝对路径在别人的机器上指向的是别的东西",
			ErrUnsafeTarget, section, name, target)
	}
	for _, seg := range strings.Split(filepath.ToSlash(target), "/") {
		if seg == ".." {
			return fmt.Errorf(`%w：[%s] 里 %q 的路径 %q 含 ".." 段，会逃出包目录`,
				ErrUnsafeTarget, section, name, target)
		}
	}
	// [artifact] 和 [script] 的值会被原样拼进下载地址（host.ReleaseURL / RawURL），
	// 而 ? 和 # 在 URL 里是 query 和 fragment 的分隔符：写成 "toy.tar.gz?x" 时
	// 真正被请求的是 "toy.tar.gz"，写成 "a#b" 时是 "a" —— 静默地取到另一个文件，
	// 而客户端照着 toy.toml 打印的仍然是作者写的那个名字（铁律 8）。
	// fetch 对 file:// 早就是「宁可报错也不猜」，这里把同一条规矩补齐。
	// 带 scheme 的绝对 URL 三道检查一个都不沾（不以 / 开头、没有 ".." 段、
	// 没有 ?#），但它同样不是「包内的相对路径」：规格 §3 说得很死 ——
	// [artifact] / [script] 里只写 release 资产名，**不写 URL**。放行的话，
	// 裸地址那条路上 base.ResolveReference 会直接跳到另一台机器（甚至
	// file:///etc/shadow 和 169.254.169.254），而客户端照着 toy.toml 打印的
	// 仍然是作者写的那个来源 —— 那句话就成了假话（铁律 8）。
	if i := strings.Index(target, "://"); i > 0 && isURLScheme(target[:i]) {
		return fmt.Errorf("%w：[%s] 里 %q 的值 %q 是一个 URL。这里只能写 release 里的"+
			"资产名（或包内相对路径）—— 地址由 toy 自己按托管方拼，写死 URL 会让它"+
			"指向 toy.toml 说的来源之外的地方",
			ErrUnsafeTarget, section, name, target)
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		return fmt.Errorf("%w：[%s] 里 %q 的路径 %q 含 %q。它会被拼进下载地址，"+
			"而 ? 和 # 在 URL 里会把后面的部分切掉，取回来的是另一个文件。"+
			"资产名里真有这两个字符就在 release 里改个名",
			ErrUnsafeTarget, section, name, target, string(target[i]))
	}
	return checkPrintable(fmt.Sprintf("[%s] 里 %q 的路径", section, name), target)
}

// isURLScheme 按 RFC 3986 判断 s 是不是一个 scheme（字母开头，后跟字母数字 +-.）。
// 严格按规范判，免得把 "a:b://c" 这种正常文件名误当成 URL 拦下来。
func isURLScheme(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// checkPlatformKey：key 必须是 GOOS/GOARCH，或者带一段 libc 的 GOOS/GOARCH/libc。
// 写错了不会报错，只会永远匹配不上 —— 所以这里宁可当场拒绝：作者以为自己发布了
// linux 版，用户被告知「没有适配的构建」，这中间没有任何地方会说话。
func checkPlatformKey(section, plat string) error {
	// runtime.GOOS/GOARCH 永远是小写、不带空格。写成 "Linux/AMD64" 或 " linux/amd64 "
	// 不会报错，只会永远匹配不上。这条要先判，因为下面的分段比较是逐字节的。
	if plat != strings.ToLower(plat) || strings.ContainsAny(plat, " \t") {
		return fmt.Errorf("toyfile: [%s] 里的 key %q 必须全小写且不带空格，"+
			"它要和 Go 的 GOOS/GOARCH 逐字节相等才匹配得上，比如 \"linux/amd64\"", section, plat)
	}
	parts := strings.Split(plat, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("toyfile: [%s] 里的 key %q 不是 GOOS/GOARCH 形式，"+
			`比如 "linux/amd64"、"darwin/arm64"，或者带一段 libc 的 "linux/amd64/musl"`, section, plat)
	}
	// arch 那一段不做白名单：Go 每加一个 GOARCH 都会让白名单变成一条假报错，
	// 而 armv6 / armv7 这种「GOARCH 分不出来的代次」本来就得能写进去。
	if len(parts) == 3 && parts[2] != "musl" && parts[2] != "gnu" {
		return fmt.Errorf("toyfile: [%s] 里的 key %q 的第三段 %q 不认识 —— "+
			"第三段是 libc，只能是 \"musl\"（Alpine 那类）或 \"gnu\"（glibc）。"+
			"别的东西写在这里永远匹配不上，等于这一行没写", section, plat, parts[2])
	}
	return nil
}

// checkPrintable：name/desc/license/homepage、命令名和路径都会被原样打印到终端，
// 而安装完那一行紧挨着的就是信任等级（规格 §4）。\r 加一段 ANSI 就能把已经打出来的行
// 擦掉重写，伪造一句「✓ 已签名 · Derrity」—— 铁律 8 要求信任等级如实打印，
// 所以在解析这一层就把控制字符拒掉：打印的地方有好几处，堵源头只要堵一次。
func checkPrintable(what, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("toyfile: %s 含控制字符 %q。这些字段会原样打印在终端上，"+
				"换行和转义序列可以把信任等级那一行覆盖掉，所以一概不收", what, r)
		}
	}
	return nil
}

func describeKeys(keys []toml.Key) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return strings.Join(out, "、")
}

func isHex64(s string) bool {
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
