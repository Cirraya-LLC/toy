package sign

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustGenerate(t *testing.T) KeyPair {
	t.Helper()
	kp, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return kp
}

func TestSignVerify(t *testing.T) {
	kp := mustGenerate(t)
	other := mustGenerate(t)
	msg := []byte("name = \"tinyserve\"\nversion = \"1.2.3\"\n")
	sig := Sign(kp.Private, msg)
	pubHex := hex.EncodeToString(kp.Public)

	cases := []struct {
		name    string
		pubHex  string
		msg     []byte
		sig     string
		wantErr error // ErrBadSignature 或 nil；其它错误只断言"非 nil"
		wantAny bool
	}{
		{name: "正常验签", pubHex: pubHex, msg: msg, sig: sig},
		{name: "公钥前后有空白也认", pubHex: " " + pubHex + "\n", msg: msg, sig: sig},
		{name: "签名前后有空白也认", pubHex: pubHex, msg: msg, sig: sig + "\n"},
		{name: "空消息", pubHex: pubHex, msg: nil, sig: Sign(kp.Private, nil)},
		{name: "消息被改", pubHex: pubHex, msg: []byte("name = \"tinyserve!\""), sig: sig, wantErr: ErrBadSignature},
		{name: "换了一把公钥", pubHex: hex.EncodeToString(other.Public), msg: msg, sig: sig, wantErr: ErrBadSignature},
		{name: "签名被改", pubHex: pubHex, msg: msg, sig: Sign(other.Private, msg), wantErr: ErrBadSignature},
		{name: "公钥不是 hex", pubHex: "zzzz", msg: msg, sig: sig, wantAny: true},
		{name: "公钥长度不对", pubHex: "a3f9", msg: msg, sig: sig, wantAny: true},
		{name: "签名不是 base64", pubHex: pubHex, msg: msg, sig: "!!!not base64!!!", wantAny: true},
		{name: "签名长度不对", pubHex: pubHex, msg: msg, sig: "YWJj", wantErr: ErrBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.pubHex, tc.msg, tc.sig)
			switch {
			case tc.wantAny:
				if err == nil {
					t.Fatal("想要一个错误，却通过了")
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("想要 %v，得到 %v", tc.wantErr, err)
				}
			default:
				if err != nil {
					t.Fatalf("应该验签通过，却得到 %v", err)
				}
			}
		})
	}
}

// 消息就是原始字节，不再自己套一层 hash —— 这条被 Verify 和标准库直接验签
// 结果一致钉住。
func TestSignsRawMessageNotAHashOfIt(t *testing.T) {
	kp := mustGenerate(t)
	msg := []byte("原始字节")
	if err := Verify(hex.EncodeToString(kp.Public), msg, Sign(kp.Private, msg)); err != nil {
		t.Fatalf("自签自验失败: %v", err)
	}
	if !ed25519.Verify(kp.Public, msg, ed25519.Sign(kp.Private, msg)) {
		t.Fatal("标准库对同一条原始消息验不过，说明 Sign 多加了一层")
	}
}

func TestFingerprint(t *testing.T) {
	kp := mustGenerate(t)
	full := hex.EncodeToString(kp.Public)

	fp := Fingerprint(kp.Public)
	if !strings.HasSuffix(fp, "…") {
		t.Fatalf("指纹要带省略号好在界面上显示，得到 %q", fp)
	}
	if got, want := strings.TrimSuffix(fp, "…"), full[:fingerprintHexLen]; got != want {
		t.Fatalf("指纹前缀 = %q，想要 %q", got, want)
	}
	if Fingerprint(nil) == "" {
		t.Fatal("没有公钥时也要给一句能显示的话，不能是空串")
	}
	// 铁律 8：长度不对的东西不是公钥，不许渲染成一个像样的指纹。
	// 界面上打 "key a3…" 会被读成"这个包有签名者"，而它连验签都过不去。
	for _, bad := range []ed25519.PublicKey{{0xa3}, make(ed25519.PublicKey, 31), make(ed25519.PublicKey, 33)} {
		got := Fingerprint(bad)
		if strings.HasSuffix(got, "…") || len(bad) == 1 && got == "a3" {
			t.Fatalf("%d 字节的公钥被渲染成了 %q，看起来像个正常指纹", len(bad), got)
		}
		if !strings.Contains(got, "invalid") {
			t.Fatalf("%d 字节的公钥应该明说无效，得到 %q", len(bad), got)
		}
	}
}

func TestKeyPairRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys") // 不存在，SaveKeyPair 要自己建
	kp := mustGenerate(t)

	if err := SaveKeyPair(dir, kp); err != nil {
		t.Fatalf("SaveKeyPair: %v", err)
	}
	for name, want := range map[string]os.FileMode{pubFile: 0o644, secFile: 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s 权限 = %04o，想要 %04o", name, got, want)
		}
	}

	got, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if !got.Public.Equal(kp.Public) || !got.Private.Equal(kp.Private) {
		t.Fatal("读回来的密钥对和写进去的不一样")
	}
	// 公钥文件是 hex 文本，用户能直接 cat 出来贴进索引条目。
	raw, err := os.ReadFile(filepath.Join(dir, pubFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != hex.EncodeToString(kp.Public) {
		t.Fatalf("key.pub 应该是公钥的 hex，得到 %q", raw)
	}
}

func TestSaveKeyPairRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	first := mustGenerate(t)
	if err := SaveKeyPair(dir, first); err != nil {
		t.Fatalf("第一次保存: %v", err)
	}
	if err := SaveKeyPair(dir, mustGenerate(t)); err == nil {
		t.Fatal("已有密钥时必须拒绝覆盖")
	}
	got, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if !got.Private.Equal(first.Private) {
		t.Fatal("原来的私钥被覆盖了")
	}
}

func TestLoadKeyPairErrors(t *testing.T) {
	// 私钥权限不是 0600 就拒绝加载，跟 ssh 一样。
	t.Run("私钥权限太松", func(t *testing.T) {
		dir := t.TempDir()
		if err := SaveKeyPair(dir, mustGenerate(t)); err != nil {
			t.Fatal(err)
		}
		secPath := filepath.Join(dir, secFile)
		if err := os.Chmod(secPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, e := LoadKeyPair(dir); e == nil {
			t.Fatal("0644 的私钥必须被拒绝")
		} else if !strings.Contains(e.Error(), "chmod 600") {
			t.Fatalf("报错要告诉用户怎么修，得到 %v", e)
		}
	})

	t.Run("完全没有密钥", func(t *testing.T) {
		if _, err := LoadKeyPair(t.TempDir()); !errors.Is(err, ErrNoKey) {
			t.Fatalf("想要 ErrNoKey，得到 %v", err)
		}
	})

	t.Run("只有私钥缺公钥", func(t *testing.T) {
		dir := t.TempDir()
		kp := mustGenerate(t)
		if err := SaveKeyPair(dir, kp); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, pubFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("缺公钥必须报错")
		}
	})

	t.Run("私钥不是 hex", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte("这不是十六进制"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("坏私钥必须报错")
		}
	})

	t.Run("私钥长度不对", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte("a3f9"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("长度不对的私钥必须报错")
		}
	})

	t.Run("公钥私钥不是一对", func(t *testing.T) {
		dir := t.TempDir()
		a, b := mustGenerate(t), mustGenerate(t)
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte(hex.EncodeToString(a.Private)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pubFile), []byte(hex.EncodeToString(b.Public)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("两个文件不是一对时必须报错")
		}
	})
}

func TestSaveKeyPairRejectsBadKey(t *testing.T) {
	if err := SaveKeyPair(t.TempDir(), KeyPair{}); err == nil {
		t.Fatal("空密钥对不该被写进磁盘")
	}
}

