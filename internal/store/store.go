// Package store 管 ~/.toy 的一切：目录布局、config.toml、安装清单、软链。规格 §6。
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Cirraya-LLC/toy/internal/host"
)

var (
	ErrShadow       = errors.New("store: 这个命令名会遮蔽 PATH 上已有的程序")
	ErrNotInstalled = errors.New("store: 没装过这个包")
	ErrLinkExists   = errors.New("store: 已经有同名的链接了")
)

// Trust 等级，如实打给用户看（铁律 8）。
const (
	TrustSigned     = "signed"     // 索引 + 作者签名
	TrustHost       = "host"       // 托管方的账号体系
	TrustUnverified = "unverified" // 裸 URL
)

// DefaultIndexURL 是内置索引的地址。**这就是那一个常量** —— 索引真的传上线之后，
// 把地址填在这里就是全部改动，别处一个字都不用动（规格 §5：索引来源「实现上就是
// 一个 URL」，file:// 和 https:// 走的是同一条代码路径）。
//
// 现在是空串，表示【不预置任何索引】。v1 的索引就是仓库里那个本地文件
// （index/toy-index.json），还没传到任何静态托管上 —— 预置一个 404 的地址，
// 等于每个新用户第一次 `toy doctor` / `toy search` 就吃一条红叉，而那不是他的
// 问题（铁律 8：不许把我们自己的状态说成用户的错）。没有索引是合法状态：
// `toy install github:owner/repo` 那条路从头到尾就不经过索引。
const DefaultIndexURL = ""

const defaultIndexName = "default"

// ~/.toy 下的一级目录。规格 §6。
const (
	dirBin       = "bin"
	dirPkg       = "pkg"
	dirCache     = "cache"
	dirInstalled = "installed"
	dirKeys      = "keys"
	dirKnown     = "known"
	dirIndex     = "index"
	configName   = "config.toml"
)

// Config 是 ~/.toy/config.toml。
type Config struct {
	Indexes []IndexRef           `toml:"index"`
	Mirrors []string             `toml:"mirrors"`
	Hosts   map[string]host.Host `toml:"hosts"` // 覆盖/新增托管方，加三行就多支持一家
}

type IndexRef struct {
	Name string `toml:"name"`
	URL  string `toml:"url"` // file:// 或 https://，v1 用 file:// 就够，以后改一行
}

// DefaultConfig 返回内置默认：DefaultIndexURL 非空时预置那一个索引，否则一个都不预置。
func DefaultConfig() Config {
	c := Config{Indexes: []IndexRef{}, Mirrors: []string{}, Hosts: map[string]host.Host{}}
	if DefaultIndexURL != "" {
		c.Indexes = append(c.Indexes, IndexRef{Name: defaultIndexName, URL: DefaultIndexURL})
	}
	return c
}

type Store struct{ Root string }

// Open 打开（必要时创建）store。root 为空时用 ~/.toy，
// 受 TOY_HOME 环境变量覆盖（测试要靠它）。
func Open(root string) (*Store, error) {
	if root == "" {
		root = os.Getenv("TOY_HOME")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("store: 找不到用户主目录，也没设 TOY_HOME —— "+
				"设一个 TOY_HOME=/某个目录 再试：%w", err)
		}
		root = filepath.Join(home, ".toy")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: %q 没法转成绝对路径：%w", root, err)
	}
	s := &Store{Root: abs}

	// keys/ 里是私钥，只给自己看。其余目录 0755。
	for _, d := range []string{dirBin, dirPkg, dirCache, dirInstalled, dirKnown, dirIndex} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
			return nil, fmt.Errorf("store: 建不出目录 %s：%w", filepath.Join(abs, d), err)
		}
	}
	if err := os.MkdirAll(s.KeysDir(), 0o700); err != nil {
		return nil, fmt.Errorf("store: 建不出目录 %s：%w", s.KeysDir(), err)
	}
	// MkdirAll 对已经存在的目录是空操作 —— 一个之前被建成 0755 的 keys/ 会一直
	// 松着，而里面是私钥。收紧失败就报错，不能装作没看见（铁律 8）。
	if fi, err := os.Stat(s.KeysDir()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(s.KeysDir(), 0o700); err != nil {
			return nil, fmt.Errorf("store: %s 是 %o，别人读得到里面的私钥，"+
				"而且收紧不了：%w", s.KeysDir(), fi.Mode().Perm(), err)
		}
	}
	return s, nil
}

