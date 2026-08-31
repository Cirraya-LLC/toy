package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// 回归（铁律 4）：缓存命中必须重新算哈希，不能只看文件在不在。
//
// 原本怀疑：缓存只按文件名（= 声明的 hash）判命中，于是文件系统上任何人都能
// 造一个"名字对、内容错"的文件让它被直接装上去。
// 复核结论：不成立 —— Get 里那句 SumFile 已经在重算了。这条不是修复，是【钉住】
// 那次重算：它看起来像一次可以省掉的磁盘 IO，而省掉它就是把 ~/.toy/cache
// 变成一个谁都能写的信任根（那个目录还是 toy serve 的镜像源，规格 §6/§8）。
func TestAuditCacheHitRecomputesHash(t *testing.T) {
	good := []byte("the real artifact")
	want := auditSum(good)
	cache := t.TempDir()
	// 缓存里放一个【名字对、内容错】的文件：文件系统上谁都能造出这一个。
	if err := os.WriteFile(filepath.Join(cache, want), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(good)
	}))
	defer srv.Close()

	res, err := Get(context.Background(), srv.URL, Options{SHA256: want, CacheDir: cache})
	if err != nil {
		t.Fatalf("应该重新下载并成功，却报错：%v", err)
	}
	if res.FromCache {
		t.Fatal("铁律 4 被绕过：被篡改的缓存文件被当成命中直接返回了")
	}
	b, _ := os.ReadFile(res.Path)
	if string(b) != string(good) {
		t.Fatalf("落地的内容不是校验过的那份：%q", b)
	}
}

// 回归：声明的 sha256 大小写 / 前后空白 / 0x 前缀的处理，不能意外通过也不能意外失败。
//
// 原本怀疑：normalize 把畸形的 hash 放过去，或者反过来把大小写不同的合法 hash 拒掉。
// 复核结论：不成立，两个方向都对。这条守的是"宽进"和"严出"的那条线：
// 大小写和空白是作者手抄 toy.toml 时的常见噪声，必须容忍；而长度和字符集不能容忍，
// 因为这个字符串会被当成缓存里的文件名和镜像 URL 的路径段用。
func TestAuditSHA256Normalization(t *testing.T) {
	body := []byte("x")
	want := auditSum(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	cases := []struct {
		name, give string
		ok         bool
	}{
		{"原样", want, true},
		{"大写", strings.ToUpper(want), true},
		{"前后空白", "  " + want + "\n", true},
		{"0X 前缀", "0X" + want[2:], false},
		{"截断", want[:63], false},
	}
	for _, c := range cases {
		_, err := Get(context.Background(), srv.URL, Options{SHA256: c.give})
		if c.ok && err != nil {
			t.Errorf("%s：应该通过，却失败：%v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s：应该被拒，却通过了", c.name)
		}
	}
}

// 回归（铁律 4）：校验失败时缓存目录里不能留下残骸，尤其不能留下一个名字恰好是
// 合法哈希的半截文件。
//
// 原本怀疑：临时文件在校验失败后被留了下来。
// 复核结论：不成立 —— fetchOne 的 defer 用 placed 标志兜住了每一条失败路径。
// 这条钉住的是那个标志：只要有人新加一条 return 而忘了它，缓存目录里就会开始
// 长出半截文件，而缓存目录正是 toy serve 直接对外提供的那个目录。
func TestAuditFailedDownloadLeavesNoCacheFile(t *testing.T) {
	cache := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not what was promised"))
	}))
	defer srv.Close()

	_, err := Get(context.Background(), srv.URL, Options{SHA256: auditSum([]byte("promised")), CacheDir: cache})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("想要 ErrChecksum，拿到 %v", err)
	}
	ents, _ := os.ReadDir(cache)
	for _, e := range ents {
		t.Fatalf("缓存目录里留下了 %q", e.Name())
	}
}

// 回归（规格 §8）：没有 sha256 时不能用镜像 —— 拿不到哈希就没法验证镜像给的是什么。
//
// 原本怀疑：无哈希时也会去试镜像，等于让一个陌生人替上游发货。
// 复核结论：不成立，sources 里那个提前 return 就是干这个的。
// 镜像"不需要被信任"的全部前提就是内容可以按哈希核对；哈希不在的时候，
// 这个前提不成立，那条回退路径就必须整个消失。
func TestAuditNoMirrorsWithoutHash(t *testing.T) {
	got := sources("https://origin/a", Options{Mirrors: []string{"https://mirror"}})
	if len(got) != 1 {
		t.Fatalf("无哈希时不该给镜像，拿到 %v", got)
	}
}
