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
	ErrNoArtifact   = errors.New("toyfile: this repo publishes no binaries")
	ErrBothForms    = errors.New("toyfile: [artifact] and [script] are mutually exclusive")
	ErrNoPlatform   = errors.New("toyfile: no build for this platform")
	ErrUnsafeTarget = errors.New("toyfile: unsafe bin target path")
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

	// Origin 记录这份 toyfile 是作者自己写的还是索引提供的（规格 §5 的内联 toyfile）。
	// **不进磁盘格式**：这是「这几行字节从哪来的」，不是作者能声明的东西 ——
	// 能声明就等于内联的那份可以自称 upstream，而信任等级正是照着它分档的（铁律 8）。
	// toml:"-" 也顺带让 toy.toml 里真写了 origin = 的时候落进 Undecoded 被拒。
	Origin string `toml:"-"`
}

// toyfile 的来源。Origin 只有这两个值。
const (
	OriginUpstream = "upstream" // 仓库里那份 toy.toml，作者自己写的
	OriginIndex    = "index"    // 索引条目内联的那份，提交索引记录的人写的
)

// Parse 解析并校验。校验必须拒绝：
//   - artifact 和 script 同时存在或都不存在
//   - name 为空或含路径分隔符
//   - bin/script 的 key 含 '/'，或 value 是绝对路径 / 含 ".."（铁律 5、7）
//   - sha256 的值不是 64 位 hex
//
// 报错要说人话，并告诉作者该怎么改。
func Parse(data []byte) (*Toyfile, error) { return ParseFrom(data, OriginUpstream) }

// ParseFrom 是 Parse 加上一条来源标记，校验规则**一模一样，一条都不放松**。
// 索引内联的那份是远端内容（索引谁都能 fork、谁都能提 PR），比作者自己仓库里
// 的那份更需要这些检查，不是更不需要 —— 尤其 bin 目标不许绝对路径 / 不许 ".."、
// artifact 值不许是 URL 这几条，它们挡的正是「一条恶意索引记录能干什么」。
func ParseFrom(data []byte, origin string) (*Toyfile, error) {
	var t Toyfile
	md, err := toml.Decode(string(data), &t)
	if err != nil {
		return nil, fmt.Errorf("toyfile: toy.toml is not valid TOML: %w", err)
	}
	// 认不出来的键一律拒，不能静默忽略：把 [sha256] 打成 [sha265] 的后果是
	// 「作者以为写了 hash、客户端从头到尾没校验过」，正是铁律 4 要防的那件事。
	// 顺带也拦住 install/postinstall/hook 这类键 —— toy 不执行包里的任何代码（铁律 1），
	// 静默忽略会让作者以为它们生效了。
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("toyfile: toy.toml has keys toy does not recognize: %s. "+
			"Only name / desc / license / homepage and the [bin] [artifact] [sha256] [script] sections are valid. "+
			"An unknown key would do nothing -- and a [sha256] with one letter wrong means no checksum verification "+
			"at all, so it is rejected outright rather than ignored. Also: toy never runs code from a package, "+
			"so install / build / hook keys do not exist in toy.toml",
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
		return nil, fmt.Errorf("%w: %q declares both sections. Shipping release binaries? Keep only [artifact]. "+
			"Shipping a single-file script? Keep only [script]", ErrBothForms, t.Name)
	case !hasArtifact && !hasScript:
		return nil, fmt.Errorf("%w: the toy.toml for %q has neither [artifact] nor [script], so there is nothing "+
			"to install -- toy never builds from source. If the project publishes release assets, add [artifact] "+
			"(key is GOOS/GOARCH, value is the asset name, {version} and {tag} are available as placeholders); "+
			"if it is a single-file script, use [script] instead. Either way that is about 8 lines of toml, "+
			"and you do not need anyone's approval to add it", ErrNoArtifact, t.Name)
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
		return nil, fmt.Errorf("toyfile: %q has [artifact] but no [bin], so installing it puts no command on "+
			"your PATH. Add a [bin] section, e.g. %s = \"%s\" -- the right-hand side is the path inside the "+
			"extracted package", t.Name, t.Name, t.Name)
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
			return nil, fmt.Errorf("toyfile: the [sha256] value for %q is %q, which is not 64 lowercase hex "+
				"characters. Paste the digest sha256sum prints, verbatim -- no filename, no uppercase", plat, sum)
		}
		// key 打错一个字母，校验就会被静默跳过 —— 而铁律 4 说有 sha256 就必须强制校验，
		// 所以这里宁可拒绝，也不要装一个没人验过的二进制。
		if _, ok := t.Artifact[plat]; !ok {
			if hasScript {
				return nil, fmt.Errorf("toyfile: %q is a [script] package and has no per-platform assets, so the "+
					"[sha256] entry for %q would never be used. Single-file scripts pin their checksum on first "+
					"install (trust on first use), so just delete the whole [sha256] section", t.Name, plat)
			}
			return nil, fmt.Errorf("toyfile: [sha256] has %q but [artifact] does not. The keys must match "+
				"exactly, or this checksum is never used -- the same as not writing it at all", plat)
		}
	}

	t.Origin = origin
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
		return "", "", "", fmt.Errorf("toyfile: %q is a single-file script package ([script]) and has no "+
			"per-platform assets. Callers should check IsScript() first and use each Script entry directly", t.Name)
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
	want := "the current platform"
	if len(candidates) > 0 {
		want = baseKey(candidates[0])
	}
	return "", "", "", fmt.Errorf("%w: tried %s, but this toy.toml only declares %s. "+
		"To get it, open an issue upstream asking for a %s asset in the release plus one more line in toy.toml",
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
		return "none"
	}
	return strings.Join(plats, ", ")
}

