package ref

import (
	"errors"
	"strings"
	"testing"
)

func TestParseOK(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Ref
	}{
		{"index form", "Derrity/tinyserve", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve"}},
		{"index form with version", "Derrity/tinyserve@v1.2.3", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve", Version: "v1.2.3"}},
		{"host form", "github:Derrity/tinyserve", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve"}},
		{"host form with version", "github:Derrity/tinyserve@v1.2.3", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve", Version: "v1.2.3"}},
		{"host names are case-insensitive", "GitHub:Derrity/tinyserve", Ref{Kind: KindHost, Host: "github", Owner: "Derrity", Repo: "tinyserve"}},
		{"another host", "codeberg:a/b", Ref{Kind: KindHost, Host: "codeberg", Owner: "a", Repo: "b"}},
		{"host names may contain dots and hyphens", "git.example-1:a/b", Ref{Kind: KindHost, Host: "git.example-1", Owner: "a", Repo: "b"}},
		{"surrounding whitespace", "  Derrity/tinyserve  ", Ref{Kind: KindIndex, Owner: "Derrity", Repo: "tinyserve"}},
		{"dots, underscores and hyphens in names", "a.b_c-d/e.f_g-h", Ref{Kind: KindIndex, Owner: "a.b_c-d", Repo: "e.f_g-h"}},
		{"pre-release version", "a/b@v1.2.3-rc.1", Ref{Kind: KindIndex, Owner: "a", Repo: "b", Version: "v1.2.3-rc.1"}},
		{"version with build metadata", "a/b@1.0.0+build.5", Ref{Kind: KindIndex, Owner: "a", Repo: "b", Version: "1.0.0+build.5"}},
		{"https", "https://example.com/toy.toml", Ref{Kind: KindURL, URL: "https://example.com/toy.toml"}},
		{"http", "http://example.com/x/toy.toml", Ref{Kind: KindURL, URL: "http://example.com/x/toy.toml"}},
		{"file", "file:///home/u/toy.toml", Ref{Kind: KindURL, URL: "file:///home/u/toy.toml"}},
		{"scheme is normalized to lowercase", "HTTPS://example.com/toy.toml", Ref{Kind: KindURL, URL: "https://example.com/toy.toml"}},
		{"an @ inside a URL is not a version", "https://u@example.com/toy.toml", Ref{Kind: KindURL, URL: "https://u@example.com/toy.toml"}},
		// Host names are case-insensitive: without lowercasing, two addresses
		// differing only in case become two TOFU records, and Example.COM slips
		// past the hash already pinned for example.com (spec §4).
		{"host name is normalized to lowercase", "https://Example.COM/Toy.TOML", Ref{Kind: KindURL, URL: "https://example.com/Toy.TOML"}},
		{"host name with a port is normalized too", "HTTPS://Example.COM:8443/toy.toml", Ref{Kind: KindURL, URL: "https://example.com:8443/toy.toml"}},
		{"file allows the localhost host name", "file://localhost/home/u/toy.toml", Ref{Kind: KindURL, URL: "file://localhost/home/u/toy.toml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", c.in, err)
			}
			c.want.Raw = c.in
			if got != c.want {
				t.Fatalf("Parse(%q)\ngot  %+v\nwant %+v", c.in, got, c.want)
			}
			// String()'s contract is "a canonical form that parses again", so
			// every accepted shape must round-trip — not just a few spot checks.
			again, err := Parse(got.String())
			if err != nil {
				t.Fatalf("Parse(%q).String() = %q no longer parses: %v", c.in, got.String(), err)
			}
			again.Raw = got.Raw
			if again != got {
				t.Fatalf("Parse(%q) does not round-trip:\nfirst    %+v\nreparsed %+v", c.in, got, again)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"no slash", "tinyserve"},
		{"two slashes", "a/b/c"},
		{"trailing slash", "a/b/"},
		{"empty owner", "/b"},
		{"empty repo", "a/"},
		{"empty segment", "a//b"},
		{"owner is dot-dot", "../b"},
		{"repo is dot-dot", "a/.."},
		{"owner is a dot", "./b"},
		{"repo is a dot", "a/."},
		{"backslash in repo", `a/b\c`},
		{"backslash in owner", `a\b/c`},
		{"a percent-encoded slash is not an allowlisted character", "a/b%2Fc"},
		{"space in a name", "a b/c"},
		{"colon in a name", "a/b:c"},
		{"non-ASCII", "作者/包"},
		{"owner too long", strings.Repeat("a", 101) + "/b"},
		{"repo too long", "a/" + strings.Repeat("b", 101)},
		{"a Windows drive letter is not a host prefix", "C:foo"},
		{"Windows drive letter with backslashes", `C:\Users\x\toy.toml`},
		{"Windows drive letter with slashes", "C:/Users/x/toy.toml"},
		{"single-letter host", "g:a/b"},
		{"empty host", ":a/b"},
		{"host starts with a digit", "9git:a/b"},
		{"host with no owner/repo after it", "github:tinyserve"},
		{"nothing after the host", "github:"},
		{"empty version", "a/b@"},
		{"version is dot-dot", "a/b@.."},
		{"slash in the version", "a/b@v1/2"},
		{"a second @ in the version", "a/b@v1@v2"},
		{"version too long", "a/b@" + strings.Repeat("1", 101)},
		{"unrecognized scheme", "ftp://example.com/toy.toml"},
		{"git scheme", "git://example.com/x.git"},
		{"ssh scheme", "ssh://git@example.com/a/b"},
		{"https with no host name", "https:///toy.toml"},
		{"file with no path", "file://toy.toml"},
		// The host name after file:// is ignored entirely by whoever reads it
		// (only u.Path is used), so a ref that looks like a remote address in
		// fact reads a local file — the last thing that should ever happen
		// silently.
		{"file with a host name", "file://evil.com/etc/passwd"},
		{"file with an uppercase host name", "FILE://EVIL.COM/etc/passwd"},
		{"a port with no host name", "https://:8443/toy.toml"},
		{"userinfo with no host name", "https://user@/toy.toml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if err == nil {
				t.Fatalf("Parse(%q) should have been rejected, but returned %+v", c.in, got)
			}
			if !errors.Is(err, ErrBadRef) {
				t.Fatalf("the error from Parse(%q) does not wrap ErrBadRef: %v", c.in, err)
			}
			// On error the value must be a clean zero Ref: a caller who forgot
			// to check err must not get a half-filled Ref (right Host, empty
			// Owner), which turns into a silently malformed request.
			if got != (Ref{}) {
				t.Fatalf("Parse(%q) returned an error and a half-filled Ref: %+v", c.in, got)
			}
		})
	}
}

func TestID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Derrity/tinyserve", "Derrity/tinyserve"},
		{"github:Derrity/tinyserve@v1.2.3", "Derrity/tinyserve"},
		{"https://example.com/toy.toml", ""}, // a bare URL has no index key
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := r.ID(); got != c.want {
			t.Errorf("Parse(%q).ID() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Derrity/tinyserve", "Derrity/tinyserve"},
		{"Derrity/tinyserve@v1.2.3", "Derrity/tinyserve@v1.2.3"},
		{"github:Derrity/tinyserve", "github:Derrity/tinyserve"},
		{"github:Derrity/tinyserve@v1.2.3", "github:Derrity/tinyserve@v1.2.3"},
		{"GitHub:Derrity/tinyserve", "github:Derrity/tinyserve"},
		{"  a/b  ", "a/b"},
		{"https://example.com/toy.toml", "https://example.com/toy.toml"},
		{"file:///home/u/toy.toml", "file:///home/u/toy.toml"},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := r.String(); got != c.want {
			t.Fatalf("Parse(%q).String() = %q, want %q", c.in, got, c.want)
		}
		// The canonical form must parse again and yield the same Ref (Raw aside).
		again, err := Parse(r.String())
		if err != nil {
			t.Fatalf("Parse(%q).String() = %q no longer parses: %v", c.in, r.String(), err)
		}
		again.Raw, r.Raw = "", ""
		if again != r {
			t.Fatalf("does not round-trip: %+v vs %+v", again, r)
		}
	}
}

// Error messages have to talk like a person: at minimum they must name the
// input that failed, or the user can't tell which one it was.
func TestErrorMentionsInput(t *testing.T) {
	for _, in := range []string{"a/b/c", "C:foo", "ftp://x/y", "a/b@"} {
		_, err := Parse(in)
		if err == nil {
			t.Fatalf("Parse(%q) should have failed", in)
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf("the error from Parse(%q) does not mention the input itself: %v", in, err)
		}
	}
}
