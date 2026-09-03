// Package ui is terminal output. Spec §9: progress goes to stderr, and non-TTY
// must degrade automatically.
package ui

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"
)

// out is where everything in this package goes. Spec §9: progress and status go
// to stderr, machine-readable output (--json) belongs to stdout. It is a var
// only so tests can assert on it.
var out io.Writer = os.Stderr

// Plainest possible ANSI. No color library.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// IsTTY reports whether stderr is a terminal. When it isn't, the progress bar
// degrades to plain log lines — otherwise CI logs fill up with \r garbage.
func IsTTY() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

// NoColor is true when the NO_COLOR environment variable is present, or when
// we're not on a TTY.
func NoColor() bool { return noColor(IsTTY()) }

// noColor is the pure version of NoColor, so it's testable. NO_COLOR is checked
// for presence only, never for its value — that's the convention, so NO_COLOR=0
// still means don't colorize.
func noColor(tty bool) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return !tty
}

// paint wraps s in ANSI, or returns it untouched under NoColor.
func paint(code, s string) string { return paintIf(NoColor(), code, s) }

// paintIf is the pure version of paint, so it's testable. It has to exist:
// under go test, stderr is a pipe, so NoColor() is always true and the
// colorizing branch is unreachable through paint alone.
func paintIf(noColor bool, code, s string) string {
	if noColor {
		return s
	}
	return code + s + ansiReset
}

// isCtrl covers C0, DEL, and C1 (0x80-0x9f). C1 isn't usually treated as
// control in a UTF-8 terminal, but it also never shows up in a legitimate
// package name, URL, or error message.
func isCtrl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

