// Package store 管 ~/.toy 的一切：目录布局、config.toml、安装清单、软链。规格 §6。
package store

import (
	"errors"

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

// DefaultConfig 返回内置默认：一个默认索引、空镜像列表。
func DefaultConfig() Config { panic("todo") }

type Store struct{ Root string }

// Open 打开（必要时创建）store。root 为空时用 ~/.toy，
// 受 TOY_HOME 环境变量覆盖（测试要靠它）。
func Open(root string) (*Store, error) { panic("todo") }

func (s *Store) BinDir() string   { panic("todo") }
func (s *Store) CacheDir() string { panic("todo") }
func (s *Store) KeysDir() string  { panic("todo") }
func (s *Store) KnownDir() string { panic("todo") }
func (s *Store) IndexDir() string { panic("todo") }

// PkgDir 是 pkg/<host>/<owner>/<name>/<version>/
func (s *Store) PkgDir(hostName, owner, name, version string) string { panic("todo") }

func (s *Store) LoadConfig() (Config, error) { panic("todo") }
func (s *Store) SaveConfig(c Config) error   { panic("todo") }

// Manifest 是一次安装的完整记录。没有它就没法 uninstall 和 update。
type Manifest struct {
	ID          string   `toml:"id"` // "github:Derrity/tinyserve"
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

func (s *Store) SaveManifest(m Manifest) error            { panic("todo") }
func (s *Store) LoadManifest(id string) (Manifest, error) { panic("todo") }
func (s *Store) ListManifests() ([]Manifest, error)       { panic("todo") }
func (s *Store) RemoveManifest(id string) error           { panic("todo") }

// FindByName 按命令名或包名找已装的包，用于 `toy uninstall tinyserve`。
// 命中多个时返回 error 并列出候选，让用户写全 id。
func (s *Store) FindByName(name string) (Manifest, error) { panic("todo") }

// Link 在 BinDir 里建一个指向 target 的软链。
// 铁律 5：如果 name 已经存在于 PATH 上（且不是我们自己 BinDir 里的），
// 返回 ErrShadow，除非 force。这一条堵掉 bin = "sudo" 这类提权。
func (s *Store) Link(name, target string, force bool) (string, error) { panic("todo") }

func (s *Store) Unlink(path string) error { panic("todo") }

// InPath 报告 BinDir 是否已经在用户的 PATH 里，toy doctor 用。
func (s *Store) InPath() bool { panic("todo") }
