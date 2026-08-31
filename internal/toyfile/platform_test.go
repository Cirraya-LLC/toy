package toyfile

import (
	"errors"
	"strings"
	"testing"
)

const threeSeg = `name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64/musl" = "x_musl.tar.gz"
"linux/amd64/gnu"  = "x_gnu.tar.gz"
"linux/amd64"      = "x.tar.gz"
"linux/armv6"      = "x_armv6.tar.gz"
"linux/armv7"      = "x_armv7.tar.gz"
"darwin/amd64"     = "x_darwin.tar.gz"
`

// 三段 key（GOOS/GOARCH/libc）和 armv6 / armv7 这种代次 arch 必须收，
// 否则作者根本没法表达「这份是 musl 的」。
func TestAcceptsLibcAndARMGenerationKeys(t *testing.T) {
	if _, err := Parse([]byte(threeSeg)); err != nil {
		t.Fatalf("合法的三段 key / armv6 / armv7 被拒了：%v", err)
	}
}

// 但第三段只能是 libc。写别的不会报错、只会永远匹配不上 —— 和大小写写错
// 是同一类静默失效，所以照样当场拒绝（这条保护不许弱化）。
func TestRejectsUnknownThirdSegment(t *testing.T) {
	for _, plat := range []string{
		"linux/amd64/uclibc", "linux/amd64/v3", "linux/amd64/",
		"linux/amd64/musl/x", "linux//musl", "/amd64/musl",
	} {
		body := "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"" + plat + "\" = \"x.tar.gz\"\n"
		if tf, err := Parse([]byte(body)); err == nil {
			t.Errorf("平台 key %q 必须被拒绝，却解出了 %+v", plat, tf)
		}
	}
	// 报错要点名那一段，作者才知道该改哪里。
	_, err := Parse([]byte("name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64/uclibc\" = \"x.tar.gz\"\n"))
	if err == nil || !strings.Contains(err.Error(), "uclibc") {
		t.Errorf("报错里应当点名那一段：%v", err)
	}
}

func TestAssetForTakesTheFirstHit(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		candidates []string
		wantAsset  string
		wantMatch  string
	}{
		{"Alpine 拿 musl 那份", []string{"linux/amd64/musl", "linux/amd64"}, "x_musl.tar.gz", "linux/amd64/musl"},
		{"glibc 拿 gnu 那份", []string{"linux/amd64/gnu", "linux/amd64"}, "x_gnu.tar.gz", "linux/amd64/gnu"},
		{"没有 libc 专版就退回裸 key", []string{"linux/386/gnu", "linux/386", "linux/amd64"}, "x.tar.gz", "linux/amd64"},
		{"Pi 3 拿 armv7", []string{"linux/armv7", "linux/arm"}, "x_armv7.tar.gz", "linux/armv7"},
		{"Pi Zero 拿 armv6", []string{"linux/armv6", "linux/arm"}, "x_armv6.tar.gz", "linux/armv6"},
		{"Apple Silicon 走 Rosetta 回退", []string{"darwin/arm64", "darwin/amd64"}, "x_darwin.tar.gz", "darwin/amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asset, _, matched, err := tf.AssetFor(c.candidates, "1.0.0", "v1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if asset != c.wantAsset {
				t.Errorf("资产名 = %q，想要 %q", asset, c.wantAsset)
			}
			// 调用方要靠 matched 告诉用户走的是回退（铁律 8）。
			if matched != c.wantMatch {
				t.Errorf("命中的 key = %q，想要 %q", matched, c.wantMatch)
			}
		})
	}
}

