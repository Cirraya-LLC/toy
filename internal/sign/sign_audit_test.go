package sign

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 指纹是作者身份在界面上唯一露出的东西（toy keygen 原话："别人安装时打印的就是
// 这一串"，author.go），而索引里的 key 谁都能提 PR 写进去。所以它要挡的不是碰撞
// 是**原像**：攻击者反复 ed25519 keygen 直到前缀等于真作者的指纹，再拿这把钥匙
// 冒名 —— 签名照样验得过，因为签的是他自己的钥匙。
//
// 实测这台机器约 3.6 万次 keygen/秒/核，32 位（8 个 hex）外推约 33 core-hour，
// 云上几美元且完全可并行。所以指纹至少要有 128 位。
func TestFingerprintCarriesEnoughBitsToResistPreimage(t *testing.T) {
	const minBits = 128
	if bits := fingerprintHexLen * 4; bits < minBits {
		t.Fatalf("指纹只有 %d 个 hex = %d 位，磨一把前缀相同的公钥是几十 core-hour 的事；"+
			"至少要 %d 位", fingerprintHexLen, bits, minBits)
	}
	kp := mustGenerate(t)
	if got, want := Fingerprint(kp.Public), hex.EncodeToString(kp.Public)[:fingerprintHexLen]+"…"; got != want {
		t.Fatalf("指纹 = %q，想要 %q", got, want)
	}
}

// 私钥权限的判据是「group/other 一位都不能有」，不是「恰好 0600」。
// 0400 比 0600 更严（备份恢复私钥之后常见就是这个权限），拒绝它没有安全收益，
// 而原来那句 "修复：chmod 600" 是在教用户把只读的私钥改成可写。
func TestAuditKeyPermCheckRejectsStricterModes(t *testing.T) {
	load := func(t *testing.T, perm os.FileMode) error {
		t.Helper()
		dir := t.TempDir()
		kp, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveKeyPair(dir, kp); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, secFile), perm); err != nil {
			t.Fatal(err)
		}
		_, err = LoadKeyPair(dir)
		return err
	}

	for _, perm := range []os.FileMode{0o400, 0o600} {
		if err := load(t, perm); err != nil {
			t.Fatalf("%04o 不对别人开放，应该放行，却拒了：%v", perm, err)
		}
	}
	for _, perm := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o606} {
		err := load(t, perm)
		if err == nil {
			t.Fatalf("%04o 让同组或其他人读得到私钥，必须拒绝", perm)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("%04o 被拒了但没告诉用户怎么修：%v", perm, err)
		}
	}
}
