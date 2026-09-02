package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// serve 起一个只认几条固定路径的测试服务器，路径 → 内容。
func serve(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// recordSink 记下回调，用来确认 Sink 真的被喂了东西。
type recordSink struct {
	starts int
	total  int64
	added  int64
	dones  int
	lastEr error
}

func (s *recordSink) Start(_ string, total int64) { s.starts++; s.total = total }
func (s *recordSink) Add(n int64)                 { s.added += n }
func (s *recordSink) Done(err error)              { s.dones++; s.lastEr = err }

func TestGet(t *testing.T) {
	payload := "toy binary payload"
	srv := serve(t, map[string]string{"/ok": payload})

	cases := []struct {
		name    string
		path    string
		sha     string
		wantErr error
		wantOK  bool
	}{
		{name: "无 hash 也能装", path: "/ok", wantOK: true},
		{name: "hash 对得上", path: "/ok", sha: sum([]byte(payload)), wantOK: true},
		{name: "hash 对不上", path: "/ok", sha: sum([]byte("别的东西")), wantErr: ErrChecksum},
		{name: "404", path: "/nope", wantErr: ErrAllFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			// Sink 为 nil 必须全程不崩
			res, err := Get(context.Background(), srv.URL+tc.path, Options{SHA256: tc.sha, CacheDir: cache})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("想成功，却是 %v", err)
				}
				if res.SHA256 != sum([]byte(payload)) {
					t.Fatalf("hash 不对：%s", res.SHA256)
				}
				if res.FromCache {
					t.Fatal("第一次下载不该说是缓存命中")
				}
				if res.Source != srv.URL+tc.path {
					t.Fatalf("Source 该是实际成功的地址，却是 %q", res.Source)
				}
				got, _ := os.ReadFile(res.Path)
				if string(got) != payload {
					t.Fatalf("内容不对：%q", got)
				}
				if filepath.Base(res.Path) != res.SHA256 {
					t.Fatalf("缓存里的文件名必须是内容 hash，却是 %q", filepath.Base(res.Path))
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("想要 %v，却是 %v", tc.wantErr, err)
			}
			// 失败时缓存目录必须是空的：绝不能留半个文件或临时文件
			ents, _ := os.ReadDir(cache)
			if len(ents) != 0 {
				t.Fatalf("失败后缓存目录不干净：%v", ents)
			}
		})
	}
}

