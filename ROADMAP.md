# Roadmap

This document answers three questions: **what's done**, **what we won't do and why**,
and **where the known gaps are**.

The second section is longer than the first. That's deliberate — a distributor's
character is defined by what it refuses to do.

---

## 1. What v0.1.0 already does

- Four install paths (index name / `github:` / other hosts / bare `toy.toml` URL)
- Version discovery via `git ls-remote`, no host's API involved
- Two package shapes: release binary, single-file script
- Platform candidate fallback (`linux/amd64/musl`, Rosetta, armv6/armv7)
- `toy init`: derives a `toy.toml` from `dist/`, `[sha256]` computed on the spot
- Four trust levels + a TOFU keyring
- Multiple indexes; `toy serve` mirrors
- ed25519 signing (`keygen` / `publish`), optional
- Install manifests, which is why `uninstall` / `update` / `list` are reliable

---

## 2. What we deliberately won't do

### No building from source

**We install exactly two things: (1) release binaries, (2) single-file scripts.
Anything else is an error.**

The moment you build, you own a toolchain: which Go/Rust/Node version, pinned how,
isolated how, and what to say when the build fails. Three times the project, and
**a toolchain is a far larger attack surface than the package it builds**.

More fundamentally: building *is* execution. A distributor that executes nothing
by design cannot have an "except when building" exception.

**What would change our mind**: nothing. This is iron law 1 wearing a different hat.
People who need to install from source should use `cargo install` / `go install` —
those tools do it better than we would.

### No inter-package dependencies

**Every toy is self-contained.**

A dependency graph is apt rebuilt from scratch, and apt is the thing we're reacting
against. Graphs bring version solving, conflicts, diamond dependencies, transitive
trust, and left-pad.

And the target here is "the small tools nobody packaged for you" — a static binary
or a script. It never needed a graph.

**The cost, stated plainly**: toy can't install anything that needs a runtime (a
script that wants Python 3.11, a program dynamically linked against a pile of
libraries). Those aren't our users.

### No P2P / DHT / NAT traversal

Artifacts are content-addressed and signature-checkable, so **a mirror is
inherently untrusted anyway**. And if you don't have to trust it, "where do I find
a mirror" is a low-risk question — **and low-risk questions get stupid answers**: a
list of URLs in a config file, tried in order when the original fails.

The value of P2P is "the package survives its upstream disappearing", and
`toy serve` already delivers that in about fifteen lines. Auto-discovery only fills
in entries on that list; the payoff is nowhere near the cost of NAT traversal and
peer discovery.

**What would change our mind**: people actually using mirrors, with hand-maintaining
the mirror list as the bottleneck. Even then the answer isn't a DHT — it's a static
"known mirrors" list.

### No Windows

v1 is Linux + macOS only.

Not because it doesn't matter, but because Windows symlinks (developer mode or
admin), the `.exe` suffix, PATH semantics, and "how does an unpacked file become a
command you can type" are a whole separate pile of work that doesn't map cleanly
onto this model.

`toy init` **skips Windows assets and says why** rather than dropping them
silently — that's the door left open.

**What would change our mind**: someone actually asking. Unlike the three above,
this one is "not yet", not "no".

### No x86-64 microarchitecture levels (v2/v3/v4)

It requires parsing CPUID, the standard library won't do it, and pulling in a
library violates the dependency policy (exactly two third-party deps, see
the design notes §10). `toy doctor` prints one hint. That's all.

### Ratings are not a leaderboard

Install counts and votes are an **admission signal for the index**, not a chart.
A vote with no consequence is decoration — AUR votes have a consequence (promotion
into the official repos), which is why they're real there.

---

## 3. Known gaps

This is the most important section in this document.

### 1. Signatures prove nothing about ownership (the big one)

`✓ signed` proves **continuity**, not identity. The signed message is
`id + source + the raw toy.toml`, all three of which are publicly readable, and
**not one byte of it binds the key to the repo's owner**. Anyone can fork the index,
re-sign the same `id` with their own key, and point `source` at their own repo.

So that line always carries the "does not mean the person themselves" tail, and
**what you should look at on a first install is where `source` points, not whether
there's a checkmark**. See `SECURITY.md §4`.

**What fixing it needs**: an index entry point that **verifies the submitter has
push access to that repo**. While the index is "a JSON file plus a PR", this can't
be done — it needs something on the index side that runs code.

Until then, don't claim more for that checkmark than it earns.

### 2. Index cold start

**v1 ships with no index preconfigured**, because the public index isn't hosted
anywhere yet. `DefaultIndexURL` is an empty string constant — once the index is
live, changing that **one constant** is the whole job.

The cold-start plan is **scraping awesome-lists** rather than waiting for people to
register: those lists are already in `owner/repo` form, which is exactly what an
index entry needs.

Until then, `toy install github:owner/repo` is fully usable end to end — the index
only affects whether you can find something by name.

### 3. The musl and Rosetta fallbacks **have not been verified on real hardware**

The platform-candidate detection is all pure functions with test coverage, but
those tests are fed constructed inputs.

**Two things unverified on real hardware**:
- Installing a package that only ships `linux/amd64` on Alpine (musl) — it should
  install and **print an extra note** (a glibc binary on Alpine reports `not found`,
  even though the file is right there).
- Installing a package that only ships `darwin/amd64` on Apple Silicon — it should
  go through Rosetta 2 and say so. On a machine without Rosetta it will simply fail
  to open.

The armv6 / armv7 generation check (reading `/proc/cpuinfo`) likewise only has
constructed-input tests.

These three are the parts of v0.1.0 we're least sure about. If you hit one, open an
issue.

### 4. Everything else

- **No uniform `--json`**: only `toy search` has it; `toy list` / `toy info` don't yet.
- **No parallel installs**: `toy update` goes one at a time.
- **The host table has four built-in entries**; adding one means editing
  `config.toml`. That's by design (a table beats code), but there's no path yet for
  "contribute a host config".
- **`reservedLinkNames` is maintained by memory**: the next time toy adds an
  external program it execs *by name*, a line has to go in there — and nothing
  automated will remind whoever adds the second `exec.Command`.
  See `docs/audit-2026-08.md`.
- **`internal/host` and `internal/store` each carry their own copy of the bin-dir
  derivation** (importing across would cycle); the comments point at each other.
  Change one and forget the other, and that critical defense fails silently.

---

## 4. What's likely next

No dates. Ordered by "someone actually needs it":

1. Host the public index, fill in that empty constant, scrape a round of
   awesome-lists for the cold start.
2. Verify the musl / Rosetta fallbacks on real hardware.
3. Ownership verification on the index side — i.e. make `✓ signed` actually able to
   say "the owner of this repo signed it".
4. Extend `--json` to `list` / `info`.
5. Windows, if someone asks.
