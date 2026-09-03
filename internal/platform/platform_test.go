package platform

import (
	"runtime"
	"strings"
	"testing"
)

// candidatesFor is the spec of this package (the table in the package doc), so
// pin it line by line.
func TestCandidatesFor(t *testing.T) {
	cases := []struct {
		name         string
		goos, goarch string
		musl         bool
		arm          int
		want         []string
	}{
		// No reverse direction here: a musl machine falling back to a glibc
		// build genuinely won't run (it reports `not found`).
		{"Alpine x86-64", "linux", "amd64", true, 0, []string{"linux/amd64/musl", "linux/amd64"}},
		// A glibc machine falls back to musl last: statically linked musl
		// binaries run fine here, and ripgrep / zoxide ship musl only for
		// x86-64 — without the fallback that whole set is uninstallable.
		{"glibc x86-64", "linux", "amd64", false, 0, []string{"linux/amd64/gnu", "linux/amd64", "linux/amd64/musl"}},
		{"Apple Silicon can run amd64 via Rosetta", "darwin", "arm64", false, 0, []string{"darwin/arm64", "darwin/amd64"}},
		{"Intel Mac does not fall back to arm64", "darwin", "amd64", false, 0, []string{"darwin/amd64"}},
		{"Raspberry Pi 3 is armv7", "linux", "arm", false, 7, []string{"linux/armv7", "linux/arm"}},
		{"Raspberry Pi Zero is armv6", "linux", "arm", false, 6, []string{"linux/armv6", "linux/arm"}},
		{"linux/arm64 musl", "linux", "arm64", true, 0, []string{"linux/arm64/musl", "linux/arm64"}},
		{"linux/arm64 gnu", "linux", "arm64", false, 0, []string{"linux/arm64/gnu", "linux/arm64", "linux/arm64/musl"}},
		// Unknown generation means don't guess: an armv7 binary on a Zero is an
		// illegal instruction.
		{"unknown arm generation gives only the bare key", "linux", "arm", false, 0, []string{"linux/arm"}},
		// 32-bit userspace on armv8 runs armv7 binaries fine, and nobody
		// publishes linux/armv8.
		{"arm >= 7 is treated as armv7", "linux", "arm", false, 8, []string{"linux/armv7", "linux/arm"}},
		// An unrecognized OS gets the plain two-segment key; don't invent
		// fallbacks out of nothing.
		{"freebsd has no fallback", "freebsd", "amd64", false, 0, []string{"freebsd/amd64"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := candidatesFor(c.goos, c.goarch, c.musl, c.arm)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("candidatesFor(%q,%q,%v,%d) = %v, want %v", c.goos, c.goarch, c.musl, c.arm, got, c.want)
			}
			// The most specific candidate must come first: the ordering IS the
			// point of falling back. The check is "the first candidate is not
			// the two-segment base" — string length is not a usable proxy for
			// specificity, since a glibc machine's last candidate
			// (linux/amd64/musl) is a character longer than its first.
			if len(got) > 1 && c.goos == "linux" && got[0] == c.goos+"/"+c.goarch {
				t.Errorf("the two-segment base must not come first: %v", got)
			}
		})
	}
}

// The exact GOOS/GOARCH key must always be among the candidates — every
// toy.toml in existence writes only that key, and the fallback layer is not
// allowed to lose them.
func TestBareKeyIsAlwaysACandidate(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		musl         bool
		arm          int
	}{
		{"linux", "amd64", true, 0}, {"linux", "amd64", false, 0},
		{"linux", "arm", false, 7}, {"linux", "arm", false, 6},
		{"darwin", "arm64", false, 0}, {"darwin", "amd64", false, 0},
	} {
		base := c.goos + "/" + c.goarch
		found := false
		for _, p := range candidatesFor(c.goos, c.goarch, c.musl, c.arm) {
			if p == base {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not among the candidates: %v", base, candidatesFor(c.goos, c.goarch, c.musl, c.arm))
		}
	}
}