func TestGetCache(t *testing.T) {
	payload := []byte("cached payload")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(payload)
	}))
	defer srv.Close()

	cache := t.TempDir()
	opt := Options{SHA256: sum(payload), CacheDir: cache}

	first, err := Get(context.Background(), srv.URL+"/x", opt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Get(context.Background(), srv.URL+"/x", opt)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache || second.Path != first.Path {
		t.Fatalf("第二次该命中缓存：%+v", second)
	}
	if hits != 1 {
		t.Fatalf("命中缓存不该再请求，实际请求了 %d 次", hits)
	}

	// 缓存文件被改坏 → 重新算的 hash 对不上 → 不算命中，重新下载
	if err := os.WriteFile(first.Path, []byte("坏掉的缓存"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := Get(context.Background(), srv.URL+"/x", opt)
	if err != nil {
		t.Fatal(err)
	}
	if third.FromCache {
		t.Fatal("内容对不上的缓存文件不该算命中")
	}
	if hits != 2 {
		t.Fatalf("该重新下载一次，实际请求了 %d 次", hits)
	}
	if got, _ := os.ReadFile(third.Path); string(got) != string(payload) {
		t.Fatalf("缓存没被修好：%q", got)
	}
}

func TestGetMirror(t *testing.T) {
	payload := []byte("mirrored payload")
	want := sum(payload)

	var mirrorHits int32
	var gotPath string
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mirrorHits, 1)
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/m/" + want:
			w.Write(payload)
		default:
			w.Write([]byte("镜像上放的是别的东西"))
		}
	}))
	defer mirror.Close()
	origin := serve(t, map[string]string{"/have": string(payload)})

	t.Run("原始地址挂了就走镜像", func(t *testing.T) {
		atomic.StoreInt32(&mirrorHits, 0)
		res, err := Get(context.Background(), origin.URL+"/gone", Options{
			SHA256: want, Mirrors: []string{mirror.URL + "/m/"}, CacheDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		// 镜像地址 = base 去掉尾斜杠 + "/" + sha256
		if gotPath != "/m/"+want {
			t.Fatalf("镜像地址拼错了：%q", gotPath)
		}
		if res.Source != mirror.URL+"/m/"+want {
			t.Fatalf("Source 该指向真正成功的镜像，却是 %q", res.Source)
		}
	})

	t.Run("镜像内容不对一样拒绝", func(t *testing.T) {
		cache := t.TempDir()
		_, err := Get(context.Background(), origin.URL+"/gone", Options{
			SHA256: sum([]byte("谁都没有的内容")), Mirrors: []string{mirror.URL + "/m"}, CacheDir: cache,
		})
		if !errors.Is(err, ErrAllFailed) || !errors.Is(err, ErrChecksum) {
			t.Fatalf("镜像内容要独立验一次 hash，得到 %v", err)
		}
		if ents, _ := os.ReadDir(cache); len(ents) != 0 {
			t.Fatalf("缓存目录该是空的：%v", ents)
		}
	})

	t.Run("没有 hash 就不碰镜像", func(t *testing.T) {
		atomic.StoreInt32(&mirrorHits, 0)
		_, err := Get(context.Background(), origin.URL+"/gone", Options{
			Mirrors: []string{mirror.URL + "/m/"}, CacheDir: t.TempDir(),
		})
		if !errors.Is(err, ErrAllFailed) {
			t.Fatalf("想要 ErrAllFailed，得到 %v", err)
		}
		if mirrorHits != 0 {
			t.Fatalf("没有 hash 就无法验证镜像内容，不该去请求它（请求了 %d 次）", mirrorHits)
		}
	})
}

func TestGetNoCacheDir(t *testing.T) {
	payload := []byte("no cache dir")
	srv := serve(t, map[string]string{"/x": string(payload)})
	res, err := Get(context.Background(), srv.URL+"/x", Options{SHA256: sum(payload)})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(res.Path)
	if got, _ := os.ReadFile(res.Path); string(got) != string(payload) {
		t.Fatalf("内容不对：%q", got)
	}
}

func TestGetFileScheme(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.bin")
	payload := []byte("本地文件也走同一条路")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	res, err := Get(context.Background(), "file://"+src, Options{SHA256: sum(payload), CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA256 != sum(payload) {
		t.Fatalf("hash 不对：%s", res.SHA256)
	}
	if _, err := Get(context.Background(), "file://"+dir+"/没有这个文件", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("想要 ErrAllFailed，得到 %v", err)
	}
}

func TestGetSink(t *testing.T) {
	payload := []byte("给进度条看的内容")
	srv := serve(t, map[string]string{"/x": string(payload)})

	s := &recordSink{}
	if _, err := Get(context.Background(), srv.URL+"/x", Options{Sink: s, Name: "toy", CacheDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if s.starts != 1 || s.dones != 1 || s.lastEr != nil {
		t.Fatalf("Sink 回调不对：%+v", s)
	}
	if s.added != int64(len(payload)) || s.total != int64(len(payload)) {
		t.Fatalf("字节数不对：added=%d total=%d", s.added, s.total)
	}

	bad := &recordSink{}
	_, err := Get(context.Background(), srv.URL+"/x", Options{Sink: bad, SHA256: sum([]byte("别的")), CacheDir: t.TempDir()})
	if err == nil || bad.dones != 1 || bad.lastEr == nil {
		t.Fatalf("失败也要 Done(err)：%+v", bad)
	}
}

func TestBadSHA256Format(t *testing.T) {
	// sha256 会被当成缓存里的文件名，畸形值必须在下载之前就被拒
	for _, bad := range []string{"../../etc/passwd", "abc", strings.Repeat("z", 64)} {
		if _, err := Get(context.Background(), "http://example.invalid/x", Options{SHA256: bad, CacheDir: t.TempDir()}); err == nil {
			t.Fatalf("%q 该被拒", bad)
		}
		if _, err := GetBytes(context.Background(), "http://example.invalid/x", Options{SHA256: bad}); err == nil {
			t.Fatalf("%q 该被拒", bad)
		}
	}
	// 大写和空白要被容忍，不能当成校验失败
	payload := []byte("大写 hash")
	srv := serve(t, map[string]string{"/x": string(payload)})
	if _, err := Get(context.Background(), srv.URL+"/x", Options{SHA256: " " + strings.ToUpper(sum(payload)) + " ", CacheDir: t.TempDir()}); err != nil {
		t.Fatalf("大写/带空白的 hash 该照样通过：%v", err)
	}
}

func TestRedirect(t *testing.T) {
	payload := []byte("跳一次就到")
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real", http.StatusFound)
	})
	mux.HandleFunc("/real", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop2", http.StatusFound)
	})
	mux.HandleFunc("/loop2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := Get(context.Background(), srv.URL+"/start", Options{SHA256: sum(payload), CacheDir: t.TempDir()}); err != nil {
		t.Fatalf("正常重定向该跟随：%v", err)
	}
	cache := t.TempDir()
	if _, err := Get(context.Background(), srv.URL+"/loop", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("死循环重定向该失败，得到 %v", err)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("缓存目录该是空的：%v", ents)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestGetBytes(t *testing.T) {
	payload := "name = \"tinyserve\"\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/toy.toml", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) })
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeroReader{}, maxBytes+4096)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "index.json")
	if err := os.WriteFile(local, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		url     string
		sha     string
		wantErr error
		wantAny bool // 只要求出错，不指定哨兵
	}{
		{name: "https", url: srv.URL + "/toy.toml"},
		{name: "file://", url: "file://" + local},
		{name: "hash 对得上", url: srv.URL + "/toy.toml", sha: sum([]byte(payload))},
		{name: "hash 对不上", url: srv.URL + "/toy.toml", sha: sum([]byte("x")), wantErr: ErrChecksum},
		{name: "404", url: srv.URL + "/nope", wantErr: ErrAllFailed},
		{name: "超过上限", url: srv.URL + "/huge", wantErr: ErrAllFailed, wantAny: true},
		{name: "file:// 不存在", url: "file://" + dir + "/nope", wantErr: ErrAllFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := GetBytes(context.Background(), tc.url, Options{SHA256: tc.sha})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("想要 %v，得到 %v", tc.wantErr, err)
				}
				if tc.wantAny && !strings.Contains(err.Error(), "size limit") {
					t.Fatalf("报错该说清楚是超了上限：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != payload {
				t.Fatalf("内容不对：%q", b)
			}
		})
	}
}

func TestSumFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SumFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := sum([]byte("hello")); got != want {
		t.Fatalf("想要 %s，得到 %s", want, got)
	}
	if _, err := SumFile(filepath.Join(dir, "没有")); err == nil {
		t.Fatal("不存在的文件该报错")
	}
}

func TestCancelledContext(t *testing.T) {
	srv := serve(t, map[string]string{"/x": "内容"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := t.TempDir()
	if _, err := Get(ctx, srv.URL+"/x", Options{CacheDir: cache}); err == nil {
		t.Fatal("已取消的 ctx 该失败")
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("缓存目录该是空的：%v", ents)
	}
}

// 服务器带 Content-Encoding: gzip 时，落地和 hash 的必须是线上的原始字节。
// Go 的 transport 默认会自己加 Accept-Encoding: gzip 再自己解开，那样算出来的
// hash 是解压后的内容，跟发布者对着 release 资产算的 hash 永远对不上。
func TestHashesTheBytesOnTheWire(t *testing.T) {
	raw := []byte("release 资产的真实字节")
	var wire bytes.Buffer
	gz := gzip.NewWriter(&wire)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); strings.Contains(ae, "gzip") {
			t.Errorf("不该主动要 gzip，却发了 Accept-Encoding: %q", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(wire.Bytes())
	}))
	defer srv.Close()

	// 有 sha256（发布者对着线上文件算的）时必须验得过
	res, err := Get(context.Background(), srv.URL+"/x", Options{
		SHA256: sum(wire.Bytes()), CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("发布者的 hash 是对着线上字节算的，该通过：%v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, wire.Bytes()) {
		t.Fatalf("落地的不是线上字节（被透明解压了）：%d 字节", len(got))
	}

	// 没有 sha256 的 TOFU 路径同样不能钉住解压后的 hash
	res2, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res2.SHA256 != sum(wire.Bytes()) {
		t.Fatalf("TOFU 钉住的 hash 不是线上字节的：%s", res2.SHA256)
	}
}

// file:// 里的 ? 和 # 会被 url.Parse 切掉，导致静默打开另一个文件。宁可报错也不猜。
func TestFileURLRefusesQueryAndFragment(t *testing.T) {
	dir := t.TempDir()
	// 陷阱：a 和 a#b 同时存在，要 a#b 绝不能拿到 a 的内容
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("别的文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a#b"), []byte("我要的文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a#b", "a?b"} {
		b, err := GetBytes(context.Background(), "file://"+filepath.Join(dir, name), Options{})
		if err == nil {
			t.Fatalf("%q 该被拒，却读到了 %q", name, b)
		}
		if !strings.Contains(err.Error(), "?") {
			t.Fatalf("报错该说清楚是 ? / # 的事：%v", err)
		}
	}
}

// 下载到一半断线 / 被取消 / 非 200：一律不能在缓存目录里留下任何东西。
func TestNothingLeftBehindOnFailure(t *testing.T) {
	t.Run("响应被截断", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("只发五个字"))
		}))
		defer srv.Close()
		cache := t.TempDir()
		if _, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: cache}); err == nil {
			t.Fatal("截断的响应不该算成功")
		}
		assertEmptyDir(t, cache)
	})

	t.Run("下载中途取消", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < 200; i++ {
				if _, err := w.Write(make([]byte, 4096)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(2 * time.Millisecond)
			}
		}))
		defer srv.Close()
		cache := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		s := &recordSink{}
		if _, err := Get(ctx, srv.URL+"/x", Options{CacheDir: cache, Sink: s}); err == nil {
			t.Fatal("取消了该失败")
		}
		if s.starts != s.dones || s.lastEr == nil {
			t.Fatalf("中途取消也要 Done(err)：%+v", s)
		}
		assertEmptyDir(t, cache)
	})

	// 只有 200 算数：204/206 有 body，直接当成功会装上一个半截文件
	for _, code := range []int{204, 206, 301, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte("这不是包"))
			}))
			defer srv.Close()
			cache := t.TempDir()
			if _, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
				t.Fatalf("HTTP %d 该失败，得到 %v", code, err)
			}
			assertEmptyDir(t, cache)
		})
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("留下了残骸 %q（临时文件也算：cache 目录会被 toy serve 当镜像发出去）", e.Name())
	}
}

