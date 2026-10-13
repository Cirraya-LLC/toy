# The index

One JSON file. The client downloads the whole thing and **searches locally**. It
holds pointers, never substance — the git address in `source` is always the truth.

**This file is the public index.** Every push to `main` that changes it is published
byte for byte to https://toy.cirraya.com/index.json (`.github/workflows/pages.yml`),
which is the index toy comes configured with (`DefaultIndexURL` in
`internal/store/store.go`). So **merging an entry here ships it to every user's
`toy search`** within about ten minutes — that is why entries are checked before they land.

To try a change locally before that:

```bash
toy index add local ./index/toy-index.json     # a bare path is expanded to file:///absolute/path
toy index add local file:///absolute/path/index/toy-index.json   # spelling it out works too
```

The client has only ever known "a URL", so moving the index to another host is a DNS
change for `toy.cirraya.com`, not a client change.

An entry's `source` may only be a host form (`github:owner/repo`) or an **`https://`**
bare URL. The index is remote content: allowing `file://` would let any index order
the client to read local files, and allowing plaintext `http://` would let it point
at things like `169.254.169.254`, which only mean something on the victim's machine.
Non-conforming entries are skipped silently and counted (`toy search` warns how many
were skipped).

## What an entry looks like

```json
{
  "id":       "Cirraya-LLC/TinyServe",
  "source":   "github:Cirraya-LLC/TinyServe",
  "desc":     "a tiny single-binary HTTP file server, stub HTTP server and TCP port forwarder",
  "tags":     ["http", "server"],
  "key":      "a3f9…"
}
```

`key` is the author's ed25519 public key, present only after they ran `toy keygen`
to claim the name. A package with no key is still findable and still installable.

`sig` (generated alongside it by `toy publish`) is the author's signature over
`id + source + that exact toy.toml`. **A key alone does not count as signed** —
anyone can fork this file, anyone can open a PR, and copying a public key costs
nothing. The client prints `✓ signed` only after verifying `sig`; if verification
fails it downgrades to "from …" and says why.

## Adding one

```bash
cd your-project && toy publish     # prints an entry you can paste straight in
```

## How we maintain this index

### Generating entries in bulk

`cmd/toy-index` is the maintainer's generator: feed it a list of `owner/repo` and it
produces **inlined entries** ready to drop into this JSON. It is a
**separate binary, not a `toy` subcommand** — the `toy` client never uses a host's
REST API, while an offline batch job may.

```bash
make index-gen ARGS="--repos index/repos.txt"            # only add new entries
make index-gen ARGS="--repos index/repos.txt --update"   # refresh existing ones too
make index-gen ARGS="--repos index/repos.txt --probe"    # also unpack real archives to nail down [bin]
export GITHUB_TOKEN=...   # without a token you get 60 requests/hour and hit the limit fast
```

One `owner/repo` per line, `#` starts a comment (`index/repos.txt` doesn't exist yet
— make your own; the cold-start source is scraped awesome-lists, which are already
in exactly this format). When it finishes, **read the report**,
especially the last section: the command names this batch will put on PATH.

Two things it **deliberately does not do**. Don't remove them:

- **It never writes `[sha256]`.** An inlined entry follows upstream's newest tag, so
  pinning one version's bytes turns the next upstream release into "checksum
  mismatch, refusing to install" — which reads exactly like a poisoning warning when
  all that happened is the author shipped a normal release. First installs are still
  pinned by TOFU.
- **It never touches signed entries or pointer entries.** Regenerating a signed one
  would invalidate the signature (quietly removing a `✓`); a pointer entry means
  upstream has its own `toy.toml`, and replacing that with an inlined one is a
  downgrade, not a refresh. `--update` does not override either.

### Listing other people's software: three policies

The overwhelming majority of entries point at **someone else's** repo, and the
author has no idea we listed them. Therefore:

1. **Imply no endorsement.** Listing something says only "this package installs".
   It does not say we audited its code, and it does not say the author has any
   relationship with us. Generated entries are always inlined, so what the client
   displays is the **downgraded** "packaging instructions come from the index"
   marker, never `✓ signed` — that checkmark belongs only to an author who signed
   with their own key. We copy `desc` verbatim from upstream; we don't write
   a recommendation on its behalf.
