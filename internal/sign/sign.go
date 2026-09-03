// Package sign is ed25519 signing plus the TOFU keyring. Spec §4.
// A signature is an optional upgrade, not an entry ticket -- mandatory signing means
// mandatory registration, which is just one more gate.
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
	ErrBadSignature = errors.New("sign: signature verification failed")
	// The wording deliberately draws no conclusion: the two possibilities are
	// indistinguishable from the bytes, so the client must not pick one for the user.
	ErrChanged = errors.New("sign: value does not match the one pinned on first install " +
		"(the author may have re-released the same version, or the content may have been swapped)")
	ErrNoKey = errors.New("sign: no keypair on this machine yet; run `toy keygen` first")
)

// Key file names. Both keys are stored as hex text, because Verify takes a hex
// public key -- so whatever the user cats out of key.pub can be pasted straight
// into an index entry's "key" field.
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
		return KeyPair{}, fmt.Errorf("sign: could not generate keypair: %w", err)
	}
	return KeyPair{Public: pub, Private: priv}, nil
}

// LoadKeyPair reads key.pub / key.sec from dir. The private key must not be
// readable by the group or by others; otherwise loading is refused, with a reason.
func LoadKeyPair(dir string) (KeyPair, error) {
	secPath := filepath.Join(dir, secFile)
	info, err := os.Stat(secPath)
	if errors.Is(err, os.ErrNotExist) {
		return KeyPair{}, ErrNoKey
	}
	if err != nil {
		return KeyPair{}, fmt.Errorf("sign: cannot read %s: %w", secPath, err)
	}
	// Same as ssh: refuse to load a private key others can read, and say how to fix it.
	// The test is "not one group/other bit", not "exactly 0600": 0400 is stricter than
	// 0600 (and is what you commonly get after restoring a key from a backup). Refusing
	// it buys no security, and the "fix: chmod 600" line would be teaching the user to
	// make a read-only private key writable.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return KeyPair{}, fmt.Errorf(
			"sign: private key %s has mode %04o, so your group or other users can read it. Fix it with: chmod 600 %s",
			secPath, perm, secPath)
	}

	priv, err := readHexFile(secPath, ed25519.PrivateKeySize, "private key")
	if err != nil {
		return KeyPair{}, err
	}
	pubPath := filepath.Join(dir, pubFile)
	pub, err := readHexFile(pubPath, ed25519.PublicKeySize, "public key")
	if errors.Is(err, os.ErrNotExist) {
		return KeyPair{}, fmt.Errorf("sign: private key present but public key %s is missing; delete %s and run `toy keygen` again", pubPath, secFile)
	}
	if err != nil {
		return KeyPair{}, err
	}

	kp := KeyPair{Public: ed25519.PublicKey(pub), Private: ed25519.PrivateKey(priv)}
	// The last 32 bytes of the private key are the public key; a mismatch means the two
	// files are not a pair.
	if !bytes.Equal(kp.Private.Public().(ed25519.PublicKey), kp.Public) {
		return KeyPair{}, fmt.Errorf("sign: %s and %s are not the same keypair; delete both and run `toy keygen` again", pubFile, secFile)
	}
	return kp, nil
}

func readHexFile(path string, want int, what string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err // Callers match ErrNotExist with errors.Is; do not wrap it away.
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("sign: %s %s is not valid hex: %w", what, path, err)
	}
	if len(b) != want {
		return nil, fmt.Errorf("sign: %s %s is %d bytes, expected %d", what, path, len(b), want)
	}
	return b, nil
}

// SaveKeyPair writes key.pub (0644) and key.sec (0600). It refuses to overwrite.
func SaveKeyPair(dir string, kp KeyPair) error {
	if len(kp.Public) != ed25519.PublicKeySize || len(kp.Private) != ed25519.PrivateKeySize {
		return errors.New("sign: keypair has the wrong length, refusing to save it")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sign: could not create directory %s: %w", dir, err)
	}
	pubPath := filepath.Join(dir, pubFile)
	secPath := filepath.Join(dir, secFile)
	for _, p := range []string{pubPath, secPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("sign: %s already exists and will not be overwritten. To use a new key, move or delete it yourself first", p)
		}
	}
	// Private key first: writing the public key and then failing on the private one
	// would leave a useless half of a pair on disk.
	if err := writeNew(secPath, hex.EncodeToString(kp.Private)+"\n", 0o600); err != nil {
		return fmt.Errorf("sign: could not write private key %s: %w", secPath, err)
	}
	if err := writeNew(pubPath, hex.EncodeToString(kp.Public)+"\n", 0o644); err != nil {
		return fmt.Errorf("sign: could not write public key %s: %w", pubPath, err)
	}
	return nil
}

