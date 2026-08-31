// Package sign 是 ed25519 签名和 TOFU 钥匙环。规格 §4。
// 签名是可选升级不是入场券 —— 强制签名等于强制注册，那就是又一个门槛。
package sign

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

var (
	ErrBadSignature = errors.New("sign: 签名验证失败")
	ErrChanged      = errors.New("sign: 与首次安装时钉住的值不一致")
	ErrNoKey        = errors.New("sign: 本机还没有密钥对，先跑 toy keygen")
)

// 密钥文件名。公钥和私钥都存 hex 文本，因为 Verify 收的就是 hex 公钥，
// 用户从 key.pub 里 cat 出来的东西可以直接贴进索引条目的 "key" 字段。
const (
	pubFile = "key.pub"
	secFile = "key.sec"
)

type KeyPair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

func Generate() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sign: 生成密钥对失败: %w", err)
	}
	return KeyPair{Public: pub, Private: priv}, nil
}

// LoadKeyPair 从 dir 读 key.pub / key.sec。私钥不能被同组或其他人读写，
// 不是就拒绝加载并说明原因。
func LoadKeyPair(dir string) (KeyPair, error) {
	secPath := filepath.Join(dir, secFile)
	info, err := os.Stat(secPath)
	if errors.Is(err, os.ErrNotExist) {
		return KeyPair{}, ErrNoKey
	}
	if err != nil {
		return KeyPair{}, fmt.Errorf("sign: 读不到 %s: %w", secPath, err)
	}
	// 跟 ssh 一样：私钥对别人可读就拒绝加载，并且直接告诉他怎么修。
	// 判据是"group/other 一位都不能有"，不是"恰好 0600"：0400 比 0600 更严
	// （备份恢复私钥之后常见就是这个权限），拒绝它没有任何安全收益，而那句
	// "修复：chmod 600" 是在教用户把一个只读的私钥改成可写。
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return KeyPair{}, fmt.Errorf(
			"sign: 私钥 %s 的权限是 %04o，同组或其他人能读到它。修复：chmod 600 %s",
			secPath, perm, secPath)
	}

	priv, err := readHexFile(secPath, ed25519.PrivateKeySize, "私钥")
	if err != nil {
		return KeyPair{}, err
	}
	pubPath := filepath.Join(dir, pubFile)
	pub, err := readHexFile(pubPath, ed25519.PublicKeySize, "公钥")
	if errors.Is(err, os.ErrNotExist) {
		return KeyPair{}, fmt.Errorf("sign: 有私钥但缺公钥 %s，删掉 %s 重新跑 toy keygen", pubPath, secFile)
	}
	if err != nil {
		return KeyPair{}, err
	}

	kp := KeyPair{Public: ed25519.PublicKey(pub), Private: ed25519.PrivateKey(priv)}
	// 私钥后 32 字节就是公钥，对不上说明两个文件不是一对。
	if !bytes.Equal(kp.Private.Public().(ed25519.PublicKey), kp.Public) {
		return KeyPair{}, fmt.Errorf("sign: %s 和 %s 不是同一对密钥，删掉它们重新跑 toy keygen", pubFile, secFile)
	}
	return kp, nil
}

func readHexFile(path string, want int, what string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err // 调用方要用 errors.Is 判 ErrNotExist，别包一层丢掉。
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("sign: %s %s 不是合法的十六进制: %w", what, path, err)
	}
	if len(b) != want {
		return nil, fmt.Errorf("sign: %s %s 长度是 %d 字节，应该是 %d", what, path, len(b), want)
	}
	return b, nil
}

// SaveKeyPair 写 key.pub(0644) 和 key.sec(0600)。已存在时拒绝覆盖。
func SaveKeyPair(dir string, kp KeyPair) error {
	if len(kp.Public) != ed25519.PublicKeySize || len(kp.Private) != ed25519.PrivateKeySize {
		return errors.New("sign: 密钥对长度不对，不能保存")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sign: 建目录 %s 失败: %w", dir, err)
	}
	pubPath := filepath.Join(dir, pubFile)
	secPath := filepath.Join(dir, secFile)
	for _, p := range []string{pubPath, secPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("sign: %s 已经存在，不覆盖。要换新密钥就先自己把它挪走或删掉", p)
		}
	}
	// 先写私钥：公钥先落地而私钥失败会留下一个没用的半套。
	if err := writeNew(secPath, hex.EncodeToString(kp.Private)+"\n", 0o600); err != nil {
		return fmt.Errorf("sign: 写私钥 %s 失败: %w", secPath, err)
	}
	if err := writeNew(pubPath, hex.EncodeToString(kp.Public)+"\n", 0o644); err != nil {
		return fmt.Errorf("sign: 写公钥 %s 失败: %w", pubPath, err)
	}
	return nil
}