// 钥匙环的核心行为：首次见到不算数、之后必须一致、变了要连旧值一起报。
func TestRingTOFU(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "known") // 目录也不存在
	r, err := OpenRing(dir)
	if err != nil {
		t.Fatalf("OpenRing: %v", err)
	}

	const id = "github:Derrity/tinyserve@v1.2.3"
	const first = "a3f9c1d2"
	const second = "deadbeef"

	// 文件都还不存在，要能容忍。
	tr, old, err := r.Check(id, "sha256", first)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if tr != TrustNew || old != "" {
		t.Fatalf("头一次见应该是 (TrustNew, \"\")，得到 (%v, %q)", tr, old)
	}
	// Check 绝不自动钉住 —— 调用方装成功之后才 Pin。
	if _, err := os.Stat(filepath.Join(dir, "sha256.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Check 不许写文件，钉住是 Pin 的事")
	}

	if err := r.Pin(id, "sha256", first); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	cases := []struct {
		name     string
		id, kind string
		value    string
		want     Trust
		wantOld  string
	}{
		{name: "一致", id: id, kind: "sha256", value: first, want: TrustMatch, wantOld: first},
		{name: "变了要带旧值回来", id: id, kind: "sha256", value: second, want: TrustChanged, wantOld: first},
		{name: "另一个 id 还是新的", id: "github:x/y", kind: "sha256", value: first, want: TrustNew},
		{name: "另一个 kind 各记各的", id: id, kind: "key", value: first, want: TrustNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, old, err := r.Check(tc.id, tc.kind, tc.value)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != tc.want || old != tc.wantOld {
				t.Fatalf("Check = (%v, %q)，想要 (%v, %q)", got, old, tc.want, tc.wantOld)
			}
		})
	}

	// Pin 覆盖（调用方应当已经拿到 --force）。
	if err := r.Pin(id, "sha256", second); err != nil {
		t.Fatalf("Pin 覆盖: %v", err)
	}
	if got, old, _ := r.Check(id, "sha256", second); got != TrustMatch || old != second {
		t.Fatalf("覆盖后应该 TrustMatch/%q，得到 %v/%q", second, got, old)
	}
	// 覆盖不许留下重复行。
	raw, err := os.ReadFile(filepath.Join(dir, "sha256.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), id+" "); n != 1 {
		t.Fatalf("%s 在文件里出现了 %d 次，应该只有 1 次:\n%s", id, n, raw)
	}
	if info, _ := os.Stat(filepath.Join(dir, "sha256.txt")); info.Mode().Perm() != 0o644 {
		t.Fatalf("钥匙环文件权限 = %04o，想要 0644", info.Mode().Perm())
	}
}

// id 里有 : 和 /，所以存取必须按第一个空格切。
func TestRingSplitsOnFirstSpaceOnly(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"github:Derrity/tinyserve",
		"gitlab:group/sub/proj@v2.0.0",
		"https://example.com/a/b/toy.toml",
	}
	for i, id := range ids {
		if err := r.Pin(id, "key", hex.EncodeToString([]byte{byte(i)})); err != nil {
			t.Fatalf("Pin %s: %v", id, err)
		}
	}
	for i, id := range ids {
		got, old, err := r.Check(id, "key", hex.EncodeToString([]byte{byte(i)}))
		if err != nil {
			t.Fatalf("Check %s: %v", id, err)
		}
		if got != TrustMatch {
			t.Fatalf("%s 应该 TrustMatch，得到 %v（旧值 %q）", id, got, old)
		}
	}
}

func TestRingToleratesJunkAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		"# 这是注释",
		"",
		"   ",
		"孤零零一个没有值的 id",
		"github:a/b abc123",
		"  github:c/d   def456  ", // 前后空白和多余空格
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "sha256.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _, err := r.Check("github:a/b", "sha256", "abc123"); err != nil || got != TrustMatch {
		t.Fatalf("Check = %v, %v", got, err)
	}
	if got, old, err := r.Check("github:c/d", "sha256", "def456"); err != nil || got != TrustMatch {
		t.Fatalf("多余空格那行没读对: %v, %q, %v", got, old, err)
	}
	// 从来没写过的 kind 一样要能读。
	if got, _, err := r.Check("github:a/b", "key", "x"); err != nil || got != TrustNew {
		t.Fatalf("文件不存在时应该 TrustNew，得到 %v, %v", got, err)
	}
}

