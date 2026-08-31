// Package ref 解析用户在命令行上写的包引用。
// 规格 §2 的四条路都从这里进来。
package ref

import "errors"

type Kind int

const (
	KindIndex Kind = iota // "Derrity/tinyserve"       —— 要查索引
	KindHost              // "github:Derrity/tinyserve" —— 直接读那个托管方
	KindURL               // "https://…/toy.toml"       —— 完全裸奔
)

var ErrBadRef = errors.New("ref: 无法解析这个包引用")

type Ref struct {
	Kind    Kind
	Host    string // KindHost 才有，如 "github"
	Owner   string
	Repo    string
	URL     string // KindURL 才有：toy.toml 的直接地址
	Version string // 用户写了 @v1.2.3 才有，空表示取最新
	Raw     string // 原始输入，报错时用
}

// Parse 认这四种形态：
//
//	Derrity/tinyserve            KindIndex
//	Derrity/tinyserve@v1.2.3     KindIndex + 指定版本
//	github:Derrity/tinyserve     KindHost
//	https://…/toy.toml           KindURL
//
// owner/repo 只允许 [A-Za-z0-9._-]，不允许空段，不允许多于一个斜杠。
func Parse(s string) (Ref, error) { panic("todo") }

// ID 返回规范化的 "owner/repo"，索引里用它当 key。
func (r Ref) ID() string { panic("todo") }

// String 返回可以再被 Parse 的规范形式。
func (r Ref) String() string { panic("todo") }
