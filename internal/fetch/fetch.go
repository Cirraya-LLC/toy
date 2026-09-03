// Package fetch downloads, verifies and caches. The mirror fallback from spec §8 lives here too.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrChecksum  = errors.New("fetch: sha256 checksum mismatch")
	ErrAllFailed = errors.New("fetch: could not fetch from the original URL or any mirror")
)

// Sink is the progress callback. It is declared here rather than taken from ui
// so that dependencies only ever point downward.
type Sink interface {
	Start(name string, total int64)
	Add(n int64)
	Done(err error)
}

type Options struct {
	SHA256   string   // non-empty means verification is mandatory; a mismatch returns ErrChecksum and deletes the temp file (rule 4)
	Mirrors  []string // mirror bases; a mirror URL is base + "/" + sha256 (only usable when SHA256 is set)
	CacheDir string   // empty means no caching
	Sink     Sink     // may be nil
	Name     string   // for display
	// MaxBytes caps what Get writes to disk; <=0 means DefaultMaxBytes.
	// The test is the number of bytes ACTUALLY WRITTEN, never Content-Length -- a server
	// that declares 1 KiB and then streams chunked until morning is exactly what this cap
	// is here to stop, and trusting its self-reported length is the same as having no cap.
	// GetBytes ignores this field; it has its own, much smaller fixed cap (maxBytes).
	MaxBytes int64
}

type Result struct {
	Path      string // where the file landed (in the cache or in a temp dir)
	SHA256    string // what we actually computed
	FromCache bool
	// Source is the URL that actually succeeded, so we can tell the user the truth (rule 8).
	//
	// It is the VERBATIM address, possibly still carrying credentials the user wrote into
	// the URL themselves (after `toy install https://u:p@host/toy.toml`, ResolveReference
	// carries the userinfo over to the artifact URL). Redacting here is deliberately NOT
	// done: the caller compares this against the originally requested URL to decide
	// "did these bytes come from a mirror?", and redacting one side means they never match
	// again. Redaction happens at each exit instead (ui's redactURLs, store's redactURL
	// when writing the manifest).
	// Every ERROR MESSAGE fetch builds itself always carries a redacted address; see redactURL.
	Source string
}

const (
	// maxBytes caps GetBytes. This path only fetches toy.toml and index JSON, so
	// 32 MiB is plenty of headroom while still stopping a bad URL from eating all memory.
	maxBytes = 32 << 20
	// maxRedirects follows redirects but not forever, so we never spin in a loop.
	maxRedirects = 10
	// DefaultMaxBytes is the default on-disk cap for Get (artifact downloads); Options.MaxBytes overrides it.
	//
	// 2 GiB lines up with archive.MaxTotalBytes: the total AFTER extraction may not exceed
	// that number, so a larger archive buys nothing -- the only use for those extra bytes
	// is filling up the user's disk. We deliberately do not import archive for the constant,
	// to keep fetch from depending on it (dependencies point downward, same reason Sink does
	// not depend on ui); both sides hardcode the same number and point at each other.
	DefaultMaxBytes = 2 << 30
)

