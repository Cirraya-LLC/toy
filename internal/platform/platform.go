// Package platform decides "which build should this machine get".
//
// Spec §3 says [artifact] keys are GOOS/GOARCH, but those two axes don't
// describe the real world:
//
//   - linux/amd64 says nothing about libc. Drop a glibc build into Alpine and
//     you get `sh: ./tool: not found` — what the dynamic loader says when it
//     can't find ld-linux, while the file is sitting right there. One of the
//     most misleading error messages ever written.
//   - Apple Silicon runs darwin/amd64 through Rosetta 2. Refusing it outright
//     means refusing a package that would have installed fine.
//   - GOARCH=arm doesn't distinguish armv6 (Pi Zero / Pi 1) from armv7
//     (Pi 2/3), and an armv7 binary on a Zero is an illegal instruction.
//
// So matching is not one exact comparison but a list of candidates tried in
// order, most specific first. All the reasoning lives in candidatesFor, a pure
// function; Candidates only does the filesystem probing.
package platform

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Candidates returns the platform keys to try, most specific first. Probes the
// filesystem.
func Candidates() []string {
	return candidatesFor(runtime.GOOS, runtime.GOARCH,
		detectMusl(runtime.GOOS, runtime.GOARCH), detectARM(runtime.GOARCH))
}

// Current describes this machine for humans; doctor prints it.
func Current() string { return describe(Candidates()) }

// Base is this machine's two-segment GOOS/GOARCH — the key an author actually
// writes in [artifact]. Use it when reporting "your platform is X" or "ask the
// author to add key Y"; never use Candidates()[0] for that. On Linux the first
// candidate is linux/amd64/gnu, which we synthesize ourselves — nobody
// publishes an asset called "gnu".
func Base() string { return runtime.GOOS + "/" + runtime.GOARCH }

// candidatesFor is the whole spec of this package. Pure function; every test
// aims here.
//
//	Alpine x86-64       linux/amd64/musl → linux/amd64
//	glibc x86-64        linux/amd64/gnu  → linux/amd64 → linux/amd64/musl
//	Apple Silicon       darwin/arm64     → darwin/amd64   (Rosetta 2, static rule, no probing)
//	Intel Mac           darwin/amd64
//	Raspberry Pi 3      linux/armv7      → linux/arm
//	Raspberry Pi Zero   linux/armv6      → linux/arm
//
// arm == 0 means we couldn't detect the generation — then give only linux/arm
// and DO NOT GUESS: guessing wrong hands the user a binary that dies with an
// illegal instruction.
func candidatesFor(goos, goarch string, musl bool, arm int) []string {
	base := goos + "/" + goarch
	switch {
	case goos == "linux" && goarch == "arm":
		// arm >= 7 is treated as armv7: 32-bit userspace on armv8 runs armv7
		// binaries fine, and nobody publishes an asset called linux/armv8.
		if arm >= 7 {
			return []string{"linux/armv7", base}
		}
		if arm == 6 {
			return []string{"linux/armv6", base}
		}
		return []string{base}
	case goos == "linux":
		if musl {
			// The reverse direction is DELIBERATELY ABSENT: a musl machine
			// falling back to a glibc build genuinely won't run, and the error
			// is `not found` (missing dynamic loader) while the file is right
			// there.
			return []string{base + "/musl", base}
		}
		// A glibc machine falls back to musl as a last resort: musl builds are
		// statically linked and run fine here (that's why people ship them),
		// and projects like ripgrep and zoxide ship musl ONLY for x86-64 — no
		// fallback means locking that whole set of packages out.
		return []string{base + "/gnu", base, base + "/musl"}
	case goos == "darwin" && goarch == "arm64":
		return []string{base, "darwin/amd64"}
	}
	return []string{base}
}

// describe turns the candidate list into a sentence. It reads the candidates
// themselves, so the description can never drift out of sync with what we
// actually try — that drift is the bug two independent probe paths always grow.
func describe(cands []string) string {
	switch len(cands) {
	case 0:
		return "unknown platform"
	case 1:
		return cands[0]
	default:
		return cands[0] + " (falling back to " + strings.Join(cands[1:], ", ") + " if missing)"
	}
}

// muslLoader maps GOARCH to the machine name inside the musl dynamic loader's
// filename — not the same thing as GOARCH (amd64 is called x86_64 there).
// Architectures not in this table are treated as glibc: falling back to the
// two-segment key still installs, whereas guessing musl wrongly makes the user
// download a binary that can't start at all.
var muslLoader = map[string]string{
	"amd64":   "x86_64",
	"arm64":   "aarch64",
	"386":     "i386",
	"arm":     "armhf",
	"riscv64": "riscv64",
	"s390x":   "s390x",
	"ppc64le": "powerpc64le",
}

func detectMusl(goos, goarch string) bool {
	if goos != "linux" {
		return false
	}
	machine, ok := muslLoader[goarch]
	if !ok {
		return false
	}
	_, err := os.Stat("/lib/ld-musl-" + machine + ".so.1")
	return err == nil
}

func detectARM(goarch string) int {
	if goarch != "arm" {
		return 0
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return armFromCPUInfo(string(b))
}

// armFromCPUInfo looks for the "CPU architecture: 7" line. Returns 0 when it
// can't tell.
func armFromCPUInfo(cpuinfo string) int {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "CPU architecture" {
			continue
		}
		// Old kernels write non-numeric values like "AArch64" here. That means
		// we don't know.
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return n
		}
	}
	return 0
}

// CPULevelHint roughly guesses the x86-64 microarchitecture level. IT IS FOR
// DOCTOR'S OUTPUT ONLY AND NEVER PARTICIPATES IN MATCHING.
//
// Getting this right means parsing CPUID, which the stdlib doesn't do, and
// pulling in a CPUID library would break the dependency policy (spec §10) — so
// microarchitecture matching is explicitly out of scope. But somebody who hits
// `Fatal glibc error: CPU does not support x86-64-v2` deserves a clue: nothing
// in that message points at "your CPU is too old".
// Returns "" off Linux, where there is no /proc/cpuinfo.
func CPULevelHint() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	return cpuLevelFromFlags(string(b))
}

// cpuLevelFromFlags only recognizes the x86 "flags" line — arm's /proc/cpuinfo
// writes "Features" instead, so this function returns "" on non-x86 without
// needing an explicit architecture check.
func cpuLevelFromFlags(cpuinfo string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "flags" {
			continue
		}
		has := map[string]bool{}
		for _, f := range strings.Fields(val) {
			has[f] = true
		}
		switch {
		case has["avx512f"]:
			return "x86-64-v4"
		case has["avx2"]:
			return "x86-64-v3"
		case has["sse4_2"] && has["popcnt"]:
			return "x86-64-v2"
		default:
			return "x86-64-v1"
		}
	}
	return ""
}