func TestArmFromCPUInfo(t *testing.T) {
	pi3 := "processor\t: 0\nmodel name\t: ARMv7 Processor rev 4 (v7l)\nCPU architecture: 7\n"
	if got := armFromCPUInfo(pi3); got != 7 {
		t.Errorf("armv7 = %d", got)
	}
	zero := "processor\t: 0\nCPU architecture: 6\nFeatures\t: half thumb\n"
	if got := armFromCPUInfo(zero); got != 6 {
		t.Errorf("armv6 = %d", got)
	}
	// Unreadable or non-numeric means "don't know": return 0, never invent a
	// generation.
	for _, s := range []string{"", "flags: avx2\n", "CPU architecture: AArch64\n", "CPU architecture\n"} {
		if got := armFromCPUInfo(s); got != 0 {
			t.Errorf("%q should be 0, got %d", s, got)
		}
	}
}

func TestCPULevelFromFlags(t *testing.T) {
	line := func(flags string) string { return "processor\t: 0\nflags\t\t: " + flags + "\n" }
	cases := map[string]string{
		line("fpu vme sse4_2 popcnt avx avx2 avx512f"): "x86-64-v4",
		line("fpu sse4_2 popcnt avx avx2"):             "x86-64-v3",
		line("fpu sse4_2 popcnt"):                      "x86-64-v2",
		line("fpu vme de pse tsc"):                     "x86-64-v1",
		// Missing popcnt alone makes it not v2 — which is exactly what causes
		// that x86-64-v2 glibc error.
		line("fpu sse4_2"): "x86-64-v1",
		// arm's /proc/cpuinfo writes Features, not flags: don't force a level
		// out of it.
		"processor\t: 0\nFeatures\t: half thumb neon\n": "",
		"": "",
	}
	for in, want := range cases {
		if got := cpuLevelFromFlags(in); got != want {
			t.Errorf("cpuLevelFromFlags(%q) = %q, want %q", in, got, want)
		}
	}
}

// The hint is for doctor's eyes only and never participates in matching, so it
// must not look like a platform key.
func TestCPULevelHintIsNeverAPlatformKey(t *testing.T) {
	for _, s := range []string{"x86-64-v1", "x86-64-v2", "x86-64-v3", "x86-64-v4", CPULevelHint()} {
		if strings.Contains(s, "/") {
			t.Errorf("CPU level hint %q contains /, it could be mistaken for a platform key", s)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := describe([]string{"darwin/amd64"}); got != "darwin/amd64" {
		t.Errorf("a single candidate should come back verbatim: %q", got)
	}
	got := describe([]string{"linux/amd64/musl", "linux/amd64"})
	for _, want := range []string{"linux/amd64/musl", "linux/amd64"} {
		if !strings.Contains(got, want) {
			t.Errorf("the description is missing %q: %q", want, got)
		}
	}
	if describe(nil) == "" {
		t.Error("even an empty candidate list needs a sentence")
	}
}

// Candidates/Current really do read the filesystem, so only assert the
// invariants: non-empty, most specific first, and this machine's GOOS/GOARCH is
// in there.
func TestCandidatesOnThisMachine(t *testing.T) {
	got := Candidates()
	if len(got) == 0 {
		t.Fatal("Candidates() must not be empty")
	}
	base := runtime.GOOS + "/" + runtime.GOARCH
	found := false
	for _, p := range got {
		if p == base {
			found = true
		}
	}
	if !found && runtime.GOARCH != "arm" {
		t.Errorf("%s is not in %v", base, got)
	}
	if !strings.Contains(Current(), got[0]) {
		t.Errorf("Current() = %q does not contain the first candidate %q", Current(), got[0])
	}
}

// The musl fallback is one-way: a glibc machine may fall back to a musl build
// (statically linked, runs fine), but a musl machine MUST NEVER fall back to a
// glibc build — that genuinely doesn't start, and the error looks like a
// missing file. Flipping this rule turns no other test red, so pin it on its
// own.
func TestMuslFallbackIsOneWay(t *testing.T) {
	for _, goarch := range []string{"amd64", "arm64"} {
		base := "linux/" + goarch
		glibc := candidatesFor("linux", goarch, false, 0)
		if last := glibc[len(glibc)-1]; last != base+"/musl" {
			t.Errorf("last candidate for glibc %s = %q, want %q", goarch, last, base+"/musl")
		}
		for _, c := range candidatesFor("linux", goarch, true, 0) {
			if c == base+"/gnu" {
				t.Errorf("a musl machine must not get the candidate %s: %v", c, candidatesFor("linux", goarch, true, 0))
			}
		}
	}
}
