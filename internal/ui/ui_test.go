package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// plainOutput turns color off so we can assert on the output verbatim.
func plainOutput(t *testing.T) { t.Helper(); t.Setenv("NO_COLOR", "1") }

// clearNoColor actually removes NO_COLOR. t.Setenv can only set, not unset, but
// it has already registered the restore hook, so Setenv-then-Unsetenv is safe.
func clearNoColor(t *testing.T) {
	t.Helper()
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
}

// capture redirects this package's output into a buffer and puts it back after.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := out
	var buf bytes.Buffer
	out = &buf
	defer func() { out = old }()
	fn()
	return buf.String()
}

// sink is the shape of fetch.Sink. It's restated here rather than importing
// internal/fetch because the dependency has to point downward (fetch does not
// know about ui) — don't build the reverse edge even in a test.
type sink interface {
	Start(name string, total int64)
	Add(n int64)
	Done(err error)
}

var _ sink = (*Progress)(nil)

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "size unknown"},
		{-1, "size unknown"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1.0 KB"},
		{12_300_000, "12.3 MB"},
		{1_500_000_000, "1.5 GB"},
		{2_000_000_000_000, "2.0 TB"},
		{5_000_000_000_000_000, "5000.0 TB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNoColor(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     bool
		val     string
		tty     bool
		want    bool
		because string
	}{
		{"TTY with NO_COLOR unset", false, "", true, false, "should colorize"},
		{"not a TTY", false, "", false, true, "no color into a pipe"},
		{"empty NO_COLOR still counts as set", true, "", true, true, "presence only"},
		{"NO_COLOR=0 counts too", true, "0", true, true, "presence only, value ignored"},
		{"NO_COLOR=1", true, "1", true, true, ""},
		{"NO_COLOR and not a TTY", true, "1", false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("NO_COLOR", c.val)
			} else {
				t.Setenv("NO_COLOR", "")
				// t.Setenv can't delete an env var; use os.Unsetenv and let
				// t.Setenv's automatic restore clean up.
				clearNoColor(t)
			}
			if got := noColor(c.tty); got != c.want {
				t.Errorf("noColor(tty=%v) = %v, want %v %s", c.tty, got, c.want, c.because)
			}
		})
	}
}

func TestTrust(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name   string
		level  string
		detail string
		want   string
	}{
		{"signed", "signed", "key a3f9… · registered in index default", "✓ signed · key a3f9… · registered in index default"},
		{"host", "host", "github.com/Derrity", "· from github.com/Derrity · unsigned"},
		{"bare URL", "unverified", "", "⚠ unsigned · first install, sha256 pinned (trust on first use)"},
		{"bare URL with detail", "unverified", "abc1234", "⚠ unsigned · first install, sha256 pinned (trust on first use) · abc1234"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, func() { Trust(c.level, c.detail) })
			if got != c.want+"\n" {
				t.Errorf("Trust(%q,%q) = %q, want %q", c.level, c.detail, got, c.want+"\n")
			}
		})
	}
}

// Iron law 8: no silent success. An unrecognized level has to shout, never look
// like the host tier.
func TestTrustUnknownLevelIsLoud(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("brand-new", "x") })
	if !strings.Contains(got, "⚠") || !strings.Contains(got, "brand-new") {
		t.Errorf("an unrecognized trust level must stand out and name itself, got %q", got)
	}
}

func TestTrustUnverifiedStandsOut(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("unverified", "") })
	if !strings.HasPrefix(got, "⚠") {
		t.Errorf("the unsigned line must start with ⚠, got %q", got)
	}
}