// checkName：name 会变成 ~/.toy/pkg 下的一级目录名，所以不能是路径。
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New(`toyfile: toy.toml is missing name. Add a line name = "your-tool-name" -- ` +
			`it becomes the directory name under ~/.toy once installed`)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: name %q must not contain a path separator; it is a name, not a path", name)
	case name == "." || name == ".." || strings.Contains(name, ".."):
		return fmt.Errorf(`toyfile: name %q must not contain ".."; it is used as a directory name`, name)
	}
	return checkPrintable("name", name)
}

// checkBinName：这个 key 会变成 ~/.toy/bin 下的软链名，同样不能是路径。
func checkBinName(section, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("toyfile: [%s] has an entry with an empty name. "+
			"The left-hand side is the command name on your PATH", section)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("toyfile: the name %q in [%s] must not contain a path separator -- "+
			"the left-hand side is the command name on PATH, the right-hand side is the path", name, section)
	case name == "." || name == "..":
		return fmt.Errorf("toyfile: the name %q in [%s] is not a usable command name", name, section)
	case strings.ContainsAny(name, " \t"):
		return fmt.Errorf("toyfile: the name %q in [%s] contains whitespace. It is symlinked into ~/.toy/bin "+
			"as a command name, and a name with spaces is unusable from a shell", name, section)
	}
	return checkPrintable(fmt.Sprintf("the name %q in [%s]", name, section), name)
}

// checkRelPath：等号右边必须是包内的相对路径，逃不出去（铁律 5、7）。
func checkRelPath(section, name, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("%w: the path for %q in [%s] is empty. Write the relative path inside the "+
			"extracted package", ErrUnsafeTarget, name, section)
	case strings.HasPrefix(target, "/") || filepath.IsAbs(target):
		return fmt.Errorf("%w: %q in [%s] points at the absolute path %q. Only a path inside the package "+
			"belongs here -- an absolute path means something else entirely on someone else's machine",
			ErrUnsafeTarget, name, section, target)
	}
	for _, seg := range strings.Split(filepath.ToSlash(target), "/") {
		if seg == ".." {
			return fmt.Errorf(`%w: the path %q for %q in [%s] contains a ".." segment, which escapes the `+
				`package directory`, ErrUnsafeTarget, target, name, section)
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
		return fmt.Errorf("%w: the value %q for %q in [%s] is a URL. Only the release asset name "+
			"(or a path inside the package) belongs here -- toy builds the address itself from the host, "+
			"and a hardcoded URL would point somewhere other than the source toy.toml claims",
			ErrUnsafeTarget, target, name, section)
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		return fmt.Errorf("%w: the path %q for %q in [%s] contains %q. It is appended to the download URL, "+
			"where ? and # cut off everything after them, so a different file comes back. If the asset name "+
			"really contains that character, rename it in the release",
			ErrUnsafeTarget, target, name, section, string(target[i]))
	}
	return checkPrintable(fmt.Sprintf("the path for %q in [%s]", name, section), target)
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
		return fmt.Errorf("toyfile: the key %q in [%s] must be all lowercase with no whitespace -- it has to "+
			"equal Go's GOOS/GOARCH byte for byte to ever match, e.g. \"linux/amd64\"", plat, section)
	}
	parts := strings.Split(plat, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("toyfile: the key %q in [%s] is not in GOOS/GOARCH form -- "+
			`e.g. "linux/amd64", "darwin/arm64", or with a libc segment, "linux/amd64/musl"`, plat, section)
	}
	// arch 那一段不做白名单：Go 每加一个 GOARCH 都会让白名单变成一条假报错，
	// 而 armv6 / armv7 这种「GOARCH 分不出来的代次」本来就得能写进去。
	if len(parts) == 3 && parts[2] != "musl" && parts[2] != "gnu" {
		return fmt.Errorf("toyfile: the key %q in [%s] has an unrecognized third segment %q -- that segment "+
			"is the libc and can only be \"musl\" (Alpine and friends) or \"gnu\" (glibc). Anything else "+
			"will never match, so the line does nothing", plat, section, parts[2])
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
			return fmt.Errorf("toyfile: %s contains the control character %q. These fields are printed to your "+
				"terminal verbatim, and newlines or escape sequences can overwrite the trust level line, "+
				"so none of them are accepted", what, r)
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
	return strings.Join(out, ", ")
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
