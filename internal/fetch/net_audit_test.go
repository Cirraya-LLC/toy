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

// Regression: an https request redirected to http must be refused on the spot.
//
// Original suspicion: the client follows the downgrade to plaintext, so a man in the middle
// can swap the content.
// Verdict after review: it holds -- CheckRedirect only counted hops, it never looked at the
// scheme. Fixed. What this now guards is "once https has started, it may never fall back to
// http". For an artifact with a [sha256] the hash still catches it, but when GetBytes fetches
// toy.toml and the index there is NO HASH AT ALL -- and those are precisely what decides
// "what do we download next, and which hash do we compare against".
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
		t.Fatalf("https was silently downgraded to %s, and it fetched %q", plain.URL, b)
	}
	// The error must spell out that this is a downgrade, not just a bare "could not fetch" --
	// nobody can infer "your upstream kicked me onto plaintext" from the latter.
	if !strings.Contains(err.Error(), "downgrades") {
		t.Errorf("the error does not say this is a downgrade: %v", err)
	}
}

// Regression: the other direction (http -> https) is an upgrade and must be allowed.
// A rule that rejects every cross-scheme redirect would break http->https, which is a
// perfectly ordinary configuration everywhere.
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
		t.Fatalf("http->https is an upgrade and must not be refused: %v", err)
	}
	if string(b) != "secure" {
		t.Fatalf("got %q", b)
	}
}

// Regression: passing through https in the middle and then falling back to http must be
// refused as well. An implementation that only compares against the PREVIOUS hop misses
// this one -- a chain is only as safe as its weakest segment, and the bytes could already
// have been swapped on that middle hop.
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
		t.Fatalf("http->https->http was let through, and it fetched %q", b)
	}
}

// Regression: a password in the URL must never end up inside an error value.
//
// Original suspicion: credentials go into errors verbatim. Verdict after review: it holds.
// SafeURL in the ui layer is a backstop, not a fix -- an error goes into logs, into `--json`,
// into every path that never touches ui, so it has to be scrubbed at the moment fetch builds
// the error. The user name stays: only the password is a secret, and blanking the user name
// just leaves people unable to recognise the address they typed.
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
			t.Fatalf("%s: should have failed", tc.name)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the password reached the error message: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "alice") {
			t.Errorf("%s: the user name was scrubbed too, so nobody can recognise the address they typed: %v", tc.name, err)
		}
	}
}

// Regression: the downgrade error must not carry credentials either.
// fetch builds this error itself (it is not one of net/http's already-scrubbed *url.Error
// values), so it is an independent leak path.
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
		t.Fatal("this should be refused")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the password in the redirect target reached the error: %v", err)
	}
}

// Result.Source is the VERBATIM address and is deliberately not scrubbed: install compares
// it against the originally requested URL to decide "did these bytes come from a mirror?",
// and scrubbing one side means they never match, so every install would falsely report a
// mirror. Scrubbing happens at the two exits instead (ui printing, store writing the
// manifest). This pins that contract, so nobody helpfully redacts it here and turns the
// mirror notice into something that is always true.
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
		t.Fatalf("Source = %q, wanted the requested address character for character: %q", res.Source, u)
	}
}

// Regression: a cross-host redirect must not carry Authorization along.
//
// Original suspicion: credentials followed the redirect to a second machine.
// Verdict after review: does not hold -- net/http's makeHeadersCopier drops sensitive headers
// when the host changes, including the Authorization derived from the URL userinfo. So this is
// not a fix, it PINS THAT STANDARD LIBRARY BEHAVIOUR: if it ever changes (or if we ever start
// writing the Authorization header ourselves), the password the user put in the address gets
// handed to a completely unrelated server.
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
		t.Errorf("credentials followed the redirect to the second machine: %q", seen)
	}
}