// safe replaces control characters in foreign text with visible \xNN escapes.
//
// Almost every string that reaches this package is foreign: name/desc from
// toy.toml, refs, URLs, tar entry names, and the err in ui.Error("%v", err) —
// fetch's "HTTP %s" copies the server's status line verbatim. A single
// "\r\x1b[2K\x1b[32m✓ signed · Trusted" overwrites the trust line we just
// printed, and iron law 8 says the trust level must be reported honestly
// because the client is the ONLY security boundary. A line that can be
// overwritten is not a boundary.
//
// Escaping instead of stripping: stripping would wipe the evidence for the
// attacker.
//
// It also enforces spec §9's "not a single \r on non-TTY" for real — before,
// that only held because we didn't write \r ourselves; anything smuggled in
// through a string went straight out.
//
// Same reasoning for URLs carrying a password (redactURLs): this package is the
// single exit for all terminal output, so plugging it here beats hoping every
// caller remembers.
func safe(s string) string {
	s = redactURLs(s)
	// The fast path must first confirm the whole string is valid UTF-8:
	// IndexFunc decodes invalid bytes to U+FFFD, and U+FFFD is not a control
	// character — so a bare 0x9b (8-bit CSI) would sail through as clean.
	if utf8.ValidString(s) && strings.IndexFunc(s, isCtrl) < 0 {
		return s // the common case: one scan, no allocation
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if isCtrl(rune(c)) {
				fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			// Write the invalid byte out as itself, not as U+FFFD —
			// a placeholder would wipe the evidence for the attacker.
			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		if isCtrl(r) {
			fmt.Fprintf(&b, `\x%02x`, r)
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// SafeURL replaces the password in a URL with xxxxx (stdlib's URL.Redacted) and
// escapes control characters on the way out. Text that isn't a URL, or a URL
// with no credentials, comes back untouched.
//
// A user is perfectly entitled to run
// `toy install https://u:hunter2@example.com/toy.toml`: fetch needs the URL
// verbatim to authenticate, but that password must never reach the terminal —
// terminals get screen-recorded, pasted into tickets, and CI logs are world
// readable.
func SafeURL(s string) string { return safe(redactURL(s)) }

// redactURL is the credentials half of SafeURL, without the control-character
// escaping.
func redactURL(s string) string {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	if _, ok := u.User.Password(); !ok {
		// A username alone is not a secret; redacting it would only stop the
		// user from recognizing the address they typed.
		return s
	}
	return u.Redacted()
}

// urlish spots URLs inside a run of text. The character class is exactly what
// RFC 3986 allows in a URL, so the match stops at whitespace, quotes, and
// anything else outside that set. A trailing ASCII ':' does get swallowed into
// the match (it's a legal URL character), but redaction reproduces it verbatim,
// so text like "fetch: https://u:p@x/y: HTTP 404" survives with only the
// password replaced.
var urlish = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]*`)

// redactURLs strips the password out of every credential-carrying URL in an
// arbitrary run of text.
//
// This has to happen at this layer, not only where we print a URL directly: the
// thing that actually leaks credentials is error text — fetch's "%s: %w" puts
// the download address in verbatim, and it leaves through ui.Error("%v", err).
// Plug the source once.
func redactURLs(s string) string {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return s // almost no output carries credentials; skip the regexp
	}
	return urlish.ReplaceAllStringFunc(s, redactURL)
}

// sprintf is the one formatting entry point in this package. The format string
// is our own literal, but the arguments routinely come off the network, so the
// result always goes through safe. Use it when adding an output function —
// never fmt.Sprintf directly.
func sprintf(format string, a ...any) string { return safe(fmt.Sprintf(format, a...)) }

// line writes one line to stderr, with exactly one trailing newline.
func line(s string) {
	fmt.Fprintln(out, strings.TrimRight(s, "\n"))
}

// Everything below writes to stderr. Machine-readable output belongs to stdout.
func Info(format string, a ...any) { line(sprintf(format, a...)) }

func Warn(format string, a ...any) {
	line(paint(ansiYellow, "warning: "+sprintf(format, a...)))
}

func Error(format string, a ...any) {
	line(paint(ansiRed, "error: "+sprintf(format, a...)))
}

func Step(format string, a ...any) {
	line(paint(ansiDim, "→ ") + sprintf(format, a...))
}

// Trust prints the trust level on the install-completed line (iron law 8,
// spec §4):
//
//	signed         ✓ signed · key a3f9… · registered in index default
//	host           · from github.com/Derrity · unsigned
//	index-toyfile  ⚠ packaging instructions come from the index, not the author
//	unverified     ⚠ unsigned · first install, sha256 pinned
//
// level is one of store.TrustSigned / TrustHost / TrustIndexed /
// TrustUnverified (spelled as literals here so ui doesn't depend on store).
func Trust(level, detail string) {
	// detail is a signer name or a host domain — from the index or the ref,
	// so: foreign.
	detail = safe(detail)
	switch level {
	case "signed":
		if detail == "" {
			// Saying "signed" without naming the signer vouches for a key
			// nobody can identify. Iron law 8: no silent success — when you
			// can't say who, don't claim more than you know.
			line(paint(ansiBold+ansiRed, "⚠ claims to be signed but names no signer · treating as unverified"))
			return
		}
		line(paint(ansiGreen, "✓ signed") + " · " + detail)
	case "host":
		if detail == "" {
			detail = "unknown"
		}
		line(paint(ansiDim, "· from "+detail+" · unsigned"))
	case "index-toyfile":
		// Spell out which half is the author's and which half isn't. A vague
		// "partially trusted" says nothing — the user has to be able to judge
		// the risk from this line: the bytes installed are an asset from the
		// author's own release, but WHICH asset gets installed and what the
		// command is symlinked to come from the toyfile inlined in the index
		// entry.
		s := "⚠ packaging instructions come from the index · the bytes are from the author's own release, but which asset gets installed and what the command is called come from the index — the author never wrote a toy.toml"
		if detail != "" {
			s += " · " + detail
		}
		line(paint(ansiBold+ansiYellow, s))
	case "unverified":
		s := "⚠ unsigned · first install, sha256 pinned (trust on first use)"
		if detail != "" {
			s += " · " + detail
		}
		line(paint(ansiBold+ansiYellow, s))
	default:
		// Iron law 8: no silent success. An unrecognized level has to say so,
		// never get glossed over as if it were the safe tier.
		line(paint(ansiBold+ansiRed, fmt.Sprintf("⚠ unknown trust level %q, treating as unverified · %s", level, detail)))
	}
}

// Progress implements fetch.Sink. On a TTY it's a progress bar; off a TTY it's
// a handful of plain lines (one at the start, one at the end, throttled by
// percentage in between) and never a single \r.
type Progress struct {
	tty    bool
	bar    *progressbar.ProgressBar
	name   string
	total  int64
	cur    int64
	bucket int64 // highest 25% mark already printed on non-TTY
}

func NewProgress() *Progress { return newProgress(IsTTY()) }

// newProgress lets tests ask for a TTY-shaped instance without a real terminal.
func newProgress(tty bool) *Progress { return &Progress{tty: tty} }

func (p *Progress) Start(name string, total int64) {
	// name comes from toy.toml / the ref, so it's foreign; sanitize once here
	// and both Add and Done are clean.
	p.name, p.total, p.cur, p.bucket = safe(name), total, 0, 0
	if p.tty {
		// total<=0 means the server gave us no Content-Length (that's what a
		// chunked response looks like; fetch.open passes resp.ContentLength
		// straight through). progressbar uses -1 for "total unknown, draw a
		// spinner"; pass it 0 and it draws NOTHING — the whole download is
		// silent in the terminal, while the non-TTY path dutifully says
		// "size unknown".
		if total <= 0 {
			total = -1
		}
		p.bar = progressbar.NewOptions64(total,
			progressbar.OptionSetWriter(out),
			progressbar.OptionSetDescription(p.name),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionSetPredictTime(true),
			progressbar.OptionClearOnFinish(),
		)
		return
	}
	line("downloading " + p.name + " (" + humanBytes(total) + ")")
}

func (p *Progress) Add(n int64) {
	p.cur += n
	if p.tty {
		if p.bar != nil {
			_ = p.bar.Add64(n)
		}
		return
	}
	// Non-TTY: print a line only when we cross 25/50/75/100, and never a \r.
	// With the total unknown there is no percentage to speak of, so print
	// nothing and leave the result to Done.
	if p.total <= 0 {
		return
	}
	// When one Add crosses several marks at once, print only the highest: small
	// files often arrive in a single read, and filling in the marks below would
	// emit lines like "25% (148 B / 148 B)" — the percentage and the byte count
	// describe the same instant, so the two halves contradict each other.
	b := p.bucket
	for b < 100 && p.cur*100 >= p.total*(b+25) {
		b += 25
	}
	if b == p.bucket {
		return
	}
	p.bucket = b
	line(sprintf("  %s %d%% (%s / %s)", p.name, b, humanBytes(p.cur), humanBytes(p.total)))
}

func (p *Progress) Done(err error) {
	if p.tty && p.bar != nil {
		_ = p.bar.Finish()
		p.bar = nil
	}
	if err != nil {
		// err comes off the network: fetch's "HTTP %s" copies the server's
		// status line verbatim.
		line(paint(ansiRed, sprintf("✗ %s failed: %v", p.name, err)))
		return
	}
	if p.cur <= 0 {
		// Not a single byte counted — don't invent a size.
		line(paint(ansiGreen, "✓ "+p.name+" done"))
		return
	}
	line(paint(ansiGreen, sprintf("✓ %s done (%s)", p.name, humanBytes(p.cur))))
}

// humanBytes renders a byte count for humans, e.g. 12.3 MB. Decimal units
// (1000), so it matches the sizes shown on release pages.
func humanBytes(n int64) string {
	if n <= 0 {
		return "size unknown"
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v, units := float64(n), []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		v /= 1000
		if v < 1000 || u == "TB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%d B", n) // unreachable
}
