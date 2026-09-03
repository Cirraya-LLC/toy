package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/index"
	"github.com/Cirraya-LLC/toy/internal/sign"
	"github.com/Cirraya-LLC/toy/internal/store"
)

// sgnKey mints a throwaway ed25519 key pair for tests.
func sgnKey(t *testing.T) sign.KeyPair {
	t.Helper()
	kp, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

const sgnTOML = "name = \"tinyserve\"\n[bin]\ntinyserve = \"tinyserve\"\n[artifact]\n\"linux/amd64\" = \"tinyserve_{version}_linux_amd64.tar.gz\"\n"

// Audit S1: a signature only proves SOMEBODY signed this record. It does not
// prove the repo's owner signed it.
//
// The index is a JSON file anyone can fork and anyone can send a PR against.
// An attacker signs SOMEONE ELSE'S id + source + toy.toml with their own
// private key -- and of course it verifies, because nothing in the message
// binds a key to an owner. The client then prints:
//
//	✓ signed · Derrity (key <the attacker's fingerprint>)
//
// The name outside the parens is the repo owner; the key inside is the
// attacker's. Iron law 8 demands the trust tier be printed honestly, and that
// line reads as "Derrity signed this".
func TestAuditAnyoneCanSignSomeoneElsesEntry(t *testing.T) {
	attacker := sgnKey(t)
	e := index.Entry{
		ID:     "Derrity/tinyserve",
		Source: "github:Derrity/tinyserve",
		Key:    hex.EncodeToString(attacker.Public),
	}
	e.Sig = sign.Sign(attacker.Private, autSignMessage(e.ID, e.Source, []byte(sgnTOML)))

	res := insTestResolved(t, e.Source, "v1.0.0")
	res.Raw = []byte(sgnTOML)
	res.Entry = &e
	rsvApplySignature("default", e, res)

	if res.Trust != store.TrustSigned {
		return // no checkmark at all, so there is nothing left to lie about
	}
	// With the checkmark still there, all that line can claim is "the key
	// registered in the index signed this version". Printing the repo owner
	// behind it is a lie: not one byte of the signed message ties the two
	// together.
	detail := insTrustDetail(res)
	if strings.HasPrefix(detail, "Derrity ") || strings.Contains(detail, "Derrity (key") {
		t.Fatalf("the attacker signed Derrity's entry with their own key and the client still prints "+
			"\"✓ signed · %s\" -- nothing binds key to owner, so this checkmark only means \"somebody signed it\"", detail)
	}
	if !strings.Contains(detail, res.Signer) {
		t.Fatalf("the trust line must at least name which key did the signing: %q", detail)
	}
}

// Audit S2: the author's public key is pinned by TOFU against host:owner/repo,
// and that string is derived from the INDEX ENTRY'S source. Change the source
// and you have changed identity, so the key swap never reaches Check at all --
// while spec 4 says "the public key is pinned on first install; if it changes,
// warn loudly and refuse".
//
// The script: Derrity/tinyserve is installed, signed, public key K pinned. The
// attacker sends a PR that repoints this entry's source at their own repo and
// swaps in their own key/sig. `toy update` goes back through the index via
// m.IndexID (insUpdateOne), picks up the new source, and then:
//   - insTrustDowngraded only compares tiers, both are signed, pass;
//   - insTOFU uses the new insPackageID as its id, the keyring has no such
//     entry, TrustNew, pass.
//
// At no point does anything say "the author's public key changed".
func TestAuditChangingSourceRotatesTheKeyPinIdentity(t *testing.T) {
	ring, err := sign.OpenRing(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victim := sgnKey(t)
	attacker := sgnKey(t)
	victimKey := hex.EncodeToString(victim.Public)
	attackerKey := hex.EncodeToString(attacker.Public)

	// First install: source points at the real author, public key K is pinned.
	old := insTestResolved(t, "github:Derrity/tinyserve", "v1.0.0")
	old.Entry = &index.Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Key: victimKey}
	if err := ring.Pin(insKeyID(old), "key", victimKey); err != nil {
		t.Fatal(err)
	}

	// The index entry is edited: same id, source repointed at the attacker's
	// repo, key and sig now the attacker's too.
	e := index.Entry{ID: "Derrity/tinyserve", Source: "github:evil/tinyserve", Key: attackerKey}
	e.Sig = sign.Sign(attacker.Private, autSignMessage(e.ID, e.Source, []byte(sgnTOML)))
	next := insTestResolved(t, e.Source, "v9.9.9")
	next.Raw = []byte(sgnTOML)
	next.Entry = &e
	rsvApplySignature("default", e, next)
	if next.Trust != store.TrustSigned {
		t.Fatalf("the attacker's signature should verify, got trust=%q", next.Trust)
	}

	// This is the exact check insPerform performs.
	trust, pinned, err := ring.Check(insKeyID(next), "key", attackerKey)
	if err != nil {
		t.Fatal(err)
	}
	if trust == sign.TrustChanged {
		return // fixed
	}
	t.Fatalf("the author public key for index id %q went from %s… to %s…, "+
		"but the TOFU id followed source from %q to %q, so Check returned %v (old pin %q) -- "+
		"not a single word about the key swap",
		e.ID, victimKey[:8], attackerKey[:8],
		insKeyID(old), insKeyID(next), trust, pinned)
}

// Audit S3: "signed": true in `toy search --json` only checks whether the
// entry carries a well-formed public key. It NEVER verifies the signature.
// Spec 5, verbatim: "a key alone does not count as signed". So anyone who
// copies a public key into someone else's entry -- without even forging a
// signature -- gets the machine-readable output to call that package signed.
func TestAuditSearchJSONCallsAnythingWithAKeySigned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)

	// A well-formed public key plus a signature that cannot possibly verify.
	bogus := sgnKey(t)
	idx := index.Index{Version: 1, Packages: []index.Entry{{
		ID:     "Derrity/tinyserve",
		Source: "github:Derrity/tinyserve",
		Desc:   "a very small static file server",
		Key:    hex.EncodeToString(bogus.Public),
		Sig:    "AAAA", // not a valid signature; not even the right length
	}}}
	data, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(idxPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Confirm the sig really does not verify, so we are not testing something else.
	if err := sign.Verify(hex.EncodeToString(bogus.Public),
		autSignMessage(idx.Packages[0].ID, idx.Packages[0].Source, []byte(sgnTOML)), "AAAA"); err == nil {
		t.Fatal("this signature must not verify")
	}

	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(store.Config{
		Indexes: []store.IndexRef{{Name: "t", URL: "file://" + idxPath}},
	}); err != nil {
		t.Fatal(err)
	}

	out := sgnCaptureStdout(t, func() {
		if err := runSearch(t.Context(), []string{"--json", "tinyserve"}); err != nil {
			t.Fatalf("search failed: %v", err)
		}
	})

	var got struct {
		Results []struct {
			ID     string `json:"id"`
			Signed bool   `json:"signed"`
			HasKey bool   `json:"has_key"`
			FP     string `json:"key_fingerprint"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 1 {
		t.Fatalf("want one result, got %d: %s", len(got.Results), out)
	}
	if got.Results[0].Signed {
		t.Fatalf("the signature does not verify, yet `toy search --json` reports %q as signed:true -- "+
			"that field only looked at whether a key was present (the rsvFingerprint line in cmd/toy/query.go)",
			got.Results[0].ID)
	}
	// "There is a public key here" is useful information; do not delete it in
	// passing. It just is not the same thing as "signed".
	if !got.Results[0].HasKey || got.Results[0].FP == "" {
		t.Fatalf("the entry really does carry a valid public key; has_key/key_fingerprint must say so: %s", out)
	}
}

// Originally suspected: `--force` was one switch governing six separate
// things, so a user who set it to "reinstall the same version" or to say "yes,
// I know this command name shadows something" also silently accepted "the
// author swapped in a different public key" -- exactly what spec 4 says we
// must "warn loudly and refuse" about.
//
// This one held up, and it is fixed: `--reinstall` handles reinstalls only,
// `--force` waives nothing but the changes the safety checks report, and when
// it does waive something it prints each waiver out loud (iron law 8).
// What this test guards now is the key swap: a reinstall must not waive it.
func TestReinstallDoesNotWaiveAKeySwap(t *testing.T) {
	fh := audNewFakeHost(t)
	fh.publish(t, "alice", "tool", "v1.0.0", "#!/bin/sh\necho GOOD\n")
	_, home, _ := audIndexSetup(t, fh)

	raw, err := os.ReadFile(filepath.Join(fh.root, "alice", "tool", "raw", "v1.0.0", "toy.toml"))
	if err != nil {
		t.Fatal(err)
	}
	idxPath := sgnSignedIndexPath(t, home)
	a, b := sgnKey(t), sgnKey(t)
	write := func(kp sign.KeyPair) {
		sgnWriteSignedIndex(t, idxPath, "alice/tool", "fake:alice/tool", raw, kp)
	}

	write(a)
	if err := runInstall(context.Background(), []string{"alice/tool"}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.FindByName("audtool")
	if err != nil {
		t.Fatal(err)
	}
	if m.Trust != store.TrustSigned {
		t.Fatalf("this version should be signed, got %v -- without that the key swap below means nothing", m.Trust)
	}

	// The public key in the index is swapped for a different one. The new
	// signature is self-consistent, so the trust tier does not drop; the only
	// thing that can stop this is TOFU remembering which key it was last time.
	write(b)

	err = runInstall(context.Background(), []string{"--reinstall", "alice/tool"})
	if err == nil {
		t.Fatal("--reinstall waived \"the author swapped public keys\" as well -- " +
			"reinstalling is an everyday operation and must not casually accept a changed author identity (spec 4)")
	}
	if !errors.Is(err, sign.ErrChanged) {
		t.Fatalf("it refused, but the error does not carry sign.ErrChanged, so callers cannot tell which refusal this is: %v", err)
	}

	// Only --force waives it, and it must say that a key swap is what it waived.
	out := captureStderr(t, func() {
		if err := runInstall(context.Background(), []string{"--reinstall", "--force", "alice/tool"}); err != nil {
			t.Fatalf("still cannot install with both --reinstall and --force: %v", err)
		}
	})
	if !strings.Contains(out, "--force waived") || !strings.Contains(out, "author public key") {
		t.Fatalf("--force waived a key swap without saying what it waived:\n%s", out)
	}
}

// sgnSignedIndexPath repoints home's config at a fresh index file (keeping the
// fake host).
func sgnSignedIndexPath(t *testing.T, home string) string {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signed.json")
	cfg.Indexes = []store.IndexRef{{Name: "signed", URL: "file://" + path}}
	if err := st.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

// sgnWriteSignedIndex writes one SIGNED index entry. What gets signed is
// exactly the three parts autSignMessage joins.
func sgnWriteSignedIndex(t *testing.T, path, id, source string, raw []byte, kp sign.KeyPair) {
	t.Helper()
	e := index.Entry{
		ID:     id,
		Source: source,
		Desc:   "d",
		Key:    hex.EncodeToString(kp.Public),
		Sig:    sign.Sign(kp.Private, autSignMessage(id, source, raw)),
	}
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{e}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Audit S5 (a control, not a finding): the signed message id+"\n"+source+"\n"+
// toy.toml has no length prefixes, but id and source are both whitelisted
// against newlines and spaces. The only thing that can move on the boundary is
// trailing whitespace on source (ref.Parse trims it, so index.validEntry lets
// it through), and all that can swallow is blank lines at the start of the
// toy.toml -- TOML semantics do not change. This test pins that premise down:
// the moment source starts allowing internal whitespace or newlines, the
// concatenation really does become ambiguous.
func TestAuditSignMessageBoundariesStayUnambiguous(t *testing.T) {
	for _, bad := range []string{
		"github:a/b\nname = \"x\"",
		"github:a/b name",
		"https://x/toy.toml\nname",
	} {
		idx, skipped, err := index.Load(sgnIndexJSON(t, bad), "t")
		if err != nil {
			continue
		}
		if skipped == 0 || len(idx.Packages) != 0 {
			t.Fatalf("source %q carries internal whitespace/newlines and the index accepted it -- "+
				"now the boundary between source and toy.toml can be slid at will, "+
				"and one signature covers two different inputs", bad)
		}
	}
	// Trailing whitespace does get through; noted for the record.
	idx, _, err := index.Load(sgnIndexJSON(t, "github:a/b \n"), "t")
	if err == nil && len(idx.Packages) == 1 {
		t.Logf("trailing whitespace on source is accepted (%q): it can only swallow leading whitespace "+
			"in the toy.toml, so the semantics do not change", idx.Packages[0].Source)
	}
}

func sgnIndexJSON(t *testing.T, source string) []byte {
	t.Helper()
	b, err := json.Marshal(index.Index{Version: 1, Packages: []index.Entry{
		{ID: "a/b", Source: source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sgnCaptureStdout captures whatever a piece of code writes to os.Stdout.
func sgnCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	defer func() { os.Stdout = old; f.Close() }()
	fn()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
