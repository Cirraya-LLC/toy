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
		wantErr error // ErrBadSignature or nil; other errors are only asserted "non-nil"
		wantAny bool
	}{
		{name: "valid signature", pubHex: pubHex, msg: msg, sig: sig},
		{name: "public key with surrounding whitespace", pubHex: " " + pubHex + "\n", msg: msg, sig: sig},
		{name: "signature with surrounding whitespace", pubHex: pubHex, msg: msg, sig: sig + "\n"},
		{name: "empty message", pubHex: pubHex, msg: nil, sig: Sign(kp.Private, nil)},
		{name: "message tampered with", pubHex: pubHex, msg: []byte("name = \"tinyserve!\""), sig: sig, wantErr: ErrBadSignature},
		{name: "different public key", pubHex: hex.EncodeToString(other.Public), msg: msg, sig: sig, wantErr: ErrBadSignature},
		{name: "signature tampered with", pubHex: pubHex, msg: msg, sig: Sign(other.Private, msg), wantErr: ErrBadSignature},
		{name: "public key is not hex", pubHex: "zzzz", msg: msg, sig: sig, wantAny: true},
		{name: "public key has the wrong length", pubHex: "a3f9", msg: msg, sig: sig, wantAny: true},
		{name: "signature is not base64", pubHex: pubHex, msg: msg, sig: "!!!not base64!!!", wantAny: true},
		{name: "signature has the wrong length", pubHex: pubHex, msg: msg, sig: "YWJj", wantErr: ErrBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.pubHex, tc.msg, tc.sig)
			switch {
			case tc.wantAny:
				if err == nil {
					t.Fatal("wanted an error, but it passed")
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
			default:
				if err != nil {
					t.Fatalf("should have verified, got %v", err)
				}
			}
		})
	}
}

// The message is the raw bytes with no extra hash layer -- pinned by Verify and a
// direct standard-library verification agreeing.
func TestSignsRawMessageNotAHashOfIt(t *testing.T) {
	kp := mustGenerate(t)
	msg := []byte("raw bytes")
	if err := Verify(hex.EncodeToString(kp.Public), msg, Sign(kp.Private, msg)); err != nil {
		t.Fatalf("sign/verify round trip failed: %v", err)
	}
	if !ed25519.Verify(kp.Public, msg, ed25519.Sign(kp.Private, msg)) {
		t.Fatal("the standard library rejects the same raw message, so Sign added a layer")
	}
}

func TestFingerprint(t *testing.T) {
	kp := mustGenerate(t)
	full := hex.EncodeToString(kp.Public)

	fp := Fingerprint(kp.Public)
	if !strings.HasSuffix(fp, "…") {
		t.Fatalf("the fingerprint needs an ellipsis so it displays well, got %q", fp)
	}
	if got, want := strings.TrimSuffix(fp, "…"), full[:fingerprintHexLen]; got != want {
		t.Fatalf("fingerprint prefix = %q, want %q", got, want)
	}
	if Fingerprint(nil) == "" {
		t.Fatal("with no public key it still has to say something displayable, not an empty string")
	}
	// Iron rule 8: something of the wrong length is not a public key and must not render
	// as a respectable fingerprint. A "key a3…" on screen reads as "this package has a
	// signer", when it cannot even pass verification.
	for _, bad := range []ed25519.PublicKey{{0xa3}, make(ed25519.PublicKey, 31), make(ed25519.PublicKey, 33)} {
		got := Fingerprint(bad)
		if strings.HasSuffix(got, "…") || len(bad) == 1 && got == "a3" {
			t.Fatalf("a %d-byte public key rendered as %q, which looks like a normal fingerprint", len(bad), got)
		}
		if !strings.Contains(got, "invalid") {
			t.Fatalf("a %d-byte public key should say plainly that it is invalid, got %q", len(bad), got)
		}
	}
}

func TestKeyPairRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys") // does not exist; SaveKeyPair has to create it
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
			t.Fatalf("%s mode = %04o, want %04o", name, got, want)
		}
	}

	got, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if !got.Public.Equal(kp.Public) || !got.Private.Equal(kp.Private) {
		t.Fatal("the keypair read back differs from the one written")
	}
	// The public key file is hex text, so a user can cat it straight into an index entry.
	raw, err := os.ReadFile(filepath.Join(dir, pubFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != hex.EncodeToString(kp.Public) {
		t.Fatalf("key.pub should be the hex of the public key, got %q", raw)
	}
}

func TestSaveKeyPairRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	first := mustGenerate(t)
	if err := SaveKeyPair(dir, first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := SaveKeyPair(dir, mustGenerate(t)); err == nil {
		t.Fatal("must refuse to overwrite an existing key")
	}
	got, err := LoadKeyPair(dir)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if !got.Private.Equal(first.Private) {
		t.Fatal("the existing private key was overwritten")
	}
}

func TestLoadKeyPairErrors(t *testing.T) {
	// Refuse to load a private key that is not 0600, same as ssh.
	t.Run("private key permissions too loose", func(t *testing.T) {
		dir := t.TempDir()
		if err := SaveKeyPair(dir, mustGenerate(t)); err != nil {
			t.Fatal(err)
		}
		secPath := filepath.Join(dir, secFile)
		if err := os.Chmod(secPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, e := LoadKeyPair(dir); e == nil {
			t.Fatal("a 0644 private key must be refused")
		} else if !strings.Contains(e.Error(), "chmod 600") {
			t.Fatalf("the error must tell the user how to fix it, got %v", e)
		}
	})

	t.Run("no key at all", func(t *testing.T) {
		if _, err := LoadKeyPair(t.TempDir()); !errors.Is(err, ErrNoKey) {
			t.Fatalf("want ErrNoKey, got %v", err)
		}
	})

	t.Run("private key present, public key missing", func(t *testing.T) {
		dir := t.TempDir()
		kp := mustGenerate(t)
		if err := SaveKeyPair(dir, kp); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, pubFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("a missing public key must be an error")
		}
	})

	t.Run("private key is not hex", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte("this is not hex"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("a corrupt private key must be an error")
		}
	})

	t.Run("private key has the wrong length", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte("a3f9"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("a private key of the wrong length must be an error")
		}
	})

	t.Run("public and private key are not a pair", func(t *testing.T) {
		dir := t.TempDir()
		a, b := mustGenerate(t), mustGenerate(t)
		if err := os.WriteFile(filepath.Join(dir, secFile), []byte(hex.EncodeToString(a.Private)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pubFile), []byte(hex.EncodeToString(b.Public)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKeyPair(dir); err == nil {
			t.Fatal("two files that are not a pair must be an error")
		}
	})
}

func TestSaveKeyPairRejectsBadKey(t *testing.T) {
	if err := SaveKeyPair(t.TempDir(), KeyPair{}); err == nil {
		t.Fatal("an empty keypair must not be written to disk")
	}
}

// Core keyring behaviour: the first sighting does not count, later ones must match,
// and a change is reported together with the old value.
func TestRingTOFU(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "known") // the directory does not exist either
	r, err := OpenRing(dir)
	if err != nil {
		t.Fatalf("OpenRing: %v", err)
	}

	const id = "github:Derrity/tinyserve@v1.2.3"
	const first = "a3f9c1d2"
	const second = "deadbeef"

	// None of the files exist yet; that has to be tolerated.
	tr, old, err := r.Check(id, "sha256", first)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if tr != TrustNew || old != "" {
		t.Fatalf("a first sighting should be (TrustNew, \"\"), got (%v, %q)", tr, old)
	}
	// Check never pins by itself -- the caller Pins after the install succeeds.
	if _, err := os.Stat(filepath.Join(dir, "sha256.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Check must not write files; pinning is Pin's job")
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
		{name: "matches", id: id, kind: "sha256", value: first, want: TrustMatch, wantOld: first},
		{name: "a change returns the old value", id: id, kind: "sha256", value: second, want: TrustChanged, wantOld: first},
		{name: "a different id is still new", id: "github:x/y", kind: "sha256", value: first, want: TrustNew},
		{name: "a different kind is recorded separately", id: id, kind: "key", value: first, want: TrustNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, old, err := r.Check(tc.id, tc.kind, tc.value)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != tc.want || old != tc.wantOld {
				t.Fatalf("Check = (%v, %q), want (%v, %q)", got, old, tc.want, tc.wantOld)
			}
		})
	}

	// Pin overwrites (the caller is expected to already have --force).
	if err := r.Pin(id, "sha256", second); err != nil {
		t.Fatalf("Pin overwrite: %v", err)
	}
	if got, old, _ := r.Check(id, "sha256", second); got != TrustMatch || old != second {
		t.Fatalf("after overwriting expected TrustMatch/%q, got %v/%q", second, got, old)
	}
	// Overwriting must not leave a duplicate row.
	raw, err := os.ReadFile(filepath.Join(dir, "sha256.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), id+" "); n != 1 {
		t.Fatalf("%s appears %d times in the file, expected exactly 1:\n%s", id, n, raw)
	}
	if info, _ := os.Stat(filepath.Join(dir, "sha256.txt")); info.Mode().Perm() != 0o644 {
		t.Fatalf("keyring file mode = %04o, want 0644", info.Mode().Perm())
	}
}