// 顺序就是优先级：候选反过来就该命中另一个。写死「musl 优先」的实现过不了这条。
func TestAssetForRespectsCandidateOrder(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, matched, err := tf.AssetFor([]string{"linux/amd64", "linux/amd64/musl"}, "1.0.0", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if matched != "linux/amd64" {
		t.Errorf("应当命中候选里排在前面的 linux/amd64，拿到 %q", matched)
	}
}

// sha256 要跟着真正命中的那个 key 走 —— 拿错了会用 musl 的哈希去校验 gnu 的包，
// 而铁律 4 的下场是一律拒绝安装，看起来像是包被投毒了。
func TestAssetForReturnsTheMatchedPlatformsSHA(t *testing.T) {
	musl := strings.Repeat("a", 64)
	gnu := strings.Repeat("b", 64)
	tf, err := Parse([]byte(`name = "x"
[bin]
x = "x"
[artifact]
"linux/amd64/musl" = "x_musl.tar.gz"
"linux/amd64"      = "x.tar.gz"
[sha256]
"linux/amd64/musl" = "` + musl + `"
"linux/amd64"      = "` + gnu + `"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, sum, _, _ := tf.AssetFor([]string{"linux/amd64/musl", "linux/amd64"}, "1", "v1"); sum != musl {
		t.Errorf("musl 那份的 sha256 = %q", sum)
	}
	if _, sum, _, _ := tf.AssetFor([]string{"linux/amd64"}, "1", "v1"); sum != gnu {
		t.Errorf("裸 key 那份的 sha256 = %q", sum)
	}
	// 回退发生时最容易写错：哈希必须取自真正命中的那个 key，不是首选候选。
	if _, sum, _, _ := tf.AssetFor([]string{"linux/386", "linux/amd64"}, "1", "v1"); sum != gnu {
		t.Errorf("回退命中 linux/amd64 时应当拿它自己的 sha256，拿到 %q", sum)
	}
}

// 一个都不命中时，用户要知道我们试过什么、作者要知道该补哪个 key。
func TestAssetForListsTriedAndDeclared(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = tf.AssetFor([]string{"openbsd/riscv64"}, "1.0.0", "v1.0.0")
	if !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("应当是 ErrNoPlatform：%v", err)
	}
	for _, want := range []string{"openbsd/riscv64", "linux/amd64/musl", "darwin/amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%s", want, err)
		}
	}
	// 空候选不许 panic。
	if _, _, _, err := tf.AssetFor(nil, "1.0.0", "v1.0.0"); !errors.Is(err, ErrNoPlatform) {
		t.Errorf("空候选应当是 ErrNoPlatform：%v", err)
	}
}

// script 形态没有按平台区分的资产，AssetFor 也要明确说清而不是给个空串。
func TestAssetForRejectsScriptForm(t *testing.T) {
	tf, err := Parse([]byte("name = \"x\"\n[script]\nx = \"x.sh\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tf.AssetFor([]string{"linux/amd64"}, "1", "v1"); err == nil {
		t.Error("对 script 形态调 AssetFor 应当报错")
	}
}

// 展开占位符之后仍然要过 checkRelPath —— 回退这一层不许绕开铁律 7。
// （{tag} 是上游给的，一个叫 ../../evil 的 tag 能把下载地址指到别的仓库。）
func TestAssetForStillChecksExpandedPath(t *testing.T) {
	tf, err := Parse([]byte("name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64/musl\" = \"x_{tag}.tar.gz\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = tf.AssetFor([]string{"linux/amd64/musl"}, "1.0.0", "../../../../evil/releases/download/v9/pwn")
	if !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("展开之后含 .. 的资产名必须被拒：%v", err)
	}
}

// 三段 key 也要排得下，而且顺序稳定（map 遍历顺序是随机的）。
func TestPlatformsSortsThreeSegmentKeys(t *testing.T) {
	tf, err := Parse([]byte(threeSeg))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"darwin/amd64", "linux/amd64", "linux/amd64/gnu", "linux/amd64/musl",
		"linux/armv6", "linux/armv7",
	}
	for i := 0; i < 20; i++ {
		got := tf.Platforms()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("Platforms() = %v，想要 %v", got, want)
		}
	}
}
