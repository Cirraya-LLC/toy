package main

import (
	"strings"
	"testing"
)

// An inlined entry has to be version-independent: it lives in the index and
// follows the upstream's newest tag, while [sha256] pins the bytes of one
// specific version. Leave it in and the next release the author cuts hands every
// user a "checksum mismatch, refusing to install" — which reads exactly like a
// poisoning warning, and is nothing but the author shipping normally.
func TestInlineStripsSHA256(t *testing.T) {
	raw := []byte(`name = "fd"
desc = "find files"

[bin]
fd = "fd"

[artifact]
"linux/amd64" = "fd_{version}_linux_amd64.tar.gz"

# With a sha256 the checksum is enforced, and a mismatch always refuses the install.
[sha256]
"linux/amd64" = "aa"

[script]
x = "y"
`)
	got, ok := autStripSHA256(raw)
	if !ok {
		t.Fatal("there is a [sha256] section and it reported stripping nothing")
	}
	s := string(got)
	if strings.Contains(s, "[sha256]") || strings.Contains(s, `"linux/amd64" = "aa"`) {
		t.Fatalf("the [sha256] section was not stripped cleanly:\n%s", s)
	}
	if strings.Contains(s, "the checksum is enforced") {
		t.Fatalf("the comment right above it has to go too, otherwise it points at a section that no longer exists:\n%s", s)
	}
	// Every other byte stays exactly as it was — this raw text is what the client
	// verifies the signature against.
	for _, must := range []string{`name = "fd"`, `[bin]`, `fd = "fd"`, `[artifact]`,
		`"linux/amd64" = "fd_{version}_linux_amd64.tar.gz"`, `[script]`, `x = "y"`} {
		if !strings.Contains(s, must) {
			t.Fatalf("collateral damage to %q:\n%s", must, s)
		}
	}
}

func TestInlineLeavesFileWithoutSHA256Alone(t *testing.T) {
	raw := []byte("name = \"fd\"\n[bin]\nfd = \"fd\"\n")
	got, ok := autStripSHA256(raw)
	if ok {
		t.Fatal("there is no [sha256] and it reported stripping something")
	}
	if string(got) != string(raw) {
		t.Fatalf("must not be modified: %q", got)
	}
}