// Regression: artifact downloads are capped, and the test is the BYTES ACTUALLY WRITTEN.
//
// Original suspicion: Get had no cap at all, so it wrote whatever the server chose to stream
// and could fill up the user's disk.
// Verdict after review: it holds (GetBytes had maxBytes, Get had nothing). Fixed with
// Options.MaxBytes, default DefaultMaxBytes = 2 GiB, lined up with archive.MaxTotalBytes.
//
// The two subcases cover both untrustworthy shapes of a server's self-reported length: none
// at all (chunked) and an absurdly large one. The opposite direction (declaring less than it
// sends) needs no test because it cannot overflow -- net/http already treats Content-Length
// as the ceiling when reading the body, so the client never even sees the extra bytes. The
// dangerous cases are "no length" and "an absurd length", and both can only be stopped by
// counting the bytes that really land on disk.
func TestArtifactDownloadIsCapped(t *testing.T) {
	const limit = 1 << 20

	for _, tc := range []struct {
		name    string
		declare bool // honestly declare an 8 MiB Content-Length
	}{
		{"chunked, no length", false},
		{"Content-Length declared very large", true},
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
				t.Fatal("going over the cap somehow succeeded")
			}
			if !strings.Contains(err.Error(), "size limit") {
				t.Errorf("the error does not say this is the size cap: %v", err)
			}
			// A few buffers of overshoot are fine: LimitReader stops at limit+1, but the
			// server may already have pushed some blocks into kernel buffers. What we want
			// is "bounded", not "not one byte more".
			if got := sent.Load(); got > 4*limit {
				t.Errorf("the server streamed %d bytes before stopping, cap is %d -- nothing actually stopped it", got, limit)
			}
			// No half-written file may survive: rule 4's "leave no wreckage" applies to the
			// size cap too, or one attack leaves cap-sized garbage in the cache directory.
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				t.Errorf("the cache directory was left holding a half-written file %q", e.Name())
			}
		})
	}
}

// Regression: with no MaxBytes set the default cap applies, and a normally sized package
// must still install. A cap that stops everyone is not a cap, it is an outage.
func TestDefaultCapLetsNormalDownloadsThrough(t *testing.T) {
	if DefaultMaxBytes != 2<<30 {
		t.Fatalf("DefaultMaxBytes = %d, the spec says it matches archive.MaxTotalBytes at 2 GiB", DefaultMaxBytes)
	}
	body := strings.Repeat("z", 3<<20) // 3 MiB: under GetBytes's 32 MiB, over 1 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, strings.NewReader(body))
	}))
	defer srv.Close()

	res, err := Get(context.Background(), srv.URL+"/x.tar.gz", Options{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("the default cap blocked a 3 MiB package: %v", err)
	}
	st, err := os.Stat(res.Path)
	if err != nil || st.Size() != int64(len(body)) {
		t.Fatalf("landed at size %v (err %v), wanted %d", st, err, len(body))
	}
}

// Regression: an HTTP redirect cannot hop to file:// or any other scheme.
//
// Original suspicion: a remote 302 is enough to make us read a local file.
// Verdict after review: does not hold -- the Transport has no file protocol registered, so
// hopping there only yields "unsupported protocol scheme". This pins it, because the day
// someone registers RegisterProtocol("file", ...) on the Transport for convenience, the hole
// is genuinely open.
func TestRedirectCannotJumpToFileScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()
	if b, err := GetBytes(context.Background(), srv.URL+"/toy.toml", Options{}); err == nil {
		t.Errorf("the redirect hopped to file:// and fetched %q", b)
	}
}

// Regression: redirects are bounded, so a loop cannot leave the client hanging.
func TestRedirectLoopIsBounded(t *testing.T) {
	n := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Redirect(w, r, srv.URL+fmt.Sprintf("/next%d", n), http.StatusFound)
	}))
	defer srv.Close()
	if _, err := GetBytes(context.Background(), srv.URL+"/toy.toml", Options{}); err == nil {
		t.Fatal("a redirect loop somehow succeeded")
	}
	if n > maxRedirects+1 {
		t.Errorf("followed %d redirects, the cap is %d", n, maxRedirects)
	}
}
