package assetname

import "testing"

// watchexec ships `watchexec-2.3.2-x86_64-unknown-linux-gnu.deb.b3`, a BLAKE3
// digest beside a .deb. The suffix that decides is the LAST one, so having .deb
// on the list did not help -- .b3 was missing, the file was picked as the
// artifact for linux/amd64/gnu, and what got installed was a text file holding a
// hex digest. Running it produced `<digest>: command not found` from line 1.
func TestChecksumSidecarsWithADoubleExtensionAreSkipped(t *testing.T) {
	for _, f := range []string{
		"watchexec-2.3.2-x86_64-unknown-linux-gnu.deb.b3",
		"tool-x86_64-unknown-linux-gnu.tar.gz.b3",
		"tool-x86_64-unknown-linux-gnu.tar.gz.minisig",
		"tool_1.0.0_linux_amd64.tar.gz.sha256",
	} {
		if a := Classify(f); a.Platform != "" {
			t.Errorf("Classify(%q) claimed platform %q; a sidecar must never win a platform key", f, a.Platform)
		}
	}
	// The real asset beside them still resolves.
	if a := Classify("watchexec-2.3.2-x86_64-unknown-linux-gnu.tar.xz"); a.Platform == "" {
		t.Error("the real asset must still be classified (whether we can unpack it is a separate question)")
	}
}
