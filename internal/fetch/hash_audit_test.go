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

// Regression (rule 4): a cache hit must rehash the file, not just check that it exists.
//
// Original suspicion: the cache decides a hit purely from the file name (= the declared
// hash), so anyone with write access to the file system could plant a file with the right
// name and the wrong content and have it installed as-is.
// Verdict after review: does not hold -- the SumFile call in Get already rehashes.
// So this is not a fix, it PINS that rehash: it looks like a disk read you could save,
// and saving it turns ~/.toy/cache into a trust root anyone can write to (and that
// directory is also what toy serve hands out as a mirror, spec §6/§8).
func TestAuditCacheHitRecomputesHash(t *testing.T) {
	good := []byte("the real artifact")
	want := auditSum(good)
	cache := t.TempDir()
	// Plant a file with THE RIGHT NAME AND THE WRONG CONTENT: anyone on the file system can do this.
	if err := os.WriteFile(filepath.Join(cache, want), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(good)
	}))
	defer srv.Close()

	res, err := Get(context.Background(), srv.URL, Options{SHA256: want, CacheDir: cache})
	if err != nil {
		t.Fatalf("it should have downloaded again and succeeded, but errored: %v", err)
	}
	if res.FromCache {
		t.Fatal("rule 4 bypassed: a tampered cache file was returned as a hit")
	}
	b, _ := os.ReadFile(res.Path)
	if string(b) != string(good) {
		t.Fatalf("what landed is not the content that was verified: %q", b)
	}
}

// Regression: how a declared sha256 handles case, surrounding whitespace and a 0x prefix.
// It must neither pass by accident nor fail by accident.
//
// Original suspicion: normalize lets a malformed hash through, or the reverse -- it rejects
// a valid hash that differs only in case.
// Verdict after review: does not hold, both directions are right. What this pins is the line
// between "liberal on the way in" and "strict on the way out": case and whitespace are the
// usual noise from an author hand-copying a toy.toml and must be tolerated, while length and
// character set must not be, because this string is used as a file name in the cache and as
// a path segment in mirror URLs.
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
		{"verbatim", want, true},
		{"uppercase", strings.ToUpper(want), true},
		{"surrounding whitespace", "  " + want + "\n", true},
		{"0X prefix", "0X" + want[2:], false},
		{"truncated", want[:63], false},
	}
	for _, c := range cases {
		_, err := Get(context.Background(), srv.URL, Options{SHA256: c.give})
		if c.ok && err != nil {
			t.Errorf("%s: should pass, but failed: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: should be refused, but passed", c.name)
		}
	}
}

// Regression (rule 4): a failed checksum must leave no wreckage in the cache directory,
// and above all no half-written file whose name happens to be a valid hash.
//
// Original suspicion: the temp file survives a failed verification.
// Verdict after review: does not hold -- fetchOne's defer covers every failure path via the
// placed flag. What this pins is that flag: the moment someone adds a return and forgets it,
// the cache directory starts growing half-written files -- and the cache directory is exactly
// what toy serve hands straight out to the world.
func TestAuditFailedDownloadLeavesNoCacheFile(t *testing.T) {
	cache := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not what was promised"))
	}))
	defer srv.Close()

	_, err := Get(context.Background(), srv.URL, Options{SHA256: auditSum([]byte("promised")), CacheDir: cache})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("wanted ErrChecksum, got %v", err)
	}
	ents, _ := os.ReadDir(cache)
	for _, e := range ents {
		t.Fatalf("the cache directory was left holding %q", e.Name())
	}
}

// Regression (spec §8): no sha256 means no mirrors -- without a hash there is no way to
// verify what a mirror hands back.
//
// Original suspicion: mirrors get tried even without a hash, which is letting a stranger
// ship on the upstream's behalf.
// Verdict after review: does not hold, the early return in sources is exactly for this.
// The entire premise behind "a mirror never has to be trusted" is that its content can be
// checked against a hash. With no hash the premise is gone, so that fallback path has to
// disappear completely.
func TestAuditNoMirrorsWithoutHash(t *testing.T) {
	got := sources("https://origin/a", Options{Mirrors: []string{"https://mirror"}})
	if len(got) != 1 {
		t.Fatalf("no hash means no mirrors, got %v", got)
	}
}