// An id contains : and /, so storing and loading must split on the first space.
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
			t.Fatalf("%s should be TrustMatch, got %v (old value %q)", id, got, old)
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
		"# this is a comment",
		"",
		"   ",
		"a lone id with no value",
		"github:a/b abc123",
		"  github:c/d   def456  ", // leading/trailing and extra whitespace
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "sha256.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _, err := r.Check("github:a/b", "sha256", "abc123"); err != nil || got != TrustMatch {
		t.Fatalf("Check = %v, %v", got, err)
	}
	if got, old, err := r.Check("github:c/d", "sha256", "def456"); err != nil || got != TrustMatch {
		t.Fatalf("the row with extra whitespace was not read correctly: %v, %q, %v", got, old, err)
	}
	// A kind that was never written must still be readable.
	if got, _, err := r.Check("github:a/b", "key", "x"); err != nil || got != TrustNew {
		t.Fatalf("a missing file should be TrustNew, got %v, %v", got, err)
	}
}

// kind is used directly as a file name, so it must be a safe name and not a path.
func TestRingRejectsBadInput(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, id, kind, value string }{
		{"empty id", "", "key", "v"},
		{"empty value", "github:a/b", "key", ""},
		{"empty kind", "github:a/b", "", "v"},
		{"kind path traversal", "github:a/b", "../../etc/passwd", "v"},
		{"kind contains a slash", "github:a/b", "a/b", "v"},
		{"id contains a space", "github:a/b c", "key", "v"},
		{"id contains a newline", "github:a/b\nevil x", "key", "v"},
		{"value contains a space", "github:a/b", "key", "v v"},
		{"id starts with #", "#github:a/b", "key", "v"}, // load would swallow it as a comment
		{"id is just #", "#", "key", "v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Pin(tc.id, tc.kind, tc.value); err == nil {
				t.Fatal("Pin should refuse")
			}
			if _, _, err := r.Check(tc.id, tc.kind, tc.value); err == nil {
				t.Fatal("Check should refuse")
			}
		})
	}
	if _, err := OpenRing(""); err == nil {
		t.Fatal("an empty directory name should be refused")
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
			t.Fatalf("Pin call %d: %v", i, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "github:a/b abc\n" {
		t.Fatalf("re-Pinning the same value must not change the file, got %q", raw)
	}
	// No temp files left behind (key.txt.lock is Pin's lock and is meant to persist).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "key.txt" && e.Name() != "key.txt.lock" {
			t.Fatalf("left a temp file behind: %s", e.Name())
		}
	}
}

func TestTrustString(t *testing.T) {
	for tr, want := range map[Trust]string{TrustNew: "new", TrustMatch: "match", TrustChanged: "changed"} {
		if got := tr.String(); got != want {
			t.Fatalf("Trust(%d).String() = %q, want %q", int(tr), got, want)
		}
	}
}

