package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The original accusation: Tags only refuses clone URLs starting with '-' (that
// check guards against `--upload-pack=<command>`). But git has a second
// URL-shaped arbitrary command execution — `ext::<command>`, which
// git-remote-ext hands to a shell — and it does not start with '-', so neither
// the `--` separator nor that prefix check stops it.
//
// Why it does not actually hold, on two independent counts:
//
//  1. git itself disabled the ext transport by default in 2.12
//     (protocol.ext.allow defaults to never), so what really happens is
//     "fatal: transport 'ext' not allowed".
//  2. More importantly, **this path is unreachable**: the clone URL's prefix comes
//     from the host table (the four built-ins, or whatever the user wrote into
//     config.toml), and internal/ref restricts owner/repo to [A-Za-z0-9._-] (not
//     even a colon). What a package author controls is toy.toml, and nothing in
//     toy.toml can reach the clone URL.
//
// So no code is added here (the user's own gitconfig is the user's business; that
// is not a package attack surface). The test stays as a regression guard: the day
// the clone URL becomes something that can be assembled from elsewhere, or someone
// adds a `-c protocol.ext.allow=always` style switch to Tags, it goes red.
func TestExtTransportDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "PWNED")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// git-remote-ext would sh -c this string
	_, err := Tags(ctx, "ext::sh -c \": > "+marker+"\"")
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatalf("host.Tags executed an arbitrary command via the ext:: transport (%s appeared), err=%v", marker, err)
	}
	if err == nil {
		t.Fatal("want an error: ext:: is not a clone URL you can discover versions from")
	}
	t.Logf("refused (err=%v)", err)
}
