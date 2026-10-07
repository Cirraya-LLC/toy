# toy

<!-- Swap OWNER/REPO once the repo is pushed and the badge goes live. -->
[![ci](https://github.com/Cirraya-LLC/toy/actions/workflows/ci.yml/badge.svg)](https://github.com/Cirraya-LLC/toy/actions/workflows/ci.yml)

**Install the small tools nobody packages for you. Nothing is executed at install time.**

```bash
toy install github:Derrity/tinyserve   # know where the repo is? install it
tinyserve                              # it just works
```

To find tools by name (`toy search json`, `toy install Derrity/tinyserve`) you need an
index first — **v1 ships with none**. See "Install from anywhere" below.

---

## Why

Because getting a small tool into someone else's hands is absurdly long today.

Debian means waiting out a 2–3 year release cycle, and
[packaging anything non-trivial can eat five days of your life](https://news.ycombinator.com/item?id=26211842).
Homebrew core means review. Opening a tap means
[running a stranger's Ruby on other people's machines](https://docs.brew.sh/Taps) —
which is exactly why Homebrew 6.0 had to bolt on Tap Trust.

So everyone falls back to `curl | bash` and hands the whole machine to a script nobody read.

toy's answer is to cut both ends off: **publishing is 8 lines of toml, and installing
executes nothing.**

## Two rules that will never change

**1. No code runs at install time.**
No install, no postinstall, no build, no hooks. Not one.
toy downloads, verifies the hash, extracts, and creates a symlink. That's the whole verb list.

This isn't laziness, it's the reason the project exists. The Shai-Hulud worm that ate
700+ npm packages spread through `preinstall` hooks. A distributor with no hook mechanism
at all doesn't have that attack surface to defend.

**2. Packages have no dependencies on each other.**
Every toy is one self-contained binary or one single-file script. No dependency graph
means no dependency hell, and no left-pad.

## Publishing a tool

Drop a `toy.toml` at the root of your repo:

```toml
name    = "tinyserve"
desc    = "a very small static file server"
license = "MIT"

[bin]
tinyserve = "tinyserve"

[artifact]
"linux/amd64"  = "tinyserve_{version}_linux_amd64.tar.gz"
"darwin/arm64" = "tinyserve_{version}_darwin_arm64.tar.gz"
```

Push a tag, cut a release. **Done.**

Already using goreleaser? You don't have to write that file by hand:

```bash
toy init          # reads the asset filenames in ./dist and works the toy.toml out backwards
```

It rewrites the version into a `{version}` placeholder, computes `[sha256]` for you
(which upgrades the install side from trust-on-first-use to enforced verification), and
says outright at the end which parts are guesses — especially the right-hand side of
`[bin]`, the in-package path, which you'll have to fix if your archive has subdirectories
(`bin/` and friends). The case where the whole tree is wrapped in a single top-level
directory (cargo-dist's `tool-1.2.3-x86_64-unknown-linux-musl/`) needs no attention:
that layer gets stripped at extraction time. It will not overwrite an existing `toy.toml`.

No URL, no version, no host name — so the next release doesn't touch this file at all.
No registration, no review queue, nobody's permission.

```bash
toy install github:you/your-tool   # anyone can install it right now
```

Want to be searchable? Send one entry to an index. Run `toy keygen` once and you can sign
your own entries. **Both steps are optional.**

### Platform keys: two segments is enough, add a third when you need it

`[artifact]` keys are normally just `GOOS/GOARCH`. But `linux/amd64` says nothing about
libc, and a glibc build dropped into Alpine reports `not found` — with the file sitting
right there. So a key may carry one more segment, and toy tries candidates **from most
specific to most generic**, installing the first hit:

| Your machine | Tried in order |
| --- | --- |
| Ordinary Linux (glibc) | `linux/amd64/gnu` → `linux/amd64` |
| Alpine (musl) | `linux/amd64/musl` → `linux/amd64` |
| Apple Silicon | `darwin/arm64` → `darwin/amd64` (Rosetta 2) |
| Raspberry Pi 3 / Zero | `linux/armv7` / `linux/armv6` → `linux/arm` |

**As an author you can ignore all of this**: write `linux/amd64` and be done, which is what
the vast majority of projects do — a bare `linux/amd64` is almost always a glibc build.
You only need that extra `"linux/amd64/musl"` line if you **actually ship both** glibc and
musl binaries.

**When the thing installed wasn't built for this machine, toy says so** — Rosetta costs you
speed, and a glibc build on a musl box simply won't start. The reverse case stays quiet:
a package that only declares two-segment keys landing on ordinary Linux gets no extra noise,
because that build *is* the one meant for you.

## toy tells you who you're trusting

```
✓ signed · key a3f9… · registered in index community (this is not proof of github/Derrity)
· source github.com/Derrity · unsigned      you're trusting GitHub's account system
⚠ packaging instructions from the index · bytes come from the author's release, but which asset and which command name come from the index
⚠ unsigned · first install, sha256 pinned   you trusted nothing, but a content change gets stopped
```

A checksum mismatch means the install is refused. A changed public key or hash warns loudly
and stops. The model is copied straight from SSH's `known_hosts`.

**"✓ signed" proves continuity, not identity.** Anyone can fork an index, anyone can send a
PR, so there is no binding whatsoever between that public key and a GitHub account — someone
can fork an index, re-sign the same `id` with **their own** key, point `source` at their own
repo, and a first-time installer still sees the check mark. What it actually guarantees is:
**whatever you installed from this id last time was signed by the same person as this time**
(the key is pinned on first install; a change is refused). That's why the line always carries
the "not proof of X" tail. On a first install the thing to read is where `source` points, not
whether there's a check mark.

**What the third tier, "packaging instructions from the index", means.** Almost none of the
small tools out there have a `toy.toml` yet, so an index entry may inline one (see
"Install from anywhere" below). **The bytes are still the author's** — the download URL is
assembled from the host template plus the tag, and the index has no say in it. All the index
can change is *which asset from that release gets installed and what the command is symlinked
as*. So it sits one tier below "host account" and one above "trusted nothing", and it
**can never earn the check mark**, not even when the entry is signed: the signer is whoever
submitted that record, not the author.

The day upstream adds its own `toy.toml`, the inlined copy steps aside immediately and toy
mentions it. The other direction (last time it was the author's own declaration, this time
it's the index's) counts as a downgrade and stops for a `--force`.

## Install from anywhere

```bash
toy install Derrity/tinyserve            # look it up in the index
toy install github:Derrity/tinyserve     # skip the index, read the repo directly
toy install gitlab:x/y                   # three lines of config adds another host
toy install https://…/toy.toml           # fully bare
```

An index entry is **by default just a pointer at a repo**, and **the git URL is always the
truth**. Deleting an entry only means "no longer searchable" — it still installs. Nobody
can actually take a package down.

One exception: when the upstream repo has no `toy.toml` yet, an entry may inline one
(`toy publish --inline`), so the thousands of existing small tools are installable without
waiting for their authors. The cost is stated in the open — a package installed this way
drops to the third trust tier, and the install says which half wasn't written by the author.
The moment upstream has its own `toy.toml`, the inlined copy yields unconditionally.

**v1 preloads no index at all**, because the public one isn't live yet — better to admit
that than to ship an address that won't resolve and give you a red X on your very first
`toy doctor`. Add one yourself to turn search on (an index is just a JSON file; `https://`
and a local path work the same):

```bash
toy index add local ./index/toy-index.json          # the seed index in this repo
toy index add community https://example.com/index.json
toy index list                                       # what's configured
```

Having no index doesn't stop you installing anything: `toy install github:owner/repo` never
goes through an index in the first place.

Hosts are a config table, not three implementations, and version discovery uses
`git ls-remote` — so toy depends on nobody's API and gets throttled by nobody's rate limit.

## Commands

```
toy install [--reinstall] [--force] [--version v] <ref>   install a package
toy uninstall <name>                             remove it (the cache stays)
toy update [name]                                upgrade; no name means everything
toy list                                         what's installed
toy search [--limit n] [--json] <query>          search the indexes (purely local, no network search)
toy info <ref>                                   look before you install: platforms, command names, trust
toy init [--dist ./dist] [--name x] [--force]    derive a toy.toml from the asset names in dist
toy index add <name> <url> | rm <name> | list    index management
toy keygen                                       generate an ed25519 keypair
toy publish [--inline] [--source github:owner/repo] [dir]  sign, and print an entry you can paste into an index
toy serve [--addr host:port]                     turn your local cache into a mirror
toy doctor                                       check PATH, directory permissions, index reachability
```

**Flags go before the package name** (`toy install --force github:o/r`) — Go's flag parser
stops at the first non-flag argument.

`--reinstall` and `--force` are two different things, don't conflate them: `--reinstall`
only says "I know this version is installed, install it again" and it **waives no safety
check whatsoever**; `--force` is "I looked, I accept these changes" (hash changed / key
rotated / command name shadows something / trust tier downgraded / source changed), and it
prints every single thing it waived. One switch doing both jobs means "let me just reinstall"
quietly accepts the other five.

`toy serve` turns your local cache into a mirror. Because content is addressed by hash, a
mirror doesn't have to be trusted — anyone can run one, it's five minutes of work. Upstream
disappears, the package lives on. **If a mirror was actually used, the install says where
the bytes came from** — not needing to trust it isn't the same as not needing to know.

## What we don't do

No building from source, no inter-package dependencies, no P2P/DHT, no Windows (for now).
Every one of those was cut on purpose; the reasoning is in the design notes.

## Installing toy itself

**Let's say the awkward part first: the first package manager always has to arrive some
other way.** toy exists so you don't have to `curl | bash`, and toy can't install itself —
there's no way out of that loop, and anyone claiming to have solved it has just moved it
somewhere else. So neither option below is perfect; the difference is **who you're trusting**,
and we're spelling it out.

**Option 1: `go install`** (easiest, if you have a Go toolchain)

```bash
go install github.com/Cirraya-LLC/toy/cmd/toy@latest
```

You're trusting Go's module proxy and checksum database (`sum.golang.org`) — a public,
auditable transparency log. That's stronger than any promise we could make, so use this
one if you have Go.

**Option 2: download the release binary and verify the checksum yourself**

```bash
# pick your platform: linux_amd64 / linux_arm64 / darwin_amd64 / darwin_arm64
V=0.1.0; P=linux_amd64
BASE=https://github.com/Cirraya-LLC/toy/releases/download/v$V

curl -fLO $BASE/toy_${V}_${P}.tar.gz
curl -fLO $BASE/checksums.txt

# Verify. This step is not optional — skip it and you are trusting that download path unconditionally.
sha256sum --ignore-missing -c checksums.txt    # macOS: shasum -a 256 --ignore-missing -c

tar -xzf toy_${V}_${P}.tar.gz
install -m 0755 toy ~/.local/bin/toy           # or any directory on your PATH
```

**On a Mac, download with `curl` as above, not with a browser.** A browser marks the file as
quarantined, `tar` carries that onto the extracted `toy`, and Gatekeeper refuses to run it —
the binary is signed ad hoc, not notarized. If it happened anyway:
`xattr -d com.apple.quarantine toy`. (Packages toy installs for you are fetched over HTTP and
never quarantined, the same as every other command-line installer.)

`checksums.txt` and the binary come from the same release, so it guards against "altered in
transit", not against "the publisher is the problem". **The only defense against the latter
is the signature on the release page and this repo's history** — which is exactly what toy
does for every package it installs (hash pinned on first install, refused if it changes
later), and exactly what nobody did for toy itself this one time. That's the bootstrap
paradox in its concrete form.

After installing:

```bash
export PATH="$HOME/.toy/bin:$PATH"   # put this in ~/.bashrc / ~/.zshrc
toy doctor
```

`doctor` will say "no index configured" — that's the default state, not a fault, and it
doesn't affect installing anything.

## License

MIT