func (s *Store) BinDir() string   { return filepath.Join(s.Root, dirBin) }
func (s *Store) CacheDir() string { return filepath.Join(s.Root, dirCache) }
func (s *Store) KeysDir() string  { return filepath.Join(s.Root, dirKeys) }
func (s *Store) KnownDir() string { return filepath.Join(s.Root, dirKnown) }
func (s *Store) IndexDir() string { return filepath.Join(s.Root, dirIndex) }

// PkgDir 是 pkg/<host>/<owner>/<name>/<version>/
//
// 每一段都过一次 safeSeg：这些值一路来自 toy.toml 和用户输入，
// 一个 "../.." 就能把解压目标挪到 ~/.toy 外面去（铁律 7 的同一类问题），
// 而两个不同的段转义成同一个名字会让两个包共用一个安装目录。
func (s *Store) PkgDir(hostName, owner, name, version string) string {
	return filepath.Join(s.Root, dirPkg,
		safeSeg(hostName), safeSeg(owner), safeSeg(name), safeSeg(version))
}

func (s *Store) configPath() string { return filepath.Join(s.Root, configName) }

// LoadConfig 读 config.toml。文件不存在就是全新的 store，返回内置默认。
func (s *Store) LoadConfig() (Config, error) {
	data, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("store: 读不了 %s：%w", s.configPath(), err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("store: %s 不是合法的 TOML：%w", s.configPath(), err)
	}
	if c.Mirrors == nil {
		c.Mirrors = []string{}
	}
	if c.Hosts == nil {
		c.Hosts = map[string]host.Host{}
	}
	// Host.Name 是 `toml:"-"`：名字是表头那一段（[hosts.github]），不在表里面，
	// 所以解出来是空的，这里补回去。Lookup 拿它报错时要说出是哪一家。
	for name, h := range c.Hosts {
		h.Name = name
		c.Hosts[name] = h
	}
	return c, nil
}

func (s *Store) SaveConfig(c Config) error {
	data, err := encodeTOML("# toy 的配置：索引列表、镜像列表、托管方覆盖。规格 §6。", c)
	if err != nil {
		return fmt.Errorf("store: 序列化配置失败：%w", err)
	}
	if err := writeFileAtomic(s.configPath(), data, 0o644); err != nil {
		return fmt.Errorf("store: 写不了 %s：%w", s.configPath(), err)
	}
	return nil
}

// encodeTOML 把 v 编码成 TOML，顶上加一行给人看的注释。
func encodeTOML(header string, v any) ([]byte, error) {
	body, err := toml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(header+"\n\n"), body...), nil
}

// Manifest 是一次安装的完整记录。没有它就没法 uninstall 和 update。
type Manifest struct {
	ID string `toml:"id"` // "github:Derrity/tinyserve"
	// IndexID 是这个包在索引里的 id（"Derrity/tinyserve"），经索引装的才有。
	// 没有它，`toy update` 只能拿 ID 走托管方那条路，于是作者的签名在第一次
	// 升级之后就永远消失了 —— 一个升一次级就掉一档信任的包管理器（铁律 8）。
	IndexID     string   `toml:"index_id"`
	Name        string   `toml:"name"`
	Version     string   `toml:"version"`
	Tag         string   `toml:"tag"`
	Host        string   `toml:"host"`
	Owner       string   `toml:"owner"`
	Repo        string   `toml:"repo"`
	Trust       string   `toml:"trust"`  // 上面三个常量之一
	Signer      string   `toml:"signer"` // 公钥指纹，未签名为空
	SHA256      string   `toml:"sha256"`
	Source      string   `toml:"source"` // 实际下载成功的 URL
	PkgDir      string   `toml:"pkg_dir"`
	Links       []string `toml:"links"`        // 创建的软链绝对路径
	InstalledAt string   `toml:"installed_at"` // RFC3339
}