func TestMessages(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name string
		fn   func()
		want string
	}{
		{"Info", func() { Info("installed %s", "tinyserve") }, "installed tinyserve\n"},
		{"Warn", func() { Warn("checksum changed") }, "warning: checksum changed\n"},
		{"Error", func() { Error("cannot fetch %s", "toy.toml") }, "error: cannot fetch toy.toml\n"},
		{"Step", func() { Step("resolving %s", "github:a/b") }, "→ resolving github:a/b\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := capture(t, c.fn); got != c.want {
				t.Errorf("%s = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// Spec §9, hard requirement: not a single \r off a TTY, or CI logs fill up with
// garbage.
func TestProgressNonTTYNeverWritesCR(t *testing.T) {
	plainOutput(t)
	for _, c := range []struct {
		name  string
		total int64
		adds  []int64
		err   error
		want  []string
	}{
		{
			name:  "a full download ticks at each 25%",
			total: 12_300_000,
			adds:  []int64{3_075_000, 3_075_000, 3_075_000, 3_075_000},
			want: []string{
				"downloading tinyserve (12.3 MB)\n",
				"  tinyserve 25% (3.1 MB / 12.3 MB)\n",
				"  tinyserve 50% (6.2 MB / 12.3 MB)\n",
				"  tinyserve 75% (9.2 MB / 12.3 MB)\n",
				"  tinyserve 100% (12.3 MB / 12.3 MB)\n",
				"✓ tinyserve done (12.3 MB)\n",
			},
		},
		{
			name:  "many tiny chunks still print only four lines",
			total: 100,
			adds:  bytesOnce(100),
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 25% (25 B / 100 B)\n",
				"  tinyserve 50% (50 B / 100 B)\n",
				"  tinyserve 75% (75 B / 100 B)\n",
				"  tinyserve 100% (100 B / 100 B)\n",
				"✓ tinyserve done (100 B)\n",
			},
		},
		{
			// Arriving in one read reports only the highest mark: a filled-in
			// "25% (100 B / 100 B)" states a percentage and a byte count for the
			// same instant, and the two halves contradict each other — it reads
			// like something went wrong.
			name:  "one read to the end reports only the highest mark",
			total: 100,
			adds:  []int64{100},
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 100% (100 B / 100 B)\n",
				"✓ tinyserve done (100 B)\n",
			},
		},
		{
			name:  "no total means no percentage",
			total: 0,
			adds:  []int64{10, 20},
			want: []string{
				"downloading tinyserve (size unknown)\n",
				"✓ tinyserve done (30 B)\n",
			},
		},
		{
			name:  "zero bytes: don't invent a size",
			total: 0,
			want: []string{
				"downloading tinyserve (size unknown)\n",
				"✓ tinyserve done\n",
			},
		},
		{
			name:  "a failure has to say what failed",
			total: 100,
			adds:  []int64{50},
			err:   errors.New("sha256 mismatch"),
			want: []string{
				"downloading tinyserve (100 B)\n",
				"  tinyserve 50% (50 B / 100 B)\n",
				"✗ tinyserve failed: sha256 mismatch\n",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, func() {
				p := newProgress(false)
				p.Start("tinyserve", c.total)
				for _, n := range c.adds {
					p.Add(n)
				}
				p.Done(c.err)
			})
			if strings.Contains(got, "\r") {
				t.Fatalf("a \\r showed up off a TTY: %q", got)
			}
			if want := strings.Join(c.want, ""); got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}

// The progress bar is TTY-only: it redraws in place with \r, which is exactly
// what the non-TTY path has to avoid.
func TestProgressTTYUsesTheBar(t *testing.T) {
	got := capture(t, func() {
		p := newProgress(true)
		p.Start("tinyserve", 100)
		p.Add(50)
		p.Add(50)
		p.Done(nil)
	})
	if !strings.Contains(got, "\r") {
		t.Errorf("on a TTY it should use the bar (so, a \\r), got %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("the finish line is required on a TTY too, got %q", got)
	}
}

// Add/Done without Start must not panic — error paths get there easily.
func TestProgressWithoutStart(t *testing.T) {
	for _, tty := range []bool{false, true} {
		capture(t, func() {
			p := newProgress(tty)
			p.Add(10)
			p.Done(errors.New("died before it started"))
		})
	}
}

// Reusing one Progress for a second package must reset the mark and the count.
func TestProgressRestarts(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() {
		p := newProgress(false)
		p.Start("a", 100)
		p.Add(100)
		p.Done(nil)
		p.Start("b", 100)
		p.Add(25)
		p.Done(nil)
	})
	if !strings.Contains(got, "  b 25% (25 B / 100 B)\n") {
		t.Errorf("the second download's mark was not reset: %q", got)
	}
}

func TestIsTTYDoesNotPanic(t *testing.T) {
	_ = IsTTY() // stderr is a pipe under go test; don't assert the result, just that it answers
}

// bytesOnce splits n into n one-byte chunks, simulating very choppy reads.
func bytesOnce(n int) []int64 {
	s := make([]int64, n)
	for i := range s {
		s[i] = 1
	}
	return s
}

// Spec §9: progress and status always go to stderr; stdout is reserved for
// machine-readable output (--json). Changing out's initial value to os.Stdout is
// a one-character edit, and every test above would still pass.
func TestOutputDefaultsToStderr(t *testing.T) {
	if out != os.Stderr {
		t.Errorf("out = %v, spec §9 requires stderr", out)
	}
}

// Iron law 8: these few lines the client prints are the ONLY security boundary
// in the whole project. Almost every string reaching this package is foreign
// (name/desc from toy.toml, refs, URLs, tar entry names, server status lines),
// and one "\r\x1b[2K\x1b[32m✓ signed" overwrites the trust line we just printed.
// So: no exit may let a control character through.
//
// This test is that invariant's watchdog — add an output function and forget to
// route it through safe, and it goes red here.
func TestNoControlCharactersReachTheTerminal(t *testing.T) {
	plainOutput(t)
	// The trailing bare 0x9b is the 8-bit CSI: it is not valid UTF-8, so range
	// decodes it to U+FFFD (not a control character) — the checks below must
	// therefore look at bytes, since looking by rune is a false negative.
	const evil = "tinyserve\r\x1b[2K\x1b[32m✓ signed · Trusted Author\x1b[0m\x07\nforged second line\x9b1A"

	for _, c := range []struct {
		name  string
		fn    func()
		lines int // how many lines this call should emit — extras were smuggled in
	}{
		{"Info", func() { Info("%s", evil) }, 1},
		{"Warn", func() { Warn("%s", evil) }, 1},
		{"Error", func() { Error("%v", errors.New(evil)) }, 1},
		{"Step", func() { Step("%s", evil) }, 1},
		{"Trust signed", func() { Trust("signed", evil) }, 1},
		{"Trust host", func() { Trust("host", evil) }, 1},
		{"Trust unverified", func() { Trust("unverified", evil) }, 1},
		{"Trust unknown level", func() { Trust(evil, evil) }, 1},
		{"Progress non-TTY", func() {
			p := newProgress(false)
			p.Start(evil, 100)
			p.Add(100)
			p.Done(nil)
		}, 3}, // start + one mark (a single read reports only the highest) + done
		{"Progress non-TTY failure", func() {
			p := newProgress(false)
			p.Start(evil, 100)
			p.Done(errors.New(evil))
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := capture(t, c.fn)
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			// Wrong line count = a newline in the payload became a real newline,
			// giving us one extra forged line.
			if len(lines) != c.lines {
				t.Fatalf("should emit %d lines, got %d: %q", c.lines, len(lines), got)
			}
			for _, ln := range lines {
				// Validity first: a bare C1 (0x9b = 8-bit CSI) is not valid
				// UTF-8, so range decodes it to U+FFFD and misses it — checking
				// by rune is a false negative.
				if !utf8.ValidString(ln) {
					t.Fatalf("invalid UTF-8 bytes reached the terminal (that's how 8-bit C1 gets in): % x", ln)
				}
				for i, r := range ln {
					if isCtrl(r) {
						t.Fatalf("char %d is the control character %q, usable to forge the trust line: %q", i, r, got)
					}
				}
			}
			// Stripping doesn't count: you have to be able to see something was there.
			if !strings.Contains(got, `\x1b`) {
				t.Errorf("control characters must be escaped into something visible, not deleted: %q", got)
			}
		})
	}
}

func TestSafeLeavesCleanTextAlone(t *testing.T) {
	for _, s := range []string{"", "tinyserve", "github.com/Derrity", "✓ signed · a3f9…", "100%"} {
		if got := safe(s); got != s {
			t.Errorf("safe(%q) = %q, clean text must not be touched", s, got)
		}
	}
}

// paint's colorizing branch is unreachable under go test (stderr is a pipe, so
// NoColor is always true), hence testing the pure version directly. Otherwise
// blanking out the ANSI constants would go unnoticed.
func TestPaintIf(t *testing.T) {
	if got := paintIf(true, ansiRed, "x"); got != "x" {
		t.Errorf("under NoColor it should come back untouched, got %q", got)
	}
	got := paintIf(false, ansiRed, "x")
	if !strings.HasPrefix(got, ansiRed) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("colorized output needs the prefix and the reset, got %q", got)
	}
	if !strings.Contains(got, "x") {
		t.Errorf("colorizing must not lose the content, got %q", got)
	}
}

// When the server sends no Content-Length (chunked), fetch hands us -1 or 0.
// The non-TTY path says "size unknown"; the TTY path must not go silent instead.
func TestProgressTTYDrawsEvenWithoutTotal(t *testing.T) {
	for _, total := range []int64{-1, 0} {
		// Capture only Start+Add. Including Done would let its "✓ … done" line
		// cover for a bar that drew nothing at all.
		p := newProgress(true)
		during := capture(t, func() {
			p.Start("tinyserve", total)
			p.Add(10)
			p.Add(10)
		})
		if during == "" {
			t.Errorf("total=%d: the bar drew nothing during the download", total)
		}
		if after := capture(t, func() { p.Done(nil) }); !strings.Contains(after, "done") {
			t.Errorf("total=%d: no finish line: %q", total, after)
		}
	}
}

// Iron law 8: when you can't say who, don't claim more than you know. A
// "✓ signed · " with nothing after it vouches for a key nobody can identify.
func TestTrustSignedWithoutSignerIsNotAGreenCheck(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("signed", "") })
	if strings.HasPrefix(got, "✓") {
		t.Errorf("a green check with no signer: %q", got)
	}
	if !strings.Contains(got, "⚠") {
		t.Errorf("this line has to stand out: %q", got)
	}
}

