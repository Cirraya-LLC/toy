// Package sign 是 ed25519 签名和 TOFU 钥匙环。规格 §4。
// 签名是可选升级不是入场券 —— 强制签名等于强制注册，那就是又一个门槛。
package sign

import (
	"crypto/ed25519"
	"errors"
)

var (
	ErrBadSignature = errors.New("sign: 签名验证失败")
	ErrChanged      = errors.New("sign: 与首次安装时钉住的值不一致")
	ErrNoKey        = errors.New("sign: 本机还没有密钥对，先跑 toy keygen")
)

type KeyPair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

func Generate() (KeyPair, error) { panic("todo") }

// LoadKeyPair 从 dir 读 key.pub / key.sec。私钥文件权限必须是 0600，
// 不是就拒绝加载并说明原因。
func LoadKeyPair(dir string) (KeyPair, error) { panic("todo") }

// SaveKeyPair 写 key.pub(0644) 和 key.sec(0600)。已存在时拒绝覆盖。
func SaveKeyPair(dir string, kp KeyPair) error { panic("todo") }

// Fingerprint 返回公钥的短指纹，界面上显示成 "a3f9…"。
func Fingerprint(pub ed25519.PublicKey) string { panic("todo") }

// Sign 返回 base64 标准编码的签名。
func Sign(priv ed25519.PrivateKey, msg []byte) string { panic("todo") }

// Verify 用 hex 编码的公钥验签。
func Verify(pubHex string, msg []byte, sigB64 string) error { panic("todo") }

// Trust 是 TOFU 的三种结果。
type Trust int

const (
	TrustNew     Trust = iota // 头一次见，已钉住
	TrustMatch                // 和钉住的一致
	TrustChanged              // 变了 —— 必须大声警告并拒绝，除非 --force
)

// Ring 是 ~/.toy/known/ 下的 TOFU 记录，模型抄 SSH 的 known_hosts。
type Ring struct{ Dir string }

func OpenRing(dir string) (*Ring, error) { panic("todo") }

// Check 查 id 在 kind 维度上钉住的值。kind 取 "key" 或 "sha256"。
// TrustNew 时不自动钉住 —— 调用方在真的安装成功之后才调 Pin。
func (r *Ring) Check(id, kind, value string) (Trust, string, error) { panic("todo") }

// Pin 记下 id 的值。已有不同值时覆盖（调用方应当已经拿到用户的 --force）。
func (r *Ring) Pin(id, kind, value string) error { panic("todo") }