// manifestPath 把 id 变成 installed/ 下的一个文件名。
//
// id 长成 "github:Derrity/tinyserve"，里面的 : 和 / 直接当文件名会写到别的
// 目录里去，所以先把非白名单字符换成 _，再缀上 id 的 sha256 前 8 位 ——
// 转义是多对一的（a/b 和 a:b 会撞），hash 后缀把唯一性还回来。
func (s *Store) manifestPath(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("store: 包 id 是空的，没法定位安装清单")
	}
	sum := sha256.Sum256([]byte(id))
	file := safeSlug(id) + "-" + hex.EncodeToString(sum[:])[:8] + ".toml"
	return filepath.Join(s.Root, dirInstalled, file), nil
}

// safeSlug 把一段任意文本变成能安全当文件名/目录名的形式。
// 白名单之外一律换成 _，所以 / : .. 都跑不出当前目录。
//
// 大写字母也一律折成小写：macOS 默认的 APFS 大小写不敏感，"Foo" 和 "foo"
// 在那里是【同一个目录】—— 不折的话 Derrity/tinyserve 和 derrity/TinySERVE
// 会得到两个看起来不同、实际是同一个的 pkg 目录，两个包共用一份文件，
// 卸掉其中一个会把另一个的可执行文件一起删掉。折叠和转义一样是多对一的，
// 唯一性由调用方缀上的原串 hash 还回来（见 safeSeg 和 manifestPath）。
func safeSlug(s string) string {
	if s == "" {
		return "_"
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b = append(b, byte(r)+('a'-'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	// 全是点（"." ".."）在文件系统里有特殊含义，换掉。
	if strings.Trim(string(b), ".") == "" {
		b = []byte(strings.Repeat("_", len(b)))
	}
	const maxName = 96 // 留出 hash 后缀和扩展名的余量；全是 ASCII，截断不会切碎字符
	if len(b) > maxName {
		b = b[:maxName]
	}
	return string(b)
}

// safeSeg 是 safeSlug 加上「动过就缀 hash」。
//
// 转义和小写折叠都是多对一的：owner "a/b" 和 "a:b" 都变成 "a_b"，"Foo" 和
// "foo" 都变成 "foo"，超过 maxName 的两个名字截断后也一样 —— 两个不同的包
// 于是共用一个安装目录，装后一个会盖掉前一个的文件，卸后一个会把前一个的
// 可执行文件一起删掉。manifestPath 早就是这么解决的（见那里的注释），
// 这里用同一招：slug 和原串一字不差才原样保留，所以常规路径仍然是
// pkg/github/derrity-1f0a3c7d/tinyserve/1.2.3 这种还读得懂的样子。
func safeSeg(s string) string {
	slug := safeSlug(s)
	if slug == s {
		return slug
	}
	sum := sha256.Sum256([]byte(s))
	return slug + "-" + hex.EncodeToString(sum[:])[:8]
}

// redactURL 把 URL 里的密码换成 xxxxx（用标准库的 URL.Redacted）。
//
// 用户完全可以写 `toy install https://u:hunter2@example.com/toy.toml`，
// fetch 需要原样的 URL 去认证，但清单是长期躺在磁盘上、还会被打印出来的东西 ——
// 一个装一次就把密码写进 ~/.toy/installed/*.toml 的包管理器不能叫安全边界。
// 不是 URL（解析不了、或者压根没有密码）就原样返回。
func redactURL(s string) string {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return s // 绝大多数地址没有凭据，不必解析
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); !ok {
		return s
	}
	return u.Redacted()
}

func (s *Store) SaveManifest(m Manifest) error {
	path, err := s.manifestPath(m.ID)
	if err != nil {
		return err
	}
	// 落盘之前把 URL 里的密码抹掉：清单是一个长期躺在磁盘上、还会被 `toy list`
	// 打出来的文件，而 fetch 用的是内存里那份原样的 URL，不受影响。
	m.Source = redactURL(m.Source)
	data, err := encodeTOML("# toy 安装清单，自动生成。删了就没法 uninstall / update。", m)
	if err != nil {
		return fmt.Errorf("store: 序列化安装清单失败：%w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("store: 写不了安装清单 %s：%w", path, err)
	}
	return nil
}

func (s *Store) LoadManifest(id string) (Manifest, error) {
	path, err := s.manifestPath(id)
	if err != nil {
		return Manifest{}, err
	}
	m, err := readManifest(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%w：%s。`toy list` 看看装了什么", ErrNotInstalled, id)
	}
	return m, err
}

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("store: 安装清单 %s 坏了：%w（删掉它可以恢复，但那个包就得手动清）", path, err)
	}
	return m, nil
}

// ListManifests 按 id 排序返回全部安装清单。
// 读不动的清单直接报错而不是跳过 —— 静默跳过等于让那个包变成卸不掉的。
func (s *Store) ListManifests() ([]Manifest, error) {
	dir := filepath.Join(s.Root, dirInstalled)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读不了 %s：%w", dir, err)
	}
	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		m, err := readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) RemoveManifest(id string) error {
	path, err := s.manifestPath(id)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w：%s", ErrNotInstalled, id)
	}
	if err != nil {
		return fmt.Errorf("store: 删不掉安装清单 %s：%w", path, err)
	}
	return nil
}