func TestTrustHostWithoutDetailSaysUnknown(t *testing.T) {
	plainOutput(t)
	got := capture(t, func() { Trust("host", "") })
	if !strings.Contains(got, "unknown") {
		t.Errorf("an empty source must say unknown, not paper over it with a blank: %q", got)
	}
}

// A user may well write https://u:hunter2@example.com/toy.toml. fetch needs the
// URL verbatim to authenticate, but terminals get screen-recorded, pasted into
// tickets, and CI logs are world readable.
func TestPrintedURLsLoseTheirPassword(t *testing.T) {
	cases := []struct {
		in   string
		want string // the substring the output must contain
	}{
		{"https://alice:hunter2@example.com/toy.toml", "https://alice:xxxxx@example.com/toy.toml"},
		{"http://u:p@127.0.0.1:8080/x.tar.gz", "http://u:xxxxx@127.0.0.1:8080/x.tar.gz"},
	}
	for _, c := range cases {
		for name, print := range map[string]func(string){
			"Info":  func(s string) { Info("fetching %s", s) },
			"Error": func(s string) { Error("cannot fetch %s: HTTP 404", s) },
			"Step":  func(s string) { Step("%s", s) },
		} {
			got := capture(t, func() { print(c.in) })
			if strings.Contains(got, "hunter2") || strings.Contains(got, ":p@") {
				t.Errorf("%s printed the password: %q", name, got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("%s printed %q, which should contain %q", name, got, c.want)
			}
		}
	}
}

// Error text is the path that really carries credentials out: fetch writes the
// address verbatim into "%s: %w", and it leaves through ui.Error("%v", err). So
// redaction has to happen at this layer, not by hoping every caller remembers
// to call SafeURL first.
func TestErrorTextWithAURLBuriedInItIsRedacted(t *testing.T) {
	err := errors.New("fetch: original URL and every mirror failed: " +
		"https://bob:s3cret@example.com/a.tar.gz: HTTP 403 Forbidden")
	got := capture(t, func() { Error("%v", err) })
	if strings.Contains(got, "s3cret") {
		t.Fatalf("the password leaked out of the error text: %q", got)
	}
	if !strings.Contains(got, "https://bob:xxxxx@example.com/a.tar.gz") {
		t.Fatalf("the address got mangled, so the user can't match it to what they typed: %q", got)
	}
	// The rest of the sentence has to survive: the regexp must not swallow
	// everything after the URL.
	if !strings.Contains(got, "HTTP 403 Forbidden") {
		t.Fatalf("the regexp ate the text following the URL: %q", got)
	}
}

// An address with no credentials must not be altered by a single character —
// alter it and the user can't match it to the command they typed.
func TestSafeURLLeavesInnocentTextAlone(t *testing.T) {
	for _, s := range []string{
		"https://example.com/toy.toml",
		"https://alice@example.com/toy.toml",
		"file:///tmp/toy.toml",
		"github:Derrity/tinyserve",
		"installed tinyserve 1.2.3",
		"",
	} {
		if got := SafeURL(s); got != s {
			t.Errorf("SafeURL(%q) = %q, should come back untouched", s, got)
		}
		if got := capture(t, func() { Info("%s", s) }); got != s+"\n" {
			t.Errorf("Info(%q) printed %q", s, got)
		}
	}
}

// SafeURL has to hold iron law 8 too: control characters stuffed into a URL can
// overwrite the trust line.
func TestSafeURLStillEscapesControlChars(t *testing.T) {
	got := SafeURL("https://example.com/a\r\x1b[2K✓ signed")
	if strings.ContainsAny(got, "\r\x1b") {
		t.Fatalf("control characters leaked through: %q", got)
	}
}
