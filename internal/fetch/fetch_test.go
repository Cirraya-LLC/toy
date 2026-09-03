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

// serve starts a test server that knows a handful of fixed paths: path -> content.
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

// recordSink records the callbacks, so we can confirm the Sink really was fed.
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
		{name: "installs fine without a hash", path: "/ok", wantOK: true},
		{name: "hash matches", path: "/ok", sha: sum([]byte(payload)), wantOK: true},
		{name: "hash does not match", path: "/ok", sha: sum([]byte("something else")), wantErr: ErrChecksum},
		{name: "404", path: "/nope", wantErr: ErrAllFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			// A nil Sink must never panic anywhere along the way.
			res, err := Get(context.Background(), srv.URL+tc.path, Options{SHA256: tc.sha, CacheDir: cache})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("wanted success, got %v", err)
				}
				if res.SHA256 != sum([]byte(payload)) {
					t.Fatalf("wrong hash: %s", res.SHA256)
				}
				if res.FromCache {
					t.Fatal("a first download must not claim to be a cache hit")
				}
				if res.Source != srv.URL+tc.path {
					t.Fatalf("Source should be the address that actually succeeded, got %q", res.Source)
				}
				got, _ := os.ReadFile(res.Path)
				if string(got) != payload {
					t.Fatalf("wrong content: %q", got)
				}
				if filepath.Base(res.Path) != res.SHA256 {
					t.Fatalf("a cached file must be named after its content hash, got %q", filepath.Base(res.Path))
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("wanted %v, got %v", tc.wantErr, err)
			}
			// On failure the cache directory must be empty: never leave half a file or a temp file behind.
			ents, _ := os.ReadDir(cache)
			if len(ents) != 0 {
				t.Fatalf("cache directory is not clean after a failure: %v", ents)
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
		t.Fatalf("the second call should hit the cache: %+v", second)
	}
	if hits != 1 {
		t.Fatalf("a cache hit must not hit the network again, but there were %d requests", hits)
	}

	// Corrupt the cached file -> rehashing no longer matches -> not a hit, download again.
	if err := os.WriteFile(first.Path, []byte("corrupted cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := Get(context.Background(), srv.URL+"/x", opt)
	if err != nil {
		t.Fatal(err)
	}
	if third.FromCache {
		t.Fatal("a cached file whose content no longer matches must not count as a hit")
	}
	if hits != 2 {
		t.Fatalf("it should have downloaded again, but there were %d requests", hits)
	}
	if got, _ := os.ReadFile(third.Path); string(got) != string(payload) {
		t.Fatalf("the cache was not repaired: %q", got)
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
			w.Write([]byte("the mirror is serving something else"))
		}
	}))
	defer mirror.Close()
	origin := serve(t, map[string]string{"/have": string(payload)})

	t.Run("falls back to the mirror when the original is gone", func(t *testing.T) {
		atomic.StoreInt32(&mirrorHits, 0)
		res, err := Get(context.Background(), origin.URL+"/gone", Options{
			SHA256: want, Mirrors: []string{mirror.URL + "/m/"}, CacheDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		// A mirror URL is base with the trailing slash stripped + "/" + sha256.
		if gotPath != "/m/"+want {
			t.Fatalf("mirror URL was built wrong: %q", gotPath)
		}
		if res.Source != mirror.URL+"/m/"+want {
			t.Fatalf("Source should point at the mirror that actually succeeded, got %q", res.Source)
		}
	})

	t.Run("a mirror with the wrong content is refused too", func(t *testing.T) {
		cache := t.TempDir()
		_, err := Get(context.Background(), origin.URL+"/gone", Options{
			SHA256: sum([]byte("content nobody has")), Mirrors: []string{mirror.URL + "/m"}, CacheDir: cache,
		})
		if !errors.Is(err, ErrAllFailed) || !errors.Is(err, ErrChecksum) {
			t.Fatalf("mirror content must be hashed independently, got %v", err)
		}
		if ents, _ := os.ReadDir(cache); len(ents) != 0 {
			t.Fatalf("the cache directory should be empty: %v", ents)
		}
	})

	t.Run("no hash means no mirrors", func(t *testing.T) {
		atomic.StoreInt32(&mirrorHits, 0)
		_, err := Get(context.Background(), origin.URL+"/gone", Options{
			Mirrors: []string{mirror.URL + "/m/"}, CacheDir: t.TempDir(),
		})
		if !errors.Is(err, ErrAllFailed) {
			t.Fatalf("wanted ErrAllFailed, got %v", err)
		}
		if mirrorHits != 0 {
			t.Fatalf("without a hash there is no way to verify mirror content, so it must not be requested (it was, %d times)", mirrorHits)
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
		t.Fatalf("wrong content: %q", got)
	}
}

func TestGetFileScheme(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.bin")
	payload := []byte("local files take the same path")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	res, err := Get(context.Background(), "file://"+src, Options{SHA256: sum(payload), CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA256 != sum(payload) {
		t.Fatalf("wrong hash: %s", res.SHA256)
	}
	if _, err := Get(context.Background(), "file://"+dir+"/no-such-file", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("wanted ErrAllFailed, got %v", err)
	}
}

func TestGetSink(t *testing.T) {
	payload := []byte("content for the progress bar")
	srv := serve(t, map[string]string{"/x": string(payload)})

	s := &recordSink{}
	if _, err := Get(context.Background(), srv.URL+"/x", Options{Sink: s, Name: "toy", CacheDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if s.starts != 1 || s.dones != 1 || s.lastEr != nil {
		t.Fatalf("wrong Sink callbacks: %+v", s)
	}
	if s.added != int64(len(payload)) || s.total != int64(len(payload)) {
		t.Fatalf("wrong byte counts: added=%d total=%d", s.added, s.total)
	}

	bad := &recordSink{}
	_, err := Get(context.Background(), srv.URL+"/x", Options{Sink: bad, SHA256: sum([]byte("other")), CacheDir: t.TempDir()})
	if err == nil || bad.dones != 1 || bad.lastEr == nil {
		t.Fatalf("a failure must call Done(err) too: %+v", bad)
	}
}

func TestBadSHA256Format(t *testing.T) {
	// The sha256 is used as a file name inside the cache, so a malformed value must be refused before any download.
	for _, bad := range []string{"../../etc/passwd", "abc", strings.Repeat("z", 64)} {
		if _, err := Get(context.Background(), "http://example.invalid/x", Options{SHA256: bad, CacheDir: t.TempDir()}); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
		if _, err := GetBytes(context.Background(), "http://example.invalid/x", Options{SHA256: bad}); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
	// Uppercase and surrounding whitespace must be tolerated, never read as a checksum failure.
	payload := []byte("uppercase hash")
	srv := serve(t, map[string]string{"/x": string(payload)})
	if _, err := Get(context.Background(), srv.URL+"/x", Options{SHA256: " " + strings.ToUpper(sum(payload)) + " ", CacheDir: t.TempDir()}); err != nil {
		t.Fatalf("an uppercase/padded hash must still pass: %v", err)
	}
}

func TestRedirect(t *testing.T) {
	payload := []byte("one hop and we are there")
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
		t.Fatalf("an ordinary redirect should be followed: %v", err)
	}
	cache := t.TempDir()
	if _, err := Get(context.Background(), srv.URL+"/loop", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("a redirect loop should fail, got %v", err)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("the cache directory should be empty: %v", ents)
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
		wantAny bool // only requires an error, no particular sentinel
	}{
		{name: "https", url: srv.URL + "/toy.toml"},
		{name: "file://", url: "file://" + local},
		{name: "hash matches", url: srv.URL + "/toy.toml", sha: sum([]byte(payload))},
		{name: "hash does not match", url: srv.URL + "/toy.toml", sha: sum([]byte("x")), wantErr: ErrChecksum},
		{name: "404", url: srv.URL + "/nope", wantErr: ErrAllFailed},
		{name: "over the cap", url: srv.URL + "/huge", wantErr: ErrAllFailed, wantAny: true},
		{name: "file:// missing", url: "file://" + dir + "/nope", wantErr: ErrAllFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := GetBytes(context.Background(), tc.url, Options{SHA256: tc.sha})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("wanted %v, got %v", tc.wantErr, err)
				}
				if tc.wantAny && !strings.Contains(err.Error(), "size limit") {
					t.Fatalf("the error should spell out that the cap was exceeded: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != payload {
				t.Fatalf("wrong content: %q", b)
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
		t.Fatalf("wanted %s, got %s", want, got)
	}
	if _, err := SumFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a file that does not exist should error")
	}
}

func TestCancelledContext(t *testing.T) {
	srv := serve(t, map[string]string{"/x": "content"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := t.TempDir()
	if _, err := Get(ctx, srv.URL+"/x", Options{CacheDir: cache}); err == nil {
		t.Fatal("an already-cancelled ctx should fail")
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("the cache directory should be empty: %v", ents)
	}
}

// When the server sends Content-Encoding: gzip, what lands on disk and what we hash
// must both be the bytes on the wire. Go's transport would otherwise add
// Accept-Encoding: gzip on its own and decompress transparently, and the resulting hash
// would be of the decompressed content -- never matching what the publisher computed
// over the release asset.
func TestHashesTheBytesOnTheWire(t *testing.T) {
	raw := []byte("the real bytes of the release asset")
	var wire bytes.Buffer
	gz := gzip.NewWriter(&wire)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); strings.Contains(ae, "gzip") {
			t.Errorf("we must not ask for gzip, but sent Accept-Encoding: %q", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(wire.Bytes())
	}))
	defer srv.Close()

	// With a sha256 (computed by the publisher over the file on the wire) it must verify.
	res, err := Get(context.Background(), srv.URL+"/x", Options{
		SHA256: sum(wire.Bytes()), CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("the publisher's hash is over the bytes on the wire, so this must pass: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, wire.Bytes()) {
		t.Fatalf("what landed is not the bytes on the wire (transparently decompressed): %d bytes", len(got))
	}

	// The TOFU path (no sha256) must not pin the decompressed hash either.
	res2, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res2.SHA256 != sum(wire.Bytes()) {
		t.Fatalf("TOFU pinned a hash that is not of the bytes on the wire: %s", res2.SHA256)
	}
}

// A ? or # in a file:// URL gets cut off by url.Parse and would silently open a different
// file. Refuse rather than guess.
func TestFileURLRefusesQueryAndFragment(t *testing.T) {
	dir := t.TempDir()
	// The trap: "a" and "a#b" both exist; asking for a#b must never return a's content.
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("the other file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a#b"), []byte("the file I asked for"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a#b", "a?b"} {
		b, err := GetBytes(context.Background(), "file://"+filepath.Join(dir, name), Options{})
		if err == nil {
			t.Fatalf("%q should be refused, but it read %q", name, b)
		}
		if !strings.Contains(err.Error(), "?") {
			t.Fatalf("the error should spell out that this is about ? / #: %v", err)
		}
	}
}

// Connection dropped mid-download, cancelled, or a non-200: none of them may leave
// anything at all in the cache directory.
func TestNothingLeftBehindOnFailure(t *testing.T) {
	t.Run("truncated response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("only a few bytes"))
		}))
		defer srv.Close()
		cache := t.TempDir()
		if _, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: cache}); err == nil {
			t.Fatal("a truncated response must not count as success")
		}
		assertEmptyDir(t, cache)
	})

	t.Run("cancelled mid-download", func(t *testing.T) {
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
			t.Fatal("a cancelled download should fail")
		}
		if s.starts != s.dones || s.lastEr == nil {
			t.Fatalf("cancelling mid-download must still call Done(err): %+v", s)
		}
		assertEmptyDir(t, cache)
	})

	// Only 200 counts: 204/206 carry a body, and treating them as success installs half a file.
	for _, code := range []int{204, 206, 301, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte("this is not a package"))
			}))
			defer srv.Close()
			cache := t.TempDir()
			if _, err := Get(context.Background(), srv.URL+"/x", Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
				t.Fatalf("HTTP %d should fail, got %v", code, err)
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
		t.Errorf("left wreckage behind: %q (temp files count too: the cache directory is what toy serve hands out as a mirror)", e.Name())
	}
}

