package store

import "testing"

// 降档顺序：signed > host > index-toyfile > unverified > 认不出的。
// 内联 toyfile 那一档卡在 host 和 unverified 中间，两边都不能等于。
func TestTrustRankOrder(t *testing.T) {
	order := []string{TrustSigned, TrustHost, TrustIndexed, TrustUnverified, "谁知道这是什么"}
	for i := 1; i < len(order); i++ {
		if TrustRank(order[i-1]) <= TrustRank(order[i]) {
			t.Errorf("%q 应该严格高于 %q，得到 %d vs %d",
				order[i-1], order[i], TrustRank(order[i-1]), TrustRank(order[i]))
		}
	}
	// 认不出的等级排最低（铁律 8：不许当成安全的那一档糊过去）。
	if TrustRank("") != 0 || TrustRank("signed ") != 0 {
		t.Error("认不出的信任等级必须排在最低")
	}
}
