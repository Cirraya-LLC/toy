package index

import "testing"

// Audit: an index entry's source could be file://, which would let a remote
// index make the client read local files.
func TestAuditIndexEntryCanPointAtLocalFile(t *testing.T) {
	for _, src := range []string{
		"file:///home/victim/.aws/credentials",
		"file:///etc/shadow",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
	} {
		if validEntry(Entry{ID: "a/b", Source: src}) {
			t.Errorf("an index entry's source let through %q", src)
		}
	}
}