// 原始地址被投毒（200 但内容不对）时，镜像必须能把这次安装救回来，
// 而且 Sink 的 Start/Done 要成对 —— ui 的进度条靠这个收尾。
func TestPoisonedOriginFallsBackToMirror(t *testing.T) {
	payload := []byte("真的包")
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer mirror.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("投毒内容"))
	}))
	defer origin.Close()

	s := &recordSink{}
	res, err := Get(context.Background(), origin.URL+"/x", Options{
		SHA256: sum(payload), Mirrors: []string{mirror.URL}, CacheDir: t.TempDir(), Sink: s,
	})
	if err != nil {
		t.Fatalf("镜像上有对的内容，该救回来：%v", err)
	}
	if res.Source != mirror.URL+"/"+sum(payload) {
		t.Fatalf("Source 必须如实说是镜像救的（铁律 8），却是 %q", res.Source)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, payload) {
		t.Fatalf("装上去的是投毒内容：%q", got)
	}
	if s.starts != s.dones || s.starts != 2 {
		t.Fatalf("两次尝试就该有两对 Start/Done：%+v", s)
	}
	if s.lastEr != nil {
		t.Fatalf("最后一次是成功的，Done 不该带 err：%v", s.lastEr)
	}
}

// 同一个包被并发装多次：缓存里只能有一个文件，且每个调用方拿到的内容都是完整的。
func TestConcurrentGetSharesOneCache(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	cache := t.TempDir()
	opt := Options{SHA256: sum(payload), CacheDir: cache}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Get(context.Background(), srv.URL+"/x", opt)
			if err != nil {
				t.Errorf("并发下失败：%v", err)
				return
			}
			got, err := os.ReadFile(res.Path)
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("并发下读到半个文件：%d 字节 %v", len(got), err)
			}
		}()
	}
	wg.Wait()

	ents, _ := os.ReadDir(cache)
	if len(ents) != 1 || ents[0].Name() != sum(payload) {
		t.Fatalf("缓存里该只有一个以 hash 命名的文件，实际 %v", ents)
	}
}

// file:// 指向目录不能被当成一个包。
func TestFileURLDirectory(t *testing.T) {
	cache := t.TempDir()
	if _, err := Get(context.Background(), "file://"+t.TempDir(), Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("file:// 指向目录该失败，得到 %v", err)
	}
	assertEmptyDir(t, cache)
}
