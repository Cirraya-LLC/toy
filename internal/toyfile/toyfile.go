// Package toyfile 解析并校验作者写的 toy.toml。规格 §3。
package toyfile

import "errors"

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
func Parse(data []byte) (*Toyfile, error) { panic("todo") }

// IsScript 表示这是单文件脚本形态：下载 + chmod +x + 软链，零解压零构建。
func (t *Toyfile) IsScript() bool { panic("todo") }

// Asset 按 goos/goarch 挑出资产名并展开 {version} / {tag} 占位符，
// 同时返回该平台声明的 sha256（没有就是空串）。
// 找不到当前平台返回 ErrNoPlatform，并在 error 里列出它支持哪些平台。
func (t *Toyfile) Asset(goos, goarch, version, tag string) (asset, sha256 string, err error) {
	panic("todo")
}

// Platforms 返回它声明支持的平台列表，排序稳定，报错时给用户看。
func (t *Toyfile) Platforms() []string { panic("todo") }