// writeNew creates a new file and flushes it. O_EXCL rather than os.WriteFile
// (O_TRUNC): the Stat loop above only exists to produce a good error message, and
// there is a window between Stat and the write -- O_EXCL is what actually makes
// "refuses to overwrite" true. The fsync is there because losing a private key means
// losing the author identity: nothing that key signed can ever be signed again.
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

// fingerprintHexLen is how many hex characters a fingerprint shows. The spec writes
// it as "a3f9…", which is the document's own elision. The fingerprint is the only
// place an author identity surfaces in the UI (toy keygen says exactly that: "this is
// the string other people see when they install"), so what it has to resist is a
// PREIMAGE: an attacker generating ed25519 keys over and over until the prefix
// matches a real author's fingerprint, then using that key to impersonate them in
// the index. ed25519 keygen runs tens of thousands per core-second, so 32 bits
// (8 hex) costs only tens of core-hours. 32 hex = 128 bits, which cannot be ground out.
const fingerprintHexLen = 32

// Fingerprint returns the short fingerprint of a public key, shown as "a3f9…".
// Iron rule 8 (print the trust level honestly): something of the wrong length is not
// an ed25519 public key at all and must never render as a normal-looking fingerprint
// -- a "key a3…" on screen reads as "this package has a signer".
func Fingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return "(no public key)"
	}
	if len(pub) != ed25519.PublicKeySize {
		return "(invalid public key)"
	}
	return hex.EncodeToString(pub)[:fingerprintHexLen] + "…"
}