// writeNew 建一个新文件并落盘。用 O_EXCL 而不是 os.WriteFile（O_TRUNC）：
// 上面那圈 Stat 只负责给一句好报错，Stat 和写之间有窗口，真正让"拒绝覆盖"
// 成立的是 O_EXCL。fsync 是因为丢了私钥就等于丢了作者身份 —— 同一把钥匙
// 签过的东西再也签不出来。
func writeNew(path, content string, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fingerprintHexLen 是指纹取的十六进制字符数。规格里显示成 "a3f9…"，那是文档
// 自己的省略。指纹是作者身份在界面上唯一露出的东西（toy keygen 就是这么告诉
// 作者的："别人安装时打印的就是这一串"），所以它要挡的是**原像**：攻击者反复
// 生成 ed25519 密钥，直到前缀和真作者的指纹一样，然后拿这把钥匙去索引里冒名。
// ed25519 keygen 每核每秒几万次，所以 32 位（8 个 hex）只值几十个 core-hour。
// 32 个 hex = 128 位，磨不出来。
const fingerprintHexLen = 32

// Fingerprint 返回公钥的短指纹，界面上显示成 "a3f9…"。
// 铁律 8（信任等级如实打印）：长度不对的东西根本不是 ed25519 公钥，绝不能
// 渲染成一个看起来正常的指纹 —— 界面上一句 "key a3…" 会被读成"这个包有签名者"。
func Fingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return "(无公钥)"
	}
	if len(pub) != ed25519.PublicKeySize {
		return "(公钥无效)"
	}
	return hex.EncodeToString(pub)[:fingerprintHexLen] + "…"
}