// kind 直接当文件名用，所以它必须是安全名字而不是一段路径。
func TestRingRejectsBadInput(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, id, kind, value string }{
		{"空 id", "", "key", "v"},
		{"空 value", "github:a/b", "key", ""},
		{"空 kind", "github:a/b", "", "v"},
		{"kind 路径穿越", "github:a/b", "../../etc/passwd", "v"},
		{"kind 带斜杠", "github:a/b", "a/b", "v"},
		{"id 带空格", "github:a/b c", "key", "v"},
		{"id 带换行", "github:a/b\nevil x", "key", "v"},
		{"value 带空格", "github:a/b", "key", "v v"},
		{"id 以 # 开头", "#github:a/b", "key", "v"}, // load 会把它当注释吃掉
		{"id 就是 #", "#", "key", "v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Pin(tc.id, tc.kind, tc.value); err == nil {
				t.Fatal("Pin 应该拒绝")
			}
			if _, _, err := r.Check(tc.id, tc.kind, tc.value); err == nil {
				t.Fatal("Check 应该拒绝")
			}
		})
	}
	if _, err := OpenRing(""); err == nil {
		t.Fatal("空目录名应该被拒绝")
	}
}

func TestRingPinIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.Pin("github:a/b", "key", "abc"); err != nil {
			t.Fatalf("Pin 第 %d 次: %v", i, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "github:a/b abc\n" {
		t.Fatalf("重复 Pin 同一个值不该改动文件，得到 %q", raw)
	}
	// 不该留下临时文件（key.txt.lock 是 Pin 的锁，长期存在是对的）。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "key.txt" && e.Name() != "key.txt.lock" {
			t.Fatalf("留下了临时文件 %s", e.Name())
		}
	}
}

func TestTrustString(t *testing.T) {
	for tr, want := range map[Trust]string{TrustNew: "new", TrustMatch: "match", TrustChanged: "changed"} {
		if got := tr.String(); got != want {
			t.Fatalf("Trust(%d).String() = %q，想要 %q", int(tr), got, want)
		}
	}
}

// Pin 说成功就必须钉得住。这条不变量比任何单个用例都重要：一条 Pin 成功、
// Check 却读不回来的记录，等于这个包的 TOFU 被静默关掉了 —— 每次安装都是
// "首次，已钉住"，artifact 换了也不会有人告诉用户。
func TestPinnedValuesAlwaysComeBack(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"github:Derrity/tinyserve",
		"gitlab:group/sub/proj@v2.0.0",
		"https://example.com/a/b/toy.toml",
		"https://example.com/a#frag",
		"#github:evil/pkg", // 会被当注释吃掉，必须在 Pin 就被拒
		"#",
		"--force",
		"..",
		"../../etc/passwd",
		strings.Repeat("a", 4096),
		"a\x00b",
		"名字里有中文",
	}
	for _, id := range ids {
		if err := r.Pin(id, "sha256", "cafebabe"); err != nil {
			continue // 拒绝是可以接受的答案，静默丢失不是
		}
		got, old, err := r.Check(id, "sha256", "cafebabe")
		if err != nil {
			t.Fatalf("Pin %q 成功但 Check 报错: %v", id, err)
		}
		if got != TrustMatch {
			t.Fatalf("Pin %q 成功，Check 却是 %v（旧值 %q）—— 钉住记录静默丢了", id, got, old)
		}
	}
}

