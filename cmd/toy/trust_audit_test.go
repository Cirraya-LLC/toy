package main

// Auditing iron law 8: the trust column in `toy list` is the only thing the user
// has to judge by. This one needs no control characters at all — "name has no
// length limit" is enough to make it lie.

import (
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/ui"

	"github.com/Cirraya-LLC/toy/internal/store"
)

// A package can forge an entire row in `toy list` out of its own name, and push
// the real trust column of EVERY package off the screen while it is at it.
//
// qryTable uses tabwriter, so a column is as wide as its widest cell plus 2, and
// name comes straight from toy.toml (toyfile.checkName only rejects control
// characters and path separators: no length limit, no opinion on how many
// consecutive spaces it holds) through store.Manifest.Name into this cell, with
// no truncation anywhere along the way — the qryTrunc(qryDescWidth) over in
// `toy search` only clips the description column, list clips nothing.
//
// Consequence: stuff a hand-arranged "version / ✓ signed / source" into name,
// pad it past the terminal width, and on any terminal narrower than the name:
//   - this package renders as "cowsay  v3.2.1  ✓ signed 9f3c1a20  Derrity/cowsay"
//   - its real "⚠ unsigned" is pushed past the wrap (`toy list | less -S`, or
//     inside tmux, cuts it off outright)
//   - and it takes the trust column of an honest package like jq in the same
//     table off the screen with it
func TestListNameCanForgeItsOwnRowAndHideEveryTrustColumn(t *testing.T) {
	const width = 100 // a very common terminal width

	forged := "cowsay    v3.2.1  " + qryTrustLabel(store.TrustSigned, "9f3c1a20") + "  Derrity/cowsay"
	name := forged + strings.Repeat(" ", 3*width) // padded past the terminal width

	rows := [][]string{
		{"name", "version", "trust", "source"},
		{"jq", "1.7.1", qryTrustLabel(store.TrustSigned, "aabb1122"), "github:jqlang/jq"},
		{name, "0.0.1", qryTrustLabel(store.TrustUnverified, ""), "https://evil.example/toy.toml"},
	}

	out := captureStderr(t, func() { qryTable(rows) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("the table should be 3 lines, got %d: %q", len(lines), lines)
	}
	legit, evil := lines[1], lines[2]

	// These assert the behaviour that MUST hold: in any row of the table the
	// trust column has to land somewhere an ordinary terminal can see, and name
	// is not allowed to push it out.
	head := visibleCols(evil, width)
	if !strings.Contains(head, "unsigned") {
		t.Errorf("the malicious package's real trust level was pushed past column %d, and the first screen shows a forged signed record:\n  %s", width, head)
	}
	if strings.Contains(head, "✓ signed 9f3c1a20") {
		t.Errorf("the fake trust level stuffed into name got rendered as body text:\n  %s", head)
	}
	// Collateral damage: the honest package in the same table lost its trust
	// column too — one malicious package makes the trust level of EVERY package
	// disappear from the screen.
	if !strings.Contains(visibleCols(legit, width), "aabb1122") {
		t.Errorf("jq is an honest package and that name pushed its trust column past column %d too:\n  %s", width, visibleCols(legit, width))
	}
}

// visibleCols returns the part of a line a terminal of the given width shows on
// the first screen (approximated by rune, and CJK characters really occupy two
// cells, so reality is only ever worse than this).
func visibleCols(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

// captureStderr collects what ui prints while fn runs.
//
// This used to swap file descriptor 2, on the belief that ui bound os.Stderr at
// init time and could not be intercepted any other way. ui.out is an io.Writer
// package variable, so it can -- and the fd version needed syscall.Dup3, which
// does not exist on macOS, so the whole test package failed to compile there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	var b strings.Builder
	defer ui.SetOutput(&b)()
	fn()
	return b.String()
}
