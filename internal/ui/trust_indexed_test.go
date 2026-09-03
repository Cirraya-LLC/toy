package ui

import (
	"strings"
	"testing"
)

// Iron law 8: this tier has to spell out which half is the author's and which
// half is the index's. No hedging behind "partially trusted", and it must never
// look like a signed install.
func TestTrustIndexedSaysWhichHalfIsTheAuthors(t *testing.T) {
	plainOutput(t)
	out := capture(t, func() { Trust("index-toyfile", "index default") })
	for _, want := range []string{"⚠", "index", "author", "release"} {
		if !strings.Contains(out, want) {
			t.Errorf("this tier's wording should mention %q: %s", want, out)
		}
	}
	if strings.Contains(out, "✓") {
		t.Errorf("the inline-toyfile tier must never show a ✓: %s", out)
	}
	// The spec demands it be explicit; no vague word may paper over "which half
	// isn't the author's".
	for _, vague := range []string{"partially trusted", "mostly trusted", "probably", "should be fine"} {
		if strings.Contains(out, vague) {
			t.Errorf("found the hedge %q in the wording: %s", vague, out)
		}
	}
	if strings.Contains(out, "unknown trust level") {
		t.Errorf("index-toyfile fell through to the default branch: %s", out)
	}
}