// If Pin reports success, the value must actually stay pinned. This invariant matters
// more than any single case: a record that Pins successfully but that Check cannot read
// back means TOFU is silently off for that package -- every install is "first time,
// pinned", and nobody is told when the artifact changes.
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
		"#github:evil/pkg", // would be swallowed as a comment; must be refused at Pin
		"#",
		"--force",
		"..",
		"../../etc/passwd",
		strings.Repeat("a", 4096),
		"a\x00b",
		"name with non-ASCII characters",
	}
	for _, id := range ids {
		if err := r.Pin(id, "sha256", "cafebabe"); err != nil {
			continue // Refusing is an acceptable answer; silently losing it is not
		}
		got, old, err := r.Check(id, "sha256", "cafebabe")
		if err != nil {
			t.Fatalf("Pin %q succeeded but Check errored: %v", id, err)
		}
		if got != TrustMatch {
			t.Fatalf("Pin %q succeeded but Check says %v (old value %q) -- the pin was silently lost", id, got, old)
		}
	}
}

// Concurrent Pins must not lose records. Losing one means that package is TrustNew
// (allow and re-pin) on its next install instead of TrustMatch -- TOFU silently
// downgraded. Before the fix, 40 concurrent writers left 16 records.
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
			t.Fatalf("after the concurrent writes github:p/%d = %v -- the pin was lost", i, got)
		}
	}
}

// Check must fail closed. The zero value TrustNew is the "allow and pin" tier, so a
// caller that drops the error would silently downgrade into a fresh TOFU.
func TestCheckFailsClosed(t *testing.T) {
	r, err := OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Bad input is the only error path that does not depend on filesystem permissions.
	for _, tc := range []struct{ id, kind, value string }{
		{"", "sha256", "v"},
		{"github:a/b", "../etc", "v"},
		{"github:a/b", "sha256", ""},
	} {
		got, _, err := r.Check(tc.id, tc.kind, tc.value)
		if err == nil {
			t.Fatalf("Check(%q,%q,%q) should error", tc.id, tc.kind, tc.value)
		}
		if got == TrustNew {
			t.Fatalf("Check returned TrustNew (the allow tier) on error; it should be TrustChanged")
		}
	}
	// Same when the whole keyring is unreadable. chmod does not stop root, so a directory
	// is put where the file should be.
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
		t.Fatal("an unreadable keyring must error, not be treated as nothing pinned")
	}
	if got == TrustNew {
		t.Fatalf("an unreadable keyring returned TrustNew (allow); it should be TrustChanged")
	}
}

// "Refuses to overwrite" has to be guaranteed by O_EXCL, not by Stat alone -- in the
// window between Stat and the write, a private key created by another toy keygen would
// be wiped by O_TRUNC.
func TestSaveKeyPairNeverTruncatesAnExistingSecret(t *testing.T) {
	dir := t.TempDir()
	// Private key present (Stat sees it), public key absent -- it must stop before it
	// ever gets to writing the public key.
	secPath := filepath.Join(dir, secFile)
	const sentinel = "somebody else's private key"
	if err := os.WriteFile(secPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveKeyPair(dir, mustGenerate(t)); err == nil {
		t.Fatal("must refuse when the private key already exists")
	}
	raw, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatalf("the existing private key was modified: %q", raw)
	}

	// Bypass the Stat loop and confirm directly that the layer below uses O_EXCL, not O_TRUNC.
	if err := writeNew(secPath, "new", 0o600); err == nil {
		t.Fatal("writeNew must use O_EXCL and never overwrite an existing file")
	}
	if raw, _ := os.ReadFile(secPath); string(raw) != sentinel {
		t.Fatalf("writeNew truncated an existing file: %q", raw)
	}
}

// Sign message boundaries: different messages and different private keys must not
// produce the same signature.
func TestSignIsBoundToKeyAndMessage(t *testing.T) {
	a, b := mustGenerate(t), mustGenerate(t)
	msg := []byte("name = \"tinyserve\"")
	if Sign(a.Private, msg) == Sign(b.Private, msg) {
		t.Fatal("two different private keys produced the same signature")
	}
	if Sign(a.Private, msg) == Sign(a.Private, append(msg, '!')) {
		t.Fatal("two different messages produced the same signature")
	}
	// a's signature against b's public key must be refused.
	if err := Verify(hex.EncodeToString(b.Public), msg, Sign(a.Private, msg)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a different public key should give ErrBadSignature, got %v", err)
	}
}
