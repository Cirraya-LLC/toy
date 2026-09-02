package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// 回归：https 被重定向到 http 必须当场拒绝。
//
// 原本怀疑：客户端跟着降级到明文，中间人就能替换内容。
// 复核结论：成立 —— CheckRedirect 原来只数跳数，不看 scheme。已修。
// 现在这条守的是「https 一旦开始就不许落回 http」。对有 [sha256] 的 artifact
// 哈希还兜得住，但 GetBytes 拉 toy.toml 和索引时【没有任何哈希】，
// 而那正是决定"接下来下载什么、拿什么 hash 去比"的东西。
func TestRedirectHTTPSToHTTPIsRefused(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("payload-from-plaintext"))
	}))
	defer plain.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tlsSrv.Close()

	old := client
	client = tlsSrv.Client()
	client.CheckRedirect = old.CheckRedirect
	defer func() { client = old }()

	b, err := GetBytes(context.Background(), tlsSrv.URL+"/toy.toml", Options{})
	if err == nil {
		t.Fatalf("https 被静默降级到 %s，取回 %q", plain.URL, b)
	}
	// 报错必须说清楚是降级，不能只是一句光秃秃的"拿不到"——
	// 用户没法从后者推出"你的上游把我踢到明文上了"。
	if !strings.Contains(err.Error(), "downgrades") {
		t.Errorf("报错没说清是降级：%v", err)
	}
}

// 回归：反过来（http → https）是升级，必须放行。
// 一条把所有跨 scheme 重定向都拒掉的规则会打死 http→https 这种到处都是的正常配置。
func TestRedirectHTTPToHTTPSIsAllowed(t *testing.T) {
	upgraded := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secure"))
	}))
	defer upgraded.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upgraded.URL+"/x", http.StatusFound)
	}))
	defer plain.Close()

	old := client
	client = upgraded.Client()
	client.CheckRedirect = old.CheckRedirect
	defer func() { client = old }()

	b, err := GetBytes(context.Background(), plain.URL+"/toy.toml", Options{})
	if err != nil {
		t.Fatalf("http→https 是升级，不该被拒：%v", err)
	}
	if string(b) != "secure" {
		t.Fatalf("拿到 %q", b)
	}
}

// 回归：中间跳过一次 https 之后再落回 http，同样要拒。
// 只比"上一跳"的实现会漏掉这条 —— 链路的安全性取决于最弱的那一段，
// 字节在中间那一跳上已经可以被换掉了。
func TestRedirectDowngradeMidChainIsRefused(t *testing.T) {
	plain2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("payload-from-plaintext"))
	}))
	defer plain2.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain2.URL+"/z", http.StatusFound)
	}))
	defer secure.Close()
	plain1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secure.URL+"/y", http.StatusFound)
	}))
	defer plain1.Close()

	old := client
	client = secure.Client()
	client.CheckRedirect = old.CheckRedirect
	defer func() { client = old }()

	if b, err := GetBytes(context.Background(), plain1.URL+"/toy.toml", Options{}); err == nil {
		t.Fatalf("http→https→http 被放过了，取回 %q", b)
	}
}

// 回归：URL 里的密码不许出现在 error 对象里。
//
// 原本怀疑：凭据原样进报错。复核结论：成立。
// ui 那一层的 SafeURL 是兜底，不是修复 —— error 会进日志、进 `--json`、
// 进任何不经过 ui 的路径，所以必须在 fetch 拼错误的那一刻就抹掉。
// 用户名留着：只有密码是秘密，抹掉用户名反而让人对不上自己写的地址。
func TestCredentialsNeverReachErrors(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer dead.Close()

	u := strings.Replace(dead.URL, "http://", "http://alice:hunter2@", 1)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"GetBytes", func() error { _, err := GetBytes(context.Background(), u, Options{}); return err }},
		{"Get", func() error {
			_, err := Get(context.Background(), u, Options{CacheDir: t.TempDir()})
			return err
		}},
	} {
		err := tc.call()
		if err == nil {
			t.Fatalf("%s：应该失败", tc.name)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s：密码进了报错信息：%v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "alice") {
			t.Errorf("%s：用户名也被抹了，用户对不上自己写的地址：%v", tc.name, err)
		}
	}
}

// 回归：重定向的降级报错里同样不许带凭据。
// 这条错误是 fetch 自己拼的（不是 net/http 那个已经抹过的 *url.Error），
// 所以它是独立的一条泄漏路径。
func TestDowngradeErrorHasNoCredentials(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(plain.URL, "http://", "http://bob:s3cr3t@", 1)+"/x", http.StatusFound)
	}))
	defer tlsSrv.Close()

	old := client
	client = tlsSrv.Client()
	client.CheckRedirect = old.CheckRedirect
	defer func() { client = old }()

	_, err := GetBytes(context.Background(), tlsSrv.URL+"/toy.toml", Options{})
	if err == nil {
		t.Fatal("应该被拒")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("重定向目标里的密码进了报错：%v", err)
	}
}

// Result.Source 是【原样】地址，故意不抹：install 要拿它和请求的原始 URL 做等值
// 比较来判断"这份字节是不是来自镜像"，抹掉一边就永远不相等、每次都误报走了镜像。
// 抹在两个出口上（ui 打印、store 存清单），这里钉住这个契约，免得有人好心把它
// 一起 redact 掉、把镜像提示变成永远为真。
func TestResultSourceStaysVerbatimForMirrorComparison(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "http://", "http://alice:hunter2@", 1) + "/a.tar.gz"

	res, err := Get(context.Background(), u, Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != u {
		t.Fatalf("Source = %q，想要和请求地址一字不差的 %q", res.Source, u)
	}
}