2. **If a maintainer asks to be delisted, delist them. No questions.** Deleting an
   entry only means it stops being findable — `toy install github:owner/repo` still
   installs it, and anyone who forks this file has a complete parallel index. When
   delisting costs that little, there is never a reason to argue with anyone.
   A policy nobody can act on is not a policy, so there is a form for it:
   [Remove my project from the index](https://github.com/Cirraya-LLC/toy/issues/new?template=delist.yml).
   An entry going stale has its own form, because a stale inline entry is our
   mistake and not the author's — see
   [Stale index entry](https://github.com/Cirraya-LLC/toy/issues/new?template=stale-entry.yml).
   `toy` prints that second link itself when a download 404s on an inline entry:
   at that moment the user is about to blame an author who did nothing wrong.
3. **When listing, a human reads the command name on the left of `[bin]`.** This is
   the one step in the whole pipeline that cannot be automated: `reservedLinkNames`
   blocks `git`, and the PATH shadow check blocks commands that already exist on
   this machine — but a name that does **not yet** exist on PATH can be claimed
   (that is exactly the shape of the most serious finding the security audit turned up). The generator's
   report lists this batch's names separately at the end, precisely so a human takes
   that look.

### Entries rot, so there's a regression check

Upstream changes its asset naming habits once and that entry stops installing —
without us touching a line of code. `scripts/index-check.sh` **actually installs,
runs, and uninstalls** every single entry. CI runs it daily (`index-check.yml`), and
on a PR it only tests the entries the diff touched.

```bash
make index-check          # installs, runs and removes every entry (this platform only)
make index-urls           # HEADs every platform each entry declares                             # everything (needs network, really downloads)
make index-check ARGS="--only owner/repo"    # just one
```

Failures come in three kinds and are handled completely differently: **entry** =
fix that entry in this JSON; **network** = retry, don't touch the index; **run** =
it installed but won't run (upstream's release is broken, or the in-package path on
the right of `[bin]` was guessed wrong).

## What's in this index right now (verified 2026-09-01)

**19 entries, all inlined.** The candidates were 20 repos (`index/repos.txt`), run
through `toy-index gen --repos index/repos.txt --probe`, and then **every entry
passed two gates**:

1. **URLs**: compute each platform's download URL exactly the way toy does (the
   host's release template + the highest semver tag from `git ls-remote --tags` +
   expanding `{version}`/`{tag}`) and `curl -sSIL` every one of them —
   **all 122 URLs returned 200**.
   The computation has to match the client character for character or this check
   proves nothing: **semver selection must ignore prereleases the same way the
   client does** (`semver.Max`'s default), or you end up HEAD-ing a tag the client
   would never install — tstack/lnav's highest semver tag is `v0.14.1-rc1`, while
   the client installs `v0.14.0`.
2. **Actually installing**: `scripts/index-check.sh` installed, ran, and uninstalled
   all 19 on linux/amd64 (glibc) — **19 passed / 0 failed**.

This round doubled as real-hardware acceptance for three things: stripping
the top-level directory, glibc falling back to musl, and i686. The
cargo-dist projects — ripgrep / fd / bat / delta / eza / zoxide — were all
uninstallable last round; this round, **on a glibc machine, we installed the musl
build and stripped that `ripgrep-15.2.0-x86_64-unknown-linux-musl/` layer**, and
`rg --version` actually ran.

### The 19 entries

| Repo | Command | Platforms | Installed/ran locally |
| --- | --- | --- | --- |
| BurntSushi/ripgrep | `rg` | 6 | ✅ |
| ClementTsang/bottom | `btm` | 9 | ✅ |
| Peltoche/lsd | `lsd` | 9 | ✅ |
| ajeetdsouza/zoxide | `zoxide` | 7 | ✅ |
| bootandy/dust | `dust` | 9 | ✅ |
| charmbracelet/glow | `glow` | 6 | ✅ |
| chmln/sd | `sd` | 7 | ✅ |
| dalance/procs | `procs` | 2 | ✅ |
| dandavison/delta | `delta` | 6 | ✅ |
| eza-community/eza | `eza` | 4 | ✅ |
| jesseduffield/lazygit | `lazygit` | 5 | ✅ |
| junegunn/fzf | `fzf` | 6 | ✅ |
| mikefarah/yq | `yq` | 6 | ✅ |
| muesli/duf | `duf` | 7 | ✅ |
| sharkdp/bat | `bat` | 9 | ✅ |
| sharkdp/fd | `fd` | 9 | ✅ |
| sharkdp/hyperfine | `hyperfine` | 8 | ✅ |
| tstack/lnav | `lnav` | 3 | ✅ |
| wagoodman/dive | `dive` | 4 | ✅ |

### Not listed: XAMPPRocky/tokei

The latest release `v14.0.0` **has no assets at all** (a source-only release).
GitHub's auto-attached source tarballs don't count as assets, and toy doesn't build
from source (iron law 2), so this repo can't be listed right now — when it ships
binaries again, rerun the generator. No code change needed.

### The names on the left of `[bin]`: a human looked

`reservedLinkNames` blocks `git`, and the PATH shadow check blocks commands that
**already exist** on this machine. What's left for a human is "the name is too
generic" and "it collides with another project" — neither of which a machine can
judge. Four categories came out of the review:

**1. Same name as a completely different program — the one worth writing down**

- **`delta`**: Debian already has a `delta` package, a "heuristic minimizer of
  interesting files", with nothing to do with git-delta. On a machine that has it,
  the shadow check stops us (correctly), but users on both sides will assume they're
  the same thing.
- **`yq`**: there is another `yq` (kislyuk's Python implementation, a YAML wrapper
  around jq) with completely different command-line syntax. The entry here is
  mikefarah's Go implementation.

**2. Upstream itself got renamed by distributions over a collision**

Debian's package descriptions say it outright: `fd` became `fdfind` and `bat` became
`batcat`, both "because of a file name clash". Which means **`fd` and `bat` really
do collide in the real world** — just not necessarily with something installed on
every machine, and the shadow check only fires when it is.

**3. Too generic (no collision, but worth remembering)**

`sd` (two letters), `dust`, `procs`, `dive`, `glow` are all very ordinary words.
They don't conflict with any common command today, but **first come, first served**:
whoever installs first claims the name. `lsd` is one letter away from `ls`, so a
typo lands you in a different program.

**4. Command name doesn't match repo name (not a problem — a discoverability one)**

`ClementTsang/bottom → btm`, `BurntSushi/ripgrep → rg`. Someone searching for
"bottom" gets a command called `btm`. These two are exactly what `--probe` fixed:
**without `--probe` the command name comes from the asset name's prefix**
(`ripgrep-15.2.0-….tar.gz` → `ripgrep`), while the file inside the package is called
`rg` — which writes out as `[bin] ripgrep = "rg"`. It installs and it runs, but the
name it puts on PATH appears in nobody's documentation on earth (nobody has ever
written `ripgrep --version`). Now that we probe, the basename we find wins.

### Known pitfall: a bare binary's `[bin]` only held for one platform

**Bare binaries are fixed as of 2026-09-01.** Before that, install dropped an asset
with no compression suffix into the package directory **under its original asset
name** — and asset names differ per platform (`yq_linux_amd64` /
`yq_darwin_arm64`), while `[bin]` is a single line that doesn't vary by platform.
So on every platform except the one `[bin]` happened to be right for, the symlink
pointed at a file that didn't exist — **and it blew up only after the download and
the checksum had both succeeded**. `mikefarah/yq` was broken on 5 of its 6 platforms
exactly this way, and listing had only verified it on linux/amd64, so nobody saw it.

`insUnpack` now drops a single file at the path on the right of `[bin]`
(`yq = "yq"`), which holds on every platform. The generator's matching special case
is gone.


## What deleting an entry does

**It stops being findable. That's all.** `toy install github:owner/repo` still
installs it. Anyone who forks this file has a complete parallel index — nobody can
truly kill a package, including us.
