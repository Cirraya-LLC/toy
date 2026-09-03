package store

import "testing"

// Downgrade order: signed > host > index-toyfile > unverified > unrecognized.
// The inlined-toyfile level sits between host and unverified and must equal
// neither side.
func TestTrustRankOrder(t *testing.T) {
	order := []string{TrustSigned, TrustHost, TrustIndexed, TrustUnverified, "who-knows-what"}
	for i := 1; i < len(order); i++ {
		if TrustRank(order[i-1]) <= TrustRank(order[i]) {
			t.Errorf("%q must rank strictly above %q, got %d vs %d",
				order[i-1], order[i], TrustRank(order[i-1]), TrustRank(order[i]))
		}
	}
	// An unrecognized level ranks lowest (iron law 8: never fudge it into the safe end).
	if TrustRank("") != 0 || TrustRank("signed ") != 0 {
		t.Error("an unrecognized trust level must rank lowest")
	}
}