// Sign returns a standard-base64 signature. The message is the raw bytes; there is
// no extra hash layer here -- ed25519 already does that internally.
// A private key of the wrong length panics (standard library behaviour; this
// signature has no error to return). That is deliberately not caught: returning an
// empty signature would let the caller publish an entry that fails verification, and
// the failure would only surface when somebody else installs it. A private key can
// only come from Generate or LoadKeyPair, and both guarantee the length.
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// Verify checks a signature against a hex-encoded public key.
func Verify(pubHex string, msg []byte, sigB64 string) error {
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil {
		return fmt.Errorf("sign: public key is not valid hex: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("sign: public key is %d bytes, expected %d", len(pub), ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("sign: signature is not valid base64: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// Trust is the three possible TOFU outcomes.
type Trust int

const (
	TrustNew     Trust = iota // First time seen; now pinned
	TrustMatch                // Matches what was pinned
	TrustChanged              // Changed -- warn loudly and refuse, unless --force
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

// Ring is the TOFU record under ~/.toy/known/, modelled on SSH's known_hosts.
// One file per kind, <kind>.txt, one "<id> <value>" per line.
// An id can contain : and /, so split on the first space.
type Ring struct{ Dir string }

func OpenRing(dir string) (*Ring, error) {
	if dir == "" {
		return nil, errors.New("sign: keyring directory cannot be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sign: could not create keyring directory %s: %w", dir, err)
	}
	return &Ring{Dir: dir}, nil
}

// Check looks up the value pinned for id under kind. kind is "key" or "sha256".
// TrustNew does not pin automatically -- the caller calls Pin only after the install
// actually succeeded.
// The second return value is the old pinned value: on TrustChanged the caller has to
// print it for the user.
// An error only means reading the file went wrong; "the value changed" is
// TrustChanged, not an error.
// On error it returns TrustChanged rather than the zero value TrustNew: TrustNew is
// the "allow and pin" tier, and a keyring that cannot be read is exactly when you
// must not allow. A caller that drops the error hits a refused install instead of
// silently downgrading into a fresh TOFU.
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

// Pin records the value for id, overwriting a different existing one (the caller is
// expected to already have the user's --force).
func (r *Ring) Pin(id, kind, value string) error {
	if err := validEntry(id, kind, value); err != nil {
		return err
	}
	// The whole read-modify-write has to be under the lock: without it two concurrent
	// toy installs each read the old contents and each write the whole file back, and
	// the later write erases the earlier pin -- and a lost pin means that package's
	// next install is TrustNew (allow and re-pin) instead of TrustMatch, i.e. TOFU
	// silently downgraded. Measured: 40 concurrent writers left 16 records.
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

// withLock takes an exclusive lock on <kind>.txt.lock. The lock lives in a separate
// file rather than the data file because save renames over the data file, which
// would leave the lock held on the replaced inode.
// flock is released by the kernel when the process dies, so no stale lock file is
// left behind.
// Only Pin needs it: Check is read-only, and rename guarantees a reader sees either
// the old file or the new one.
// syscall.Flock restricts this package to unix -- spec §11 says v1 is Linux + macOS
// only, so there is no pair of build-tagged files for Windows. Split by platform here
// if Windows ever happens.
func (r *Ring) withLock(kind string, fn func() error) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("sign: could not create keyring directory %s: %w", r.Dir, err)
	}
	lockPath := r.path(kind) + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("sign: could not open keyring lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("sign: could not lock the keyring %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// validEntry rejects input that would corrupt this file format. kind is used
// directly as a file name, so it has to be a safe name and not a path.
func validEntry(id, kind, value string) error {
	if id == "" {
		return errors.New("sign: id cannot be empty")
	}
	if value == "" {
		return errors.New("sign: the value to pin cannot be empty")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return fmt.Errorf("sign: id %q must not contain whitespace", id)
	}
	// load skips lines starting with # as comments, so such an id would Pin fine and
	// never be read back by Check -- a record that can never be pinned means TOFU is
	// silently off for that package. Refusing is better than dropping.
	if strings.HasPrefix(id, "#") {
		return fmt.Errorf("sign: id %q must not start with # (that is the keyring comment marker)", id)
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("sign: value %q must not contain whitespace", value)
	}
	if kind == "" {
		return errors.New("sign: kind cannot be empty (use \"key\" or \"sha256\")")
	}
	for _, c := range kind {
		ok := c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("sign: kind %q may only contain lowercase letters, digits, - and _", kind)
		}
	}
	return nil
}

func (r *Ring) path(kind string) string { return filepath.Join(r.Dir, kind+".txt") }

// load reads every record for one kind. A missing file simply means "nothing pinned
// yet", not an error.
func (r *Ring) load(kind string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(r.path(kind))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sign: could not read keyring %s: %w", r.path(kind), err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// An id contains : and /, so split on the first space only.
		id, value, ok := strings.Cut(line, " ")
		if !ok || id == "" || strings.TrimSpace(value) == "" {
			continue // Skip a garbled half-line; one bad row must not turn the whole keyring into an error.
		}
		out[id] = strings.TrimSpace(value)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sign: could not read keyring %s: %w", r.path(kind), err)
	}
	return out, nil
}

func (r *Ring) save(kind string, pinned map[string]string) error {
	ids := make([]string, 0, len(pinned))
	for id := range pinned {
		ids = append(ids, id)
	}
	slices.Sort(ids) // Purely so the file content is stable and diffs cleanly.

	var buf bytes.Buffer
	for _, id := range ids {
		fmt.Fprintf(&buf, "%s %s\n", id, pinned[id])
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("sign: could not create keyring directory %s: %w", r.Dir, err)
	}
	// Write a temp file and rename: dying halfway must not leave a truncated keyring.
	tmp, err := os.CreateTemp(r.Dir, kind+".txt.*")
	if err != nil {
		return fmt.Errorf("sign: could not write the keyring: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // A no-op once the rename succeeded.
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: could not write the keyring: %w", err)
	}
	// Without this, rename's atomicity only guarantees "either the old file or the new
	// name", not that the new file has contents: the common outcome after a power cut
	// is a zero-length keyring, i.e. every pin silently gone and every package back to
	// a first install.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: could not flush the keyring to disk: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("sign: could not set keyring file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sign: could not write the keyring: %w", err)
	}
	if err := os.Rename(tmpName, r.path(kind)); err != nil {
		return fmt.Errorf("sign: could not write keyring %s: %w", r.path(kind), err)
	}
	return nil
}
