package sign

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fingerprint is the only place an author identity surfaces in the UI (toy keygen
// says it outright: "this is the string other people see when they install",
// author.go), while anyone can open a PR putting a key into the index. So what it has
// to resist is not a collision but a PREIMAGE: an attacker runs ed25519 keygen until
// the prefix equals a real author's fingerprint, then impersonates them with that key
// -- and the signature still verifies, because it is signed with his own key.
//
// Measured on this machine: roughly 36k keygens per second per core, so 32 bits
// (8 hex) extrapolates to about 33 core-hours -- a few dollars in the cloud and
// perfectly parallel. A fingerprint therefore needs at least 128 bits.
func TestFingerprintCarriesEnoughBitsToResistPreimage(t *testing.T) {
	const minBits = 128
	if bits := fingerprintHexLen * 4; bits < minBits {
		t.Fatalf("fingerprint is only %d hex = %d bits; grinding a public key with the same "+
			"prefix is a matter of tens of core-hours. It needs at least %d bits", fingerprintHexLen, bits, minBits)
	}
	kp := mustGenerate(t)
	if got, want := Fingerprint(kp.Public), hex.EncodeToString(kp.Public)[:fingerprintHexLen]+"…"; got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

// The private key permission test is "not one group/other bit", not "exactly 0600".
// 0400 is stricter than 0600 (and is what you commonly get after restoring a key from
// a backup); refusing it buys no security, and the old "fix: chmod 600" line was
// teaching the user to make a read-only private key writable.
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
			t.Fatalf("%04o is not exposed to anyone else and should be accepted, but was refused: %v", perm, err)
		}
	}
	for _, perm := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o606} {
		err := load(t, perm)
		if err == nil {
			t.Fatalf("%04o lets the group or other users read the private key and must be refused", perm)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("%04o was refused without telling the user how to fix it: %v", perm, err)
		}
	}
}