// Sign 返回 base64 标准编码的签名。消息就是原始字节，这里不再套一层 hash
// —— ed25519 内部已经做了。
// 私钥长度不对时会 panic（标准库的行为，这个签名没有 error 可返回）。
// 这是故意不接住的：返回一个空签名会让调用方把一条验不过的条目发出去，
// 而失败要等到别人安装的时候才暴露。私钥只可能来自 Generate/LoadKeyPair，
// 两者都保证了长度。
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// Verify 用 hex 编码的公钥验签。
func Verify(pubHex string, msg []byte, sigB64 string) error {
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil {
		return fmt.Errorf("sign: 公钥不是合法的十六进制: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("sign: 公钥长度是 %d 字节，应该是 %d", len(pub), ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("sign: 签名不是合法的 base64: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// Trust 是 TOFU 的三种结果。
type Trust int

const (
	TrustNew     Trust = iota // 头一次见，已钉住
	TrustMatch                // 和钉住的一致
	TrustChanged              // 变了 —— 必须大声警告并拒绝，除非 --force
)

func (t Trust) String() string {
	switch t {
	case TrustNew:
		return "new"
	case TrustMatch:
		return "match"
	case TrustChanged:
		return "changed"
	}
	return fmt.Sprintf("Trust(%d)", int(t))
}

// Ring 是 ~/.toy/known/ 下的 TOFU 记录，模型抄 SSH 的 known_hosts。
// 一个 kind 一个文件 <kind>.txt，一行一条 "<id> <value>"。
// id 里可能有 : 和 /，所以按第一个空格切。
type Ring struct{ Dir string }

func OpenRing(dir string) (*Ring, error) {
	if dir == "" {
		return nil, errors.New("sign: 钥匙环目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sign: 建钥匙环目录 %s 失败: %w", dir, err)
	}
	return &Ring{Dir: dir}, nil
}

// Check 查 id 在 kind 维度上钉住的值。kind 取 "key" 或 "sha256"。
// TrustNew 时不自动钉住 —— 调用方在真的安装成功之后才调 Pin。
// 第二个返回值是钉住的旧值：TrustChanged 时调用方要把它打给用户看。
// error 只表示读文件出了问题；"值变了" 是 TrustChanged 而不是错误。
// 出错时返回 TrustChanged 而不是零值 TrustNew：TrustNew 是"放行并钉住"那一档，
// 而钥匙环读不出来恰恰是最不该放行的时候。丢掉 error 的调用方会撞上拒绝安装，
// 不会静默降级成一次新的 TOFU。
func (r *Ring) Check(id, kind, value string) (Trust, string, error) {
	if err := validEntry(id, kind, value); err != nil {
		return TrustChanged, "", err
	}
	pinned, err := r.load(kind)
	if err != nil {
		return TrustChanged, "", err
	}
	old, ok := pinned[id]
	switch {
	case !ok:
		return TrustNew, "", nil
	case old == value:
		return TrustMatch, old, nil
	default:
		return TrustChanged, old, nil
	}
}

// Pin 记下 id 的值。已有不同值时覆盖（调用方应当已经拿到用户的 --force）。
func (r *Ring) Pin(id, kind, value string) error {
	if err := validEntry(id, kind, value); err != nil {
		return err
	}
	// 整个读改写必须在锁里：不加锁时两个并发的 toy install 会各自读到旧内容、
	// 各自整份写回，后写的把先写的那条钉住记录抹掉 —— 而丢掉一条钉住记录
	// 意味着那个包下次安装是 TrustNew（放行并重新钉住）而不是 TrustMatch，
	// 也就是 TOFU 被静默降级。实测 40 个并发只剩 16 条。
	return r.withLock(kind, func() error {
		pinned, err := r.load(kind)
		if err != nil {
			return err
		}
		if old, ok := pinned[id]; ok && old == value {
			return nil
		}
		pinned[id] = value
		return r.save(kind, pinned)
	})
}

// withLock 在 <kind>.txt.lock 上拿排他锁。锁在单独的文件上而不是数据文件上，
// 因为 save 会 rename 覆盖数据文件，锁会留在被换掉的那个 inode 上。
// flock 由内核在进程死掉时释放，所以不会留下卡死的锁文件。
// 只有 Pin 需要它：Check 只读，而 rename 保证读到的要么是旧文件要么是新文件。
// syscall.Flock 让这个包只能在 unix 上编译 —— 规格 §11 明说 v1 只有 Linux + macOS，
// 所以不为 Windows 加一对 build tag 文件。真要上 Windows 时在这里分平台。
func (r *Ring) withLock(kind string, fn func() error) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("sign: 建钥匙环目录 %s 失败: %w", r.Dir, err)
	}
	lockPath := r.path(kind) + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("sign: 打不开钥匙环锁 %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("sign: 锁不住钥匙环 %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// validEntry 挡住会毁掉这个文件格式的输入。kind 直接当文件名用，
// 所以它必须是个安全的名字而不是一段路径。
func validEntry(id, kind, value string) error {
	if id == "" {
		return errors.New("sign: id 不能为空")
	}
	if value == "" {
		return errors.New("sign: 要钉住的值不能为空")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return fmt.Errorf("sign: id %q 里不能有空白字符", id)
	}
	// load 把 # 开头的行当注释跳过，所以这种 id 会 Pin 成功、Check 永远读不回来
	// —— 一条永远钉不住的记录等于这个包的 TOFU 被静默关掉了。拒绝比丢掉好。
	if strings.HasPrefix(id, "#") {
		return fmt.Errorf("sign: id %q 不能以 # 开头（那是钥匙环的注释符）", id)
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("sign: 值 %q 里不能有空白字符", value)
	}
	if kind == "" {
		return errors.New("sign: kind 不能为空（用 \"key\" 或 \"sha256\"）")
	}
	for _, c := range kind {
		ok := c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("sign: kind %q 只能是小写字母、数字、- 和 _", kind)
		}
	}
	return nil
}

func (r *Ring) path(kind string) string { return filepath.Join(r.Dir, kind+".txt") }

// load 读一个 kind 的全部记录。文件不存在就是"还没钉过任何东西"，不是错误。
func (r *Ring) load(kind string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(r.path(kind))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sign: 读钥匙环 %s 失败: %w", r.path(kind), err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// id 里有 : 和 /，所以只按第一个空格切。
		id, value, ok := strings.Cut(line, " ")
		if !ok || id == "" || strings.TrimSpace(value) == "" {
			continue // 半行垃圾就跳过，不要让一行坏数据把整个钥匙环变成错误。
		}
		out[id] = strings.TrimSpace(value)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sign: 读钥匙环 %s 失败: %w", r.path(kind), err)
	}
	return out, nil
}

func (r *Ring) save(kind string, pinned map[string]string) error {
	ids := make([]string, 0, len(pinned))
	for id := range pinned {
		ids = append(ids, id)
	}
	slices.Sort(ids) // 只为文件内容稳定、好 diff。

	var buf bytes.Buffer
	for _, id := range ids {
		fmt.Fprintf(&buf, "%s %s\n", id, pinned[id])
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("sign: 建钥匙环目录 %s 失败: %w", r.Dir, err)
	}
	// 先写临时文件再 rename：中途挂掉不会留下一个被截断的钥匙环。
	tmp, err := os.CreateTemp(r.Dir, kind+".txt.*")
	if err != nil {
		return fmt.Errorf("sign: 写钥匙环失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功之后这一下是空操作。
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: 写钥匙环失败: %w", err)
	}
	// 没有这一下，rename 的原子性只保证"要么旧文件要么新文件名"，不保证新文件
	// 里有内容：断电后常见的结果是一个长度为 0 的钥匙环，也就是全部钉住记录
	// 静默消失、每个包都退回第一次安装。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: 钥匙环落盘失败: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: 设置钥匙环权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sign: 写钥匙环失败: %w", err)
	}
	if err := os.Rename(tmpName, r.path(kind)); err != nil {
		return fmt.Errorf("sign: 写钥匙环 %s 失败: %w", r.path(kind), err)
	}
	return nil
}