// When the original address is poisoned (200 but the wrong content), a mirror must be able
// to rescue the install -- and the Sink's Start/Done must stay paired, because that is how
// ui's progress bar finishes.
func TestPoisonedOriginFallsBackToMirror(t *testing.T) {
	payload := []byte("the real package")
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer mirror.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("poisoned content"))
	}))
	defer origin.Close()

	s := &recordSink{}
	res, err := Get(context.Background(), origin.URL+"/x", Options{
		SHA256: sum(payload), Mirrors: []string{mirror.URL}, CacheDir: t.TempDir(), Sink: s,
	})
	if err != nil {
		t.Fatalf("the mirror has the right content, so this should be rescued: %v", err)
	}
	if res.Source != mirror.URL+"/"+sum(payload) {
		t.Fatalf("Source must say truthfully that a mirror rescued this (rule 8), got %q", res.Source)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, payload) {
		t.Fatalf("the poisoned content got installed: %q", got)
	}
	if s.starts != s.dones || s.starts != 2 {
		t.Fatalf("two attempts should produce two Start/Done pairs: %+v", s)
	}
	if s.lastEr != nil {
		t.Fatalf("the last attempt succeeded, so Done must not carry an err: %v", s.lastEr)
	}
}

// The same package installed concurrently: the cache may hold exactly one file, and every
// caller must get complete content.
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
				t.Errorf("failed under concurrency: %v", err)
				return
			}
			got, err := os.ReadFile(res.Path)
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("read half a file under concurrency: %d bytes %v", len(got), err)
			}
		}()
	}
	wg.Wait()

	ents, _ := os.ReadDir(cache)
	if len(ents) != 1 || ents[0].Name() != sum(payload) {
		t.Fatalf("the cache should hold exactly one file named after the hash, got %v", ents)
	}
}

// A file:// URL pointing at a directory must not be treated as a package.
func TestFileURLDirectory(t *testing.T) {
	cache := t.TempDir()
	if _, err := Get(context.Background(), "file://"+t.TempDir(), Options{CacheDir: cache}); !errors.Is(err, ErrAllFailed) {
		t.Fatalf("file:// pointing at a directory should fail, got %v", err)
	}
	assertEmptyDir(t, cache)
}