// FindByName 按命令名或包名找已装的包，用于 `toy uninstall tinyserve`。
// 命中多个时返回 error 并列出候选，让用户写全 id。
func (s *Store) FindByName(name string) (Manifest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Manifest{}, fmt.Errorf("store: 要找的名字是空的")
	}
	all, err := s.ListManifests()
	if err != nil {
		return Manifest{}, err
	}

	var hits []Manifest
	for _, m := range all {
		// id 是唯一的，写全了就没有歧义可言，直接命中。
		if m.ID == name {
			return m, nil
		}
		if manifestMatches(m, name) {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Manifest{}, fmt.Errorf("%w：%q。`toy list` 看看装了什么", ErrNotInstalled, name)
	default:
		ids := make([]string, 0, len(hits))
		for _, m := range hits {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return Manifest{}, fmt.Errorf("store: %q 对上了 %d 个已装的包：%s。"+
			"写全上面某一个完整 id 再试", name, len(hits), strings.Join(ids, "、"))
	}
}

// manifestMatches：包名、仓库名，或者它建的任何一个软链的文件名。
func manifestMatches(m Manifest, name string) bool {
	if m.Name == name || m.Repo == name {
		return true
	}
	for _, l := range m.Links {
		if filepath.Base(l) == name {
			return true
		}
	}
	return false
}

// Link 在 BinDir 里建一个指向 target 的软链。
// 铁律 5：如果 name 已经存在于 PATH 上（且不是我们自己 BinDir 里的），
// 返回 ErrShadow，除非 force。这一条堵掉 bin = "sudo" 这类提权。
func (s *Store) Link(name, target string, force bool) (string, error) {
	if err := validLinkName(name); err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("store: 链接目标 %q 没法转成绝对路径：%w", target, err)
	}
	// 相对目标会相对 bin/ 解析，几乎永远不是作者想要的；顺手确认它真的存在，
	// 免得建出一条断链、用户运行时才看到「命令找不到」。
	fi, err := os.Stat(absTarget)
	if err != nil {
		return "", fmt.Errorf("store: 链接目标 %s 用不了（包里可能没有这个文件，"+
			"对一下 toy.toml 的 [bin] 写的相对路径）：%w", absTarget, err)
	}
	// 链到一个目录上会造出一个「存在但跑不了」的命令，和断链是同一种坏法。
	if fi.IsDir() {
		return "", fmt.Errorf("store: 链接目标 %s 是个目录，不是可执行文件 —— "+
			"toy.toml 的 [bin] 要写到具体文件", absTarget)
	}

	binDir := s.BinDir()
	linkPath := filepath.Join(binDir, name)

	// 遮蔽检查放在「已存在」之前：它是安全判断，是那个必须让用户亲手 --force 的。
	if !force {
		// exec.LookPath 找到的东西落在一个相对 PATH 项里时（"." 或者最常见的
		// PATH 里那个空项），Go 1.19 起会连同 exec.ErrDot 一起返回 —— 只判
		// err == nil 的话，整条 PATH 的遮蔽检查会被这一个相对项静默关掉，
		// 而 PATH="…:" 这种写法到处都是。它照样是 PATH 上一个同名的程序，算遮蔽。
		if found, err := exec.LookPath(name); err == nil || errors.Is(err, exec.ErrDot) {
			if abs, aerr := filepath.Abs(found); aerr == nil {
				found = abs
			}
			if !sameDir(filepath.Dir(found), binDir) {
				return "", fmt.Errorf("%w：%q 在 PATH 上已经是 %s。装上去会把它顶掉；"+
					"确认要覆盖再加 --force", ErrShadow, name, found)
			}
		}
		if _, err := os.Lstat(linkPath); err == nil {
			return "", fmt.Errorf("%w：%s 已经存在。先 `toy uninstall` 那个包，或者加 --force 替换",
				ErrLinkExists, linkPath)
		}
	}

	// 顶掉一条已经存在的软链 = 这个名字换了主人。别的包的清单里可能还记着它，
	// 而那条记录会在 `toy uninstall 那个包` 时被当成「删我自己的链」执行掉 ——
	// 删掉的其实是现在这个包的命令，新包的清单一个字没动，`toy list` 照样说它
	// 装着，doctor 也看不出来（那不是悬空软链，是根本不存在）。所以在建链之前
	// 先把过期的记录摘掉。放在建链【之前】：失败时什么都还没动。
	if _, err := os.Lstat(linkPath); err == nil {
		if err := s.disownLink(linkPath, absTarget); err != nil {
			return "", err
		}
	}

	if err := symlinkAtomic(absTarget, linkPath); err != nil {
		return "", fmt.Errorf("store: 建不了软链 %s：%w", linkPath, err)
	}
	return linkPath, nil
}