var client = &http.Client{
	// Backstop for the whole download. "The server went silent" is caught much sooner
	// by ResponseHeaderTimeout.
	Timeout: 30 * time.Minute,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		// Transparent compression MUST stay off: Go would add Accept-Encoding: gzip on its
		// own and transparently decompress, so the hash we compute would be of the
		// DECOMPRESSED bytes while the publisher hashed the release asset file itself.
		// Consequence one: every package with a [sha256] reports a bogus "checksum mismatch".
		// Consequence two: on the TOFU path (no hash) we would pin the wrong bytes under the
		// wrong hash and store them at cache/<hash> -- and that directory is exactly what
		// toy serve hands out as a mirror (spec §6/§8).
		// The rule: the bytes we hash must be the bytes on the wire.
		DisableCompression: true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("more than %d redirects; this looks like a redirect loop", maxRedirects)
		}
		// Once we are on https, a redirect down to http is refused on the spot (the other
		// direction is an upgrade, so it is allowed).
		// Anyone on a plaintext hop can rewrite the bytes: for an artifact with a [sha256]
		// the hash still catches it, but when GetBytes fetches toy.toml and the index there
		// is NO HASH AT ALL -- and those two are precisely what decides "what do we download
		// next, and which hash do we compare against". A downgrade there means every check
		// afterwards is validating a manifest the attacker wrote.
		//
		// Scan the whole via chain, not just the previous hop: http->https->http would slip
		// past a "compare with the last hop" check, and a chain is only as safe as its
		// weakest segment.
		if req.URL.Scheme != "https" {
			for _, prev := range via {
				if prev.URL.Scheme == "https" {
					return fmt.Errorf("refusing to follow this redirect: %s downgrades https to %s. "+
						"Anyone on a plaintext hop can rewrite the bytes, and toy.toml and the index carry no sha256 to catch it",
						redactURL(prev.URL.String()), redactURL(req.URL.String()))
				}
			}
		}
		return nil
	},
}

// nopSink saves every call site from a nil check when Options.Sink is unset.
type nopSink struct{}

func (nopSink) Start(string, int64) {}
func (nopSink) Add(int64)           {}
func (nopSink) Done(error)          {}

// sinkWriter turns bytes written into progress.
type sinkWriter struct{ s Sink }

func (w sinkWriter) Write(p []byte) (int, error) {
	w.s.Add(int64(len(p)))
	return len(p), nil
}

// normalize settles the optional fields and rejects a malformed sha256 along the way.
// That sha256 becomes a file name inside the cache and a path segment in mirror URLs,
// and it arrives from a toy.toml somewhere on the network -- without a format check,
// a single "../../x" writes outside the cache directory.
func normalize(opt *Options) error {
	if opt.Sink == nil {
		opt.Sink = nopSink{}
	}
	opt.SHA256 = strings.ToLower(strings.TrimSpace(opt.SHA256))
	if opt.SHA256 == "" {
		return nil
	}
	if len(opt.SHA256) != 64 {
		return fmt.Errorf("fetch: sha256 %q is not 64 hex characters; check the [sha256] section of toy.toml", opt.SHA256)
	}
	if _, err := hex.DecodeString(opt.SHA256); err != nil {
		return fmt.Errorf("fetch: sha256 %q contains non-hex characters; check the [sha256] section of toy.toml", opt.SHA256)
	}
	return nil
}