// 并发 Pin 不许丢记录。丢一条 = 那个包下次是 TrustNew（放行并重新钉住）
// 而不是 TrustMatch，也就是 TOFU 被静默降级。修之前 40 个并发只剩 16 条。
func TestConcurrentPinKeepsEveryEntry(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.Pin(fmt.Sprintf("github:p/%d", i), "sha256", fmt.Sprintf("%064x", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Pin %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		got, _, err := r.Check(fmt.Sprintf("github:p/%d", i), "sha256", fmt.Sprintf("%064x", i))
		if err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
		if got != TrustMatch {
			t.Fatalf("并发写之后 github:p/%d = %v，钉住记录丢了", i, got)
		}
	}
}

// Check 出错时的失败方向必须是"拒绝"。零值 TrustNew 是"放行并钉住"那一档，
// 丢掉 error 的调用方拿到它就等于静默降级成一次新的 TOFU。
func TestCheckFailsClosed(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 坏输入是唯一不依赖文件系统权限就能触发的出错路径。
	for _, tc := range []struct{ id, kind, value string }{
		{"", "sha256", "v"},
		{"github:a/b", "../etc", "v"},
		{"github:a/b", "sha256", ""},
	} {
		got, _, err := r.Check(tc.id, tc.kind, tc.value)
		if err == nil {
			t.Fatalf("Check(%q,%q,%q) 应该报错", tc.id, tc.kind, tc.value)
		}
		if got == TrustNew {
			t.Fatalf("Check 出错时返回了 TrustNew（放行那一档），应该是 TrustChanged")
		}
	}
	// 钥匙环整个不可读时同理。root 下 chmod 拦不住，所以用目录顶掉文件。
	dir := t.TempDir()
	r2, err := OpenRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sha256.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, _, err := r2.Check("github:a/b", "sha256", "v")
	if err == nil {
		t.Fatal("读不了钥匙环必须报错，不能当成没钉过")
	}
	if got == TrustNew {
		t.Fatalf("读不了钥匙环时返回了 TrustNew（放行），应该是 TrustChanged")
	}
}

// "已存在时拒绝覆盖" 必须是 O_EXCL 保证的，不能只靠 Stat —— Stat 和写之间
// 的窗口里，另一个 toy keygen 建出来的私钥会被 O_TRUNC 抹掉。
func TestSaveKeyPairNeverTruncatesAnExistingSecret(t *testing.T) {
	dir := t.TempDir()
	// 私钥先在（Stat 会看到），公钥不在 —— 走到写公钥那一步之前就该停。
	secPath := filepath.Join(dir, secFile)
	const sentinel = "别人的私钥"
	if err := os.WriteFile(secPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveKeyPair(dir, mustGenerate(t)); err == nil {
		t.Fatal("私钥已存在时必须拒绝")
	}
	raw, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatalf("已有私钥被动过了: %q", raw)
	}

	// 绕过 Stat 那一圈，直接确认底层用的是 O_EXCL 而不是 O_TRUNC。
	if err := writeNew(secPath, "新的", 0o600); err == nil {
		t.Fatal("writeNew 必须用 O_EXCL，不能覆盖已有文件")
	}
	if raw, _ := os.ReadFile(secPath); string(raw) != sentinel {
		t.Fatalf("writeNew 截断了已有文件: %q", raw)
	}
}

// Sign 的消息边界：不同消息、不同私钥签出来的东西不能撞。
func TestSignIsBoundToKeyAndMessage(t *testing.T) {
	a, b := mustGenerate(t), mustGenerate(t)
	msg := []byte("name = \"tinyserve\"")
	if Sign(a.Private, msg) == Sign(b.Private, msg) {
		t.Fatal("两把不同的私钥签出了同一个签名")
	}
	if Sign(a.Private, msg) == Sign(a.Private, append(msg, '!')) {
		t.Fatal("两条不同的消息签出了同一个签名")
	}
	// 拿 a 的签名配 b 的公钥必须被拒。
	if err := Verify(hex.EncodeToString(b.Public), msg, Sign(a.Private, msg)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("换一把公钥应该 ErrBadSignature，得到 %v", err)
	}
}
