package host

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// selfHostedHint's output ends up on the terminal via ui.Error, and ui.safe
// escapes every control character into a visible \xNN (iron law 8: nobody gets to
// overwrite the trust line with an injected \r or ESC, and that gate does not open
// for the sake of formatting). So a single \n or \t in this template would reach
// the user as the literal string "\x0a\x09" — worse than giving no hint at all.
// This test pins that down.
func TestSelfHostedHintHasNoControlCharsToEscape(t *testing.T) {
	hint := selfHostedHint("mygitea")
	for i, r := range hint {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("selfHostedHint character %d is the control char %q — "+
				"ui.safe will show the user \\x%02x. The whole hint is: %q", i, r, r, hint)
		}
	}
}

// Having no control characters is not enough: that line exists to be pasted whole
// into config.toml, so it must actually parse back into a Host with all four
// templates set.
func TestSelfHostedHintPastesBackIntoAWorkingHost(t *testing.T) {
	hint := selfHostedHint("mygitea")
	line := hint[strings.Index(hint, "hosts."):]

	var cfg struct {
		Hosts map[string]Host `toml:"hosts"`
	}
	if _, err := toml.Decode(line, &cfg); err != nil {
		t.Fatalf("the hint's line is not valid TOML: %v\nthe line is: %s", err, line)
	}
	h, ok := cfg.Hosts["mygitea"]
	if !ok {
		t.Fatalf("parsing produced no hosts.mygitea; got %#v", cfg.Hosts)
	}
	if h.isEmpty() {
		t.Fatalf("the parsed Host is empty — pasting it into config.toml would hit the "+
			"\"sets no templates at all\" error: %#v", h)
	}
	for name, tmpl := range map[string]string{
		"raw": h.Raw, "clone": h.Clone, "release": h.Release, "web": h.Web,
	} {
		if strings.TrimSpace(tmpl) == "" {
			t.Errorf("template %s is empty — the hint is missing that key", name)
		}
	}
}
