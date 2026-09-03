package ui

import (
	"fmt"
	"strings"
	"testing"
)

// safe's doc comment says isCtrl "covers C0, DEL, and C1 (0x80-0x9f)", and iron
// law 8 says the trust line must not be overwritable. In fact C1 only gets
// escaped when it arrives as valid UTF-8 (0xC2 0x9B); a bare 0x9b byte passes
// through untouched, because the fast path's strings.IndexFunc decodes invalid
// UTF-8 to U+FFFD (not a control character), so the whole string is judged
// clean and returned as-is.
//
// 0x9b IS the 8-bit CSI. On a terminal that interprets 8-bit C1 (the Linux
// virtual console, xterm with 8-bit controls enabled, some screen/tmux setups),
// CSI 1A + CSI 2K = move the cursor up one line + erase that line — exactly
// enough to rewrite the trust level printed above it.
const auditFakeTrust = "✓ signed · key a3f9c1d2 · registered in index default"

// rawCSI is the bare byte form; utf8CSI is the same code point as valid UTF-8.
const rawCSI = "\x9b"

var utf8CSI = string(rune(0x9b))

func auditPayload(csi string) string {
	return "404 " + csi + "1A" + csi + "2K" + csi + "32m" + auditFakeTrust
}

func TestSafeLetsRawC1BytesThroughUntouched(t *testing.T) {
	payload := auditPayload(rawCSI) // only bare C1, no other control chars: fast path
	if got := safe(payload); got == payload {
		t.Errorf("safe passed a bare C1 byte through untouched: % x", got)
	}
	// Control: the same code point written as valid UTF-8 does get escaped.
	if got := safe(auditPayload(utf8CSI)); !strings.Contains(got, `\x9b`) {
		t.Errorf("the U+009B path should escape to \\x9b, got % x", got)
	}
}

// Same byte, opposite outcomes depending on whether something ELSE in the
// string is a control character: with one, the slow path runs (and WriteRune
// turns 0x9b into U+FFFD); without one, the fast path lets it through. A
// sanitizer whose result is a coin flip is not a sanitizer.
func TestSafeIsInconsistentAboutTheSameByte(t *testing.T) {
	// Compare bytes, not ContainsRune: strings.IndexRune treats utf8.RuneError
	// as "matches any invalid byte", so testing by rune gives a false negative.
	fast := safe("x" + rawCSI + "y")
	slow := safe("x" + rawCSI + "y\r")
	if strings.Contains(fast, rawCSI) == strings.Contains(slow, rawCSI) {
		t.Skip("behavior is consistent now")
	}
	t.Errorf("one 0x9b, two fates: fast=% x slow=% x", fast, slow)
}

// End to end: fetch's "HTTP %s" copies the server's status line verbatim (the
// reason phrase is arbitrary bytes; Go only guarantees it holds no \n). With
// the default config there are no mirrors, so errors.Join has a single child
// error and inserts no \n either — meaning the whole error string contains not
// one valid control character, the fast path lets it through, and the bare CSI
// reaches the terminal. `toy update`'s loop prints package B's ui.Error right
// after package A's trust line, so this is a live path for rewriting A's trust
// level using B's failure.
func TestServerStatusLineReachesTheTerminalUnescaped(t *testing.T) {
	err := fmt.Errorf("fetch: original URL and every mirror failed: %w",
		fmt.Errorf("https://example.com/x.tar.gz: %w",
			fmt.Errorf("HTTP %s", auditPayload(rawCSI))))

	got := capture(t, func() { Error("%v", err) })
	if strings.Contains(got, rawCSI) {
		t.Errorf("a bare CSI reached the terminal verbatim: % x", got)
	}
}
