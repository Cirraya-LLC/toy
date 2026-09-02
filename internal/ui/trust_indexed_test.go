package ui

import (
	"strings"
	"testing"
)

// 铁律 8：这一档必须说清楚哪一半是作者的、哪一半是索引的，
// 不许用「部分可信」这种含糊说法糊过去，也不许长得像已签名。
func TestTrustIndexedSaysWhichHalfIsTheAuthors(t *testing.T) {
	plainOutput(t)
	out := capture(t, func() { Trust("index-toyfile", "index default") })
	for _, want := range []string{"⚠", "index", "author", "release"} {
		if !strings.Contains(out, want) {
			t.Errorf("这一档的文案里应该有 %q：%s", want, out)
		}
	}
	if strings.Contains(out, "✓") {
		t.Errorf("内联 toyfile 那一档不能出现 ✓：%s", out)
	}
	// 规格要求说清楚，不许用含糊词把「哪一半不是作者的」糊过去。
	for _, vague := range []string{"partially trusted", "mostly trusted", "probably", "should be fine"} {
		if strings.Contains(out, vague) {
			t.Errorf("文案里出现了含糊说法 %q：%s", vague, out)
		}
	}
	if strings.Contains(out, "unknown trust level") {
		t.Errorf("index-toyfile 掉进了 default 分支：%s", out)
	}
}
