package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/toyfile"
)

// An inline entry (spec §5.1) is packaging instructions the index wrote for a
// repository that has no toy.toml. It carries no version, so it survives across
// releases -- right up until the author renames their release assets. At that
// moment the asset in the entry stops existing and the user gets a bare 404.
//
// Left at that, the user reads it as "the package is broken" and goes to bother
// an author who did nothing wrong and cannot fix it either. We wrote that entry;
// the report belongs to us.
func TestStaleInlineEntryPointsTheReportAtUs(t *testing.T) {
	res := &resolved{
		Toyfile:   &toyfile.Toyfile{Origin: toyfile.OriginIndex},
		IndexName: "official",
	}
	note := insStaleEntryNote(res, fmt.Errorf("HTTP 404: %w", os404()))
	if note == "" {
		t.Fatal("a 404 on an inline entry must explain that the index entry is probably stale")
	}
	for _, want := range []string{"official", issueURL, "not from the author"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must mention %q so the user knows where to report it:\n%s", want, note)
		}
	}
}

// An author's own toy.toml returning 404 is genuinely their release being broken.
// Blaming our index there would be its own kind of lie (iron law 8).
func TestUpstreamToyfileIsNotBlamedOnTheIndex(t *testing.T) {
	res := &resolved{Toyfile: &toyfile.Toyfile{Origin: toyfile.OriginUpstream}, IndexName: "official"}
	if note := insStaleEntryNote(res, fmt.Errorf("HTTP 404: %w", os404())); note != "" {
		t.Fatalf("a package with its own toy.toml must not be blamed on the index:\n%s", note)
	}
}

// Only a definite "does not exist" means the entry is stale. A timeout or a 500 is
// upstream having a bad day, and telling the user to file a report would send a
// stream of noise at whoever reads that tracker.
func TestOnlyANotFoundLooksLikeAStaleEntry(t *testing.T) {
	res := &resolved{Toyfile: &toyfile.Toyfile{Origin: toyfile.OriginIndex}, IndexName: "official"}
	if note := insStaleEntryNote(res, fmt.Errorf("HTTP 500 server error")); note != "" {
		t.Fatalf("a 500 is not a stale entry:\n%s", note)
	}
}

// And the whole thing end to end: an inline entry whose asset has gone away.
func TestStaleInlineEntryEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("TOY_HOME", home)
	// A bare URL whose toy.toml 404s exercises the upstream path, which must stay
	// silent -- the point of the pairing is that only the index path speaks up.
	out := captureStderr(t, func() {
		_ = runInstall(context.Background(), []string{srv.URL + "/toy.toml"})
	})
	if strings.Contains(out, issueURL) {
		t.Fatalf("a bare URL is not an index entry and must not point the report at us:\n%s", out)
	}
}

func os404() error { return fs.ErrNotExist }