// 回归：跨主机重定向不许把 Authorization 带过去。
//
// 原本怀疑：凭据跟着重定向去了第二台机器。
// 复核结论：不成立 —— net/http 的 makeHeadersCopier 在换主机时会丢掉敏感头，
// URL userinfo 派生的那个 Authorization 也在内。所以这里不是修复，是【钉住标准库
// 这个行为】：它一旦变了（或者哪天我们自己开始手写 Authorization 头），
// 用户写在地址里的密码就会被送给一台完全不相干的服务器。
func TestCredentialsDoNotSurviveCrossHostRedirect(t *testing.T) {
	var seen string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/x", http.StatusFound)
	}))
	defer first.Close()

	u := strings.Replace(first.URL, "http://", "http://alice:hunter2@", 1)
	if _, err := GetBytes(context.Background(), u, Options{}); err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Errorf("凭据跟着重定向去了第二台机器：%q", seen)
	}
}

// 回归：artifact 下载有体积上限，判据是【实际写进来的字节】。
//
// 原本怀疑：Get 没有任何上限，服务器分块吐多少就写多少，能把用户的盘写满。
// 复核结论：成立（GetBytes 有 maxBytes 挡着，Get 什么都没有）。已修：
// Options.MaxBytes，默认 DefaultMaxBytes = 2 GiB，和 archive.MaxTotalBytes 对齐。
//
// 两个子用例覆盖服务器自报长度的两种不可信形态：压根不报（chunked）和报一个很大的数。
// 反方向（声明得比实际小）不用测，因为它根本溢不出来 —— net/http 读响应体时本来
// 就以 Content-Length 为上限，多写的字节客户端看都看不到。真正危险的是"没有长度"
// 和"长度大得离谱"，而这两种都只能靠数真正落盘的字节来挡。
func TestArtifactDownloadIsCapped(t *testing.T) {
	const limit = 1 << 20

	for _, tc := range []struct {
		name    string
		declare bool // 老老实实声明一个 8 MiB 的 Content-Length
	}{
		{"chunked 不报长度", false},
		{"Content-Length 声明得很大", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.declare {
					w.Header().Set("Content-Length", strconv.Itoa(8*limit))
				}
				w.WriteHeader(200)
				buf := make([]byte, 64<<10)
				for sent.Load() < 8*limit {
					n, err := w.Write(buf)
					sent.Add(int64(n))
					if err != nil {
						return
					}
				}
			}))
			defer srv.Close()

			dir := t.TempDir()
			_, err := Get(context.Background(), srv.URL+"/x.tar.gz", Options{
				CacheDir: dir,
				MaxBytes: limit,
			})
			if err == nil {
				t.Fatal("超过上限居然成功了")
			}
			if !strings.Contains(err.Error(), "size limit") {
				t.Errorf("报错没说是体积上限：%v", err)
			}
			// 允许多冲几个缓冲区：LimitReader 卡在 limit+1 上，但服务器那边可能
			// 已经把几块写进了内核缓冲。要的是"有界"，不是"一个字节不差"。
			if got := sent.Load(); got > 4*limit {
				t.Errorf("服务器吐出 %d 字节才停，上限是 %d —— 没有真的挡住", got, limit)
			}
			// 半截文件不许留下：铁律 4 那条"不留残骸"对体积上限同样成立，
			// 否则一次攻击就能在缓存目录里留下上限那么大的垃圾。
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				t.Errorf("缓存目录里留下了半截文件 %q", e.Name())
			}
		})
	}
}

// 回归：没设 MaxBytes 时用默认上限，正常大小的包必须照旧装得上。
// 一条把所有人都挡住的上限不是上限，是故障。
func TestDefaultCapLetsNormalDownloadsThrough(t *testing.T) {
	if DefaultMaxBytes != 2<<30 {
		t.Fatalf("DefaultMaxBytes = %d，规格里说的是和 archive.MaxTotalBytes 一样的 2 GiB", DefaultMaxBytes)
	}
	body := strings.Repeat("z", 3<<20) // 3 MiB，比 GetBytes 的 32 MiB 小、比 1 MiB 大
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, strings.NewReader(body))
	}))
	defer srv.Close()

	res, err := Get(context.Background(), srv.URL+"/x.tar.gz", Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("默认上限把一个 3 MiB 的包挡住了：%v", err)
	}
	st, err := os.Stat(res.Path)
	if err != nil || st.Size() != int64(len(body)) {
		t.Fatalf("落地大小 %v（err %v），想要 %d", st, err, len(body))
	}
}

// 回归：HTTP 重定向跳不到 file:// 之类的别的 scheme 上。
//
// 原本怀疑：一个远端 302 就能让我们去读本地文件。
// 复核结论：不成立 —— Transport 上没注册 file 协议，跳过去只会拿到
// "unsupported protocol scheme"。这里钉住它，因为哪天有人给 Transport
// 注册 RegisterProtocol("file", …) 图方便，这个洞就真的开了。
func TestRedirectCannotJumpToFileScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()
	if b, err := GetBytes(context.Background(), srv.URL+"/toy.toml", Options{}); err == nil {
		t.Errorf("重定向跳到了 file://，取回 %q", b)
	}
}

// 回归：重定向次数有上限，死循环不会把客户端挂在那儿。
func TestRedirectLoopIsBounded(t *testing.T) {
	n := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Redirect(w, r, srv.URL+fmt.Sprintf("/next%d", n), http.StatusFound)
	}))
	defer srv.Close()
	if _, err := GetBytes(context.Background(), srv.URL+"/toy.toml", Options{}); err == nil {
		t.Fatal("死循环重定向居然成功了")
	}
	if n > maxRedirects+1 {
		t.Errorf("跟了 %d 次重定向，上限是 %d", n, maxRedirects)
	}
}
