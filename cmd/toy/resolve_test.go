package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// Picking a version is the step both install and update go through. Get it
// wrong and the wrong thing gets installed.
func TestPickVersion(t *testing.T) {
	tags := []string{"v1.0.0", "release-junk", "v2.0.0-rc.1", "v1.2.0"}

	v, err := rsvPickVersion(tags, "")
	if err != nil || v.Raw != "v1.2.0" {
		t.Fatalf("without --version it should pick the highest stable release v1.2.0, got %q (err=%v)", v.Raw, err)
	}
	// Writing 1.2.0 and writing v1.2.0 mean the same tag.
	if v, err := rsvPickVersion(tags, "1.2.0"); err != nil || v.Raw != "v1.2.0" {
		t.Fatalf("1.2.0 should match tag v1.2.0, got %q (err=%v)", v.Raw, err)
	}
	// Naming a version that does not exist has to error and say which ones do.
	_, err = rsvPickVersion(tags, "v9.9.9")
	if err == nil || !strings.Contains(err.Error(), "v1.2.0") {
		t.Fatalf("a missing version must error and list the existing ones, got %v", err)
	}
}

// Two index names must not escape to the same cache file — a collision hands
// out somebody else's index when offline.
func TestIndexCacheNamesDoNotCollide(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a, b := rsvIndexCache(st, "a/b"), rsvIndexCache(st, "a:b"); a == b {
		t.Fatalf("%q and %q collided on the same cache file %s", "a/b", "a:b", a)
	}
}

func TestLoadIndexesCachesAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "idx.json")
	body := `{"version":1,"packages":[{"id":"Derrity/tinyserve","source":"github:Derrity/tinyserve"}]}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := store.Config{Indexes: []store.IndexRef{{Name: "local", URL: "file://" + src}}}

	idxs, err := loadIndexes(context.Background(), st, cfg)
	if err != nil || len(idxs) != 1 {
		t.Fatalf("should load 1 index, got %d (err=%v)", len(idxs), err)
	}

	// With the source gone it still has to use the last cached copy instead of
	// turning into "no such package".
	os.Remove(src)
	idxs, err = loadIndexes(context.Background(), st, cfg)
	if err != nil || len(idxs) != 1 {
		t.Fatalf("should fall back to the cache when the source is unreachable, got %d (err=%v)", len(idxs), err)
	}
	if _, ok := idxs[0].Find("derrity/tinyserve"); !ok {
		t.Fatal("the cached entry did not come back")
	}

	// With no cache either, failing everywhere has to be reported honestly —
	// never a silent empty result.
	os.RemoveAll(st.IndexDir())
	if _, err := loadIndexes(context.Background(), st, cfg); err == nil {
		t.Fatal("every index failed to load and it still reported success")
	}
}

// TestRsvApplySignatureOnlyTrustsAVerifiedSignature is where iron law 8 lands on
// the index path: "✓ signed" may only be printed after a signature actually
// verified. The index is a JSON file anyone can fork and anyone can send a PR
// against, and copying a public key into an entry costs nothing — printing the
// check mark on sight of a key hands those two characters to anyone who can edit
// the index.
func TestRsvApplySignatureOnlyTrustsAVerifiedSignature(t *testing.T) {
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	const id, source = "Derrity/tinyserve", "github:Derrity/tinyserve"
	raw := []byte("name = \"tinyserve\"\n")
	pub := hex.EncodeToString(kp.Public)
	good := sign.Sign(kp.Private, autSignMessage(id, source, raw))

	other, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		entry index.Entry
		raw   []byte
		want  string // the Trust we want
	}{
		{"only a verified signature counts as signed",
			index.Entry{ID: id, Source: source, Key: pub, Sig: good}, raw, store.TrustSigned},
		{"a key with no sig does not count",
			index.Entry{ID: id, Source: source, Key: pub}, raw, store.TrustHost},
		{"a swapped toy.toml does not verify",
			index.Entry{ID: id, Source: source, Key: pub, Sig: good},
			[]byte("name = \"tinyserve\"\n# one line snuck in\n"), store.TrustHost},
		{"somebody else's key against this signature does not count",
			index.Entry{ID: id, Source: source, Key: hex.EncodeToString(other.Public), Sig: good},
			raw, store.TrustHost},
		{"the key is not a valid public key",
			index.Entry{ID: id, Source: source, Key: "zzzz", Sig: good}, raw, store.TrustHost},
		{"no claimed name means there is no signature to speak of",
			index.Entry{ID: id, Source: source}, raw, store.TrustHost},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := &resolved{Trust: store.TrustHost, Raw: c.raw}
			rsvApplySignature("test index", c.entry, res)
			if res.Trust != c.want {
				t.Fatalf("Trust = %q, want %q", res.Trust, c.want)
			}
			if c.want != store.TrustSigned && res.Signer != "" {
				t.Fatalf("nothing verified, so there must be no signer, got %q", res.Signer)
			}
			if c.want == store.TrustSigned && res.Signer == "" {
				t.Fatal("it verified but cannot say who signed")
			}
		})
	}
}
