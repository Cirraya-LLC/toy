package toyfile

import (
	"net/url"
	"testing"
)

// Audit finding: not one absolute URL with a scheme in an [artifact]/[script] value was
// being caught. checkRelPath only blocked a leading "/", a ".." segment and "?#", and
// "file:///etc/shadow" trips none of the three. Now a regression test: these must all be
// rejected, so it fails loudly if that check is ever loosened.
func TestAuditAbsoluteURLAssetsAreAccepted(t *testing.T) {
	for _, v := range []string{
		"file:///etc/shadow",
		"file:///dev/zero",
		"https://evil.example/x.tar.gz",
		"http://169.254.169.254/latest/meta-data/",
	} {
		src := "name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"" + v + "\"\n"
		tf, err := Parse([]byte(src))
		if err != nil {
			t.Logf("rejected at parse: %q -> %v", v, err)
			continue
		}
		asset, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
		if err != nil {
			t.Logf("rejected by Asset: %q -> %v", v, err)
			continue
		}
		base, _ := url.Parse("https://good.example/pkg/toy.toml")
		rel, _ := url.Parse(asset)
		t.Errorf("let through: %q, which on the bare-URL path resolves to %s", v, base.ResolveReference(rel))
	}
}