// disownLink 把 linkPath 从所有「已经不再拥有它」的安装清单里删掉。
// 拥有者的判据是软链此刻指向谁：目标落在哪个包的 PkgDir 里，这个名字就是谁的。
func (s *Store) disownLink(linkPath, newTarget string) error {
	all, err := s.ListManifests()
	if err != nil {
		return err
	}
	for _, m := range all {
		if withinDir(m.PkgDir, newTarget) {
			continue // 就是这个包自己在重装/升级，记录没过期
		}
		kept := make([]string, 0, len(m.Links))
		for _, l := range m.Links {
			if !sameFile(l, linkPath) {
				kept = append(kept, l)
			}
		}
		if len(kept) == len(m.Links) {
			continue
		}
		m.Links = kept
		if err := s.SaveManifest(m); err != nil {
			return fmt.Errorf("store: %s 的软链换了主人，但改不了 %s 的安装清单 —— "+
				"就这么装上去的话，卸载 %s 会把这条链删掉：%w", linkPath, m.ID, m.ID, err)
		}
	}
	return nil
}

// sameFile 比两条路径是不是同一个文件名（软链本身不解引用，它可能是断的）。
func sameFile(a, b string) bool {
	if absA, err := filepath.Abs(a); err == nil {
		a = absA
	}
	if absB, err := filepath.Abs(b); err == nil {
		b = absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// withinDir 报告 path 是否在 dir 里面（或者就是 dir）。dir 为空一律 false。
func withinDir(dir, path string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// reservedLinkNames 是 toy 自己会按【名字】从 PATH 上 exec 的程序。
//
// 遮蔽检查只挡「PATH 上已经存在」的同名程序（LookPath 找得到才算），所以在一台
// 还没装 git 的机器上（macOS 没装 Xcode CLT、slim 容器、CI 基础镜像都是这样，
// 而裸地址那条安装路径压根不需要 git），一个 toy.toml 写 [bin] git = "payload"
// 就能干干净净地把 bin/git 占下来 —— 而 bin/ 正是 doctor 教用户放在 PATH 最前面
// 的目录。装的那一刻确实一行都没跑（铁律 1 的字面意思成立），但下一条
// `toy install github:…` / `toy update` / `toy publish` / `toy doctor` 就把它当
// git 执行了，执行只是推迟了一条命令。
//
// --force 不放行：这跟 bin = "sudo" 不是一类风险，用户对着「确认要覆盖再加
// --force」判断不出后果 —— 一个包没有任何正当理由要占住 toy 自己的工具链名字。
// 以后 toy 多 exec 一个程序，这里就要多一行。
var reservedLinkNames = map[string]bool{"git": true}

func validLinkName(name string) error {
	// 折成小写再比：macOS 的 APFS 和 Windows 都大小写不敏感，bin/GIT 在那里
	// 就是 bin/git。
	if reservedLinkNames[strings.ToLower(name)] {
		return fmt.Errorf("store: 命令名 %q 是 toy 自己要执行的程序 —— 让它落进 %s 就等于"+
			"下一条 toy 命令去跑这个包里的文件（铁律 1）。这一条 --force 也不放行", name, dirBin)
	}
	switch {
	case name == "":
		return fmt.Errorf("store: 命令名是空的（toy.toml 的 [bin] 段里 key 就是 PATH 上的名字）")
	case name == "." || name == "..":
		return fmt.Errorf("store: 命令名不能是 %q", name)
	case strings.ContainsRune(name, os.PathSeparator), strings.ContainsRune(name, '/'):
		return fmt.Errorf("store: 命令名 %q 里不能有斜杠 —— 它是 PATH 上的一个名字，不是路径", name)
	case strings.TrimSpace(name) != name:
		// 首尾空白的名字在 shell 里根本敲不出来，而它长得跟正常名字一模一样：
		// "sudo " 既装得上又逃过遮蔽检查（LookPath("sudo ") 什么都找不到）。
		return fmt.Errorf("store: 命令名 %q 首尾有空白，敲不出来这个名字", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("store: 命令名 %q 里有控制字符，那不是一个能敲出来的命令名", name)
		}
	}
	return nil
}

// symlinkAtomic 先建一个临时名再 rename 顶上去。
// 不用 remove-then-symlink：那中间有一段时间这个命令是不存在的，
// 而这个名字很可能正被别的进程（或另一个 toy）用着。
func symlinkAtomic(target, linkPath string) error {
	dir := filepath.Dir(linkPath)
	var tmp string
	for i := 0; ; i++ {
		var buf [6]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return err
		}
		tmp = filepath.Join(dir, "."+filepath.Base(linkPath)+".tmp"+hex.EncodeToString(buf[:]))
		err := os.Symlink(target, tmp)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || i >= 8 {
			return err
		}
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Unlink 删掉一条我们建的软链。
// 只删 BinDir 里面的东西：清单是磁盘上的文件，坏掉的清单不能变成
// 「删 /usr/bin/sudo」的授权。已经不在了当成删过（uninstall 要可重入）。
func (s *Store) Unlink(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("store: %q 没法转成绝对路径：%w", path, err)
	}
	binDir := s.BinDir()
	if !sameDir(filepath.Dir(abs), binDir) {
		return fmt.Errorf("store: 拒绝删 %s —— 它不在 %s 里，toy 只删自己建的软链", abs, binDir)
	}
	err = os.Remove(abs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: 删不掉 %s：%w", abs, err)
	}
	return nil
}

// InPath 报告 BinDir 是否已经在用户的 PATH 里，toy doctor 用。
func (s *Store) InPath() bool { return onPath(s.BinDir(), os.Getenv("PATH")) }

func onPath(dir, pathEnv string) bool {
	want := canonDir(dir)
	for _, e := range filepath.SplitList(pathEnv) {
		if e == "" {
			e = "." // POSIX：PATH 里的空项就是当前目录
		}
		if canonDir(e) == want {
			return true
		}
	}
	return false
}

func sameDir(a, b string) bool { return canonDir(a) == canonDir(b) }

// canonDir 尽力把一个目录名规范化：先转绝对路径（PATH 里允许写相对项），
// 再解符号链接（/usr/bin 常常是 /bin 的软链）。解不动就退回纯字符串比较。
func canonDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// writeFileAtomic 同目录临时文件 + rename，避免写到一半断电留下半个清单。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功之后这一下是空操作
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	// rename 只保证元数据原子，不保证内容已经落盘 —— 少了这一下，断电后
	// 可能留下一个长度为 0 的清单，而那正是这个函数说要防的事。
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