// redactURL replaces the password in an address with xxxxx (the standard library's
// URL.Redacted) and keeps the user name -- only the password is a secret, and blanking
// the user name just leaves people unable to recognise the address they typed.
//
// It has to happen AT THE MOMENT the error message is built; leaning on the ui layer as a
// backstop is not enough. The error value itself goes into logs, into `--json`, into every
// path that never touches ui -- and `toy install https://u:hunter2@host/toy.toml` is a
// perfectly ordinary thing to do. The *url.Error values net/http returns are MOSTLY already
// redacted (stripPassword); the one path that is not is a CheckRedirect failure -- see open.
//
// If it does not parse, or there is no userinfo at all, return it untouched: an "address"
// on this path may well be a file:// URL or something that was never a URL to begin with,
// and we do not mangle what we do not recognise.
func redactURL(s string) string {
	if !strings.Contains(s, "@") {
		return s // the vast majority of addresses carry no credentials; skip url.Parse
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	return u.Redacted()
}

// sources returns the addresses to try, in order: the original first, mirrors after.
// No sha256 means no mirrors -- without a hash there is no way to verify what a mirror
// hands back, and using one at that point is simply trusting a stranger (spec §8).
func sources(u string, opt Options) []string {
	out := []string{u}
	if opt.SHA256 == "" {
		return out
	}
	for _, m := range opt.Mirrors {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		out = append(out, strings.TrimSuffix(m, "/")+"/"+opt.SHA256)
	}
	return out
}

// open opens one source and returns the content stream plus the known length (-1 if unknown).
// file:// goes through os.Open; it never reaches http.Client.
func open(ctx context.Context, src string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(src, "file://") {
		p, err := filePath(src)
		if err != nil {
			return nil, 0, err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, 0, err
		}
		size := int64(-1)
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		return f, size, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// The address net/http puts into url.Error is password-stripped (stripPassword)
		// on every path but one: when CheckRedirect fails it stuffs the Location header
		// back VERBATIM (that `ue.(*url.Error).URL = loc` line in client.go). So an upstream
		// that redirects you to https://u:p@... can use that path to write credentials into
		// our error. Scrub it right here, so every error leaving fetch is clean.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = redactURL(ue.URL)
		}
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		// A 404 must be recognisable BY TYPE; callers must never have to match on error
		// text. The reason phrase in resp.Status is whatever the server chose to send, so
		// one upstream answering `HTTP/1.1 500 HTTP 404 Not Found` is enough to make any
		// strings.Contains("HTTP 404") check read a server failure as "does not exist".
		// Anyone turning a 404 into a decision (say, "upstream really has no toy.toml, so
		// fall back to the index's inline copy") must get the status code itself.
		// We reuse fs.ErrNotExist instead of inventing a sentinel: on the file:// path that
		// is exactly what os.Open returns, so both paths collapse into a single check and
		// the caller writes one errors.Is.
		err := fmt.Errorf("HTTP %s", resp.Status)
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", os.ErrNotExist, err)
		}
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func filePath(src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil {
		return "", fmt.Errorf("cannot parse file:// URL: %w", err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("file:// does not take a host name (%q); write it as file:///absolute/path", u.Host)
	}
	if u.Path == "" {
		return "", fmt.Errorf("file:// URL %q has no path", src)
	}
	// Callers usually build these by concatenating "file://" with a path. A ? or # anywhere
	// in that path makes url.Parse cut everything after it into query/fragment, and we would
	// then SILENTLY OPEN A DIFFERENT FILE -- file:///x/a#b would read /x/a. A file address
	// has no business carrying either; refuse rather than guess (rule 8).
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("file:// URL %q contains ? or #; those get parsed as query/fragment and would silently open a different file. If the name really contains them, escape as %%3F / %%23", src)
	}
	return filepath.FromSlash(u.Path), nil
}

// Get downloads to a local file. With a CacheDir set and a SHA256 that hits the cache,
// it returns immediately. If the original address fails (network error, 4xx, 5xx, bad
// checksum) it walks Mirrors in order, and every mirror's content is hashed again from
// scratch -- which is why a mirror never has to be trusted.
//
// With an empty CacheDir the result lands in the system temp directory and belongs to the caller.
func Get(ctx context.Context, u string, opt Options) (Result, error) {
	if err := normalize(&opt); err != nil {
		return Result{}, err
	}

	// A hit means "the file is there AND rehashing it still matches". Checking only that
	// the file exists would let a truncated or tampered cache file be installed as-is.
	if opt.CacheDir != "" && opt.SHA256 != "" {
		p := filepath.Join(opt.CacheDir, opt.SHA256)
		if sum, err := SumFile(p); err == nil && sum == opt.SHA256 {
			return Result{Path: p, SHA256: sum, FromCache: true, Source: u}, nil
		}
	}

	var errs []error
	for _, src := range sources(u, opt) {
		res, err := fetchOne(ctx, src, opt)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", redactURL(src), err))
		if ctx.Err() != nil {
			break
		}
	}
	// Each sub-error already names its own address, so we do not repeat u here --
	// with no mirrors configured that would print the same address twice and read
	// like two separate failures.
	return Result{}, fmt.Errorf("%w: %w", ErrAllFailed, errors.Join(errs...))
}

// fetchOne pulls one source to disk: write a temp file, hash while writing, and only
// rename it atomically into the cache once it verifies. So the cache directory never
// contains half a file.
func fetchOne(ctx context.Context, src string, opt Options) (res Result, err error) {
	dir := opt.CacheDir
	if dir != "" {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, fmt.Errorf("cannot create cache directory %s: %w", dir, err)
		}
	}
	// The temp file and the final location must sit in the same directory; a rename across
	// file systems is not atomic (and simply fails).
	tmp, err := os.CreateTemp(dir, ".toy-tmp-*")
	if err != nil {
		return Result{}, fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpName := tmp.Name()
	placed, started := false, false
	defer func() {
		tmp.Close()
		if !placed {
			// Bad checksum, connection dropped, write failed -- leave no wreckage, ever (rule 4).
			os.Remove(tmpName)
		}
		if started {
			opt.Sink.Done(err)
		}
	}()

	body, total, err := open(ctx, src)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	started = true
	opt.Sink.Start(opt.Name, total)
	h := sha256.New()
	limit := opt.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	// Read one extra byte so "exactly at the limit" and "over it" stay distinguishable.
	n, err := io.Copy(io.MultiWriter(tmp, h, sinkWriter{opt.Sink}), io.LimitReader(body, limit+1))
	if err != nil {
		return Result{}, fmt.Errorf("download interrupted: %w", err)
	}
	// The test is the BYTES ACTUALLY WRITTEN, not total -- that is just the Content-Length
	// the server claimed, a chunked response has no such header at all, and reporting an
	// absurdly large number costs nothing. The partial file is removed by the defer above
	// (placed is still false), so no wreckage is left in the cache (rule 4 holds here too).
	if n > limit {
		err = fmt.Errorf("download exceeded the %d MiB size limit; aborted and the partial file was deleted. "+
			"The count is the bytes actually written, not the length the server claimed. "+
			"Raise Options.MaxBytes if you really do need a package this large", limit>>20)
		return Result{}, err
	}
	if err = tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("failed to write temp file: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if opt.SHA256 != "" && sum != opt.SHA256 {
		err = fmt.Errorf("%w: expected %s, got %s. The downloaded file was deleted; nothing will be installed", ErrChecksum, opt.SHA256, sum)
		return Result{}, err
	}

	path := tmpName
	if opt.CacheDir != "" {
		// The cache file name is the content hash, which is what makes the cache directory
		// usable as a mirror directory with no extra work (spec §8).
		if err = os.Chmod(tmpName, 0o644); err != nil {
			return Result{}, err
		}
		path = filepath.Join(opt.CacheDir, sum)
		if err = os.Rename(tmpName, path); err != nil {
			return Result{}, fmt.Errorf("failed to move the file into the cache: %w", err)
		}
	}
	placed = true
	return Result{Path: path, SHA256: sum, Source: src}, nil
}

// GetBytes is for small files (toy.toml, index JSON). It supports file:// and https://.
// A size cap is mandatory here, so that a bad URL cannot eat all available memory.
func GetBytes(ctx context.Context, u string, opt Options) ([]byte, error) {
	if err := normalize(&opt); err != nil {
		return nil, err
	}
	var errs []error
	for _, src := range sources(u, opt) {
		b, err := getBytesOne(ctx, src, opt)
		if err == nil {
			return b, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", redactURL(src), err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w: %w", ErrAllFailed, errors.Join(errs...))
}

func getBytesOne(ctx context.Context, src string, opt Options) ([]byte, error) {
	body, _, err := open(ctx, src)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	// Read one extra byte so "exactly at the limit" and "over it" stay distinguishable.
	b, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, fmt.Errorf("content exceeded the %d MiB size limit; this path only fetches toy.toml and index JSON, so check that the URL is right", maxBytes>>20)
	}
	if opt.SHA256 != "" {
		s := sha256.Sum256(b)
		if sum := hex.EncodeToString(s[:]); sum != opt.SHA256 {
			return nil, fmt.Errorf("%w: expected %s, got %s", ErrChecksum, opt.SHA256, sum)
		}
	}
	return b, nil
}

// SumFile computes the sha256 (hex) of an existing file.
func SumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
