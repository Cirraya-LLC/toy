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

Every addition is a pull request; `index-check.yml` installs, runs and uninstalls the
entries it touches on Linux and macOS, and a person merges it.

- **The repo ships its own `toy.toml`**: add a pointer entry by hand, in id order, with
  just `id`, `source` and `desc` (Cirraya-LLC/TinyServe is one). Upstream keeps the
  packaging; nothing here can rot.
- **It does not**: put `owner/repo` in `index/repos.txt` and run the generator below. It
  writes a pointer entry by itself if it finds an upstream `toy.toml`.
- **You are the author and want a signed entry**: `toy publish` in your repo prints one.

Most additions arrive without anyone typing a repo name: see "Automatic discovery".

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

One `owner/repo` per line, `#` starts a comment (`index/repos.txt` is the hand-kept
seed list). When it finishes, **read the report**, especially the last section: the
command names this batch will put on PATH.

Before writing anything it refuses a repo when:

- **its licence does not allow redistribution** (see "Licences" below);
- it is in **`index/delisted.txt`** (its maintainer asked) or **`index/declined.txt`**
  (a reviewer turned it down);
- the **command name still carries a platform word** (`yt-dlp_linux_aarch64`,
  `gemini-darwin`): the asset names fooled the inference, and that name would only be
  right on one machine. Write such an entry by hand: put the toy.toml in a file and run
  `go run ./cmd/toy-index add --toyfile tool.toml owner/repo`, which holds it to the same
  rules (licence, no `[sha256]`, no platform words, no upstream toy.toml) and does the
  JSON escaping. ast-grep went in that way.

And when upstream already ships a `toy.toml` at its latest tag, it writes a **pointer
entry** instead of inlining one. Only a definite 404 counts as "no toy.toml"; a 500
fails that repo rather than guessing.

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

### Automatic discovery

`index-discover.yml` runs every Monday (and on demand from the Actions tab, with a cap
on how many repos). **One batch at a time**: while a discovery pull request is still open,
the run skips -- review and merge (or close) it first. It goes:

1. **`toy-index discover`** searches GitHub by topic (`cli`, `command-line`,
   `command-line-tool`, `tui`, `cli-app`) **and reads curated lists**
   (agarrharr/awesome-cli-apps, ibraheemdev/modern-unix, rothgar/awesome-tuis) for the
   well-known tools that never tagged a topic -- opencode, OpenAI's codex, croc and gum
   came in that way. Most stars first, at least 1000 stars,
   pushed in the last two years, and **created at least six months ago** -- a tool
   people install from here should have a public track record. (The first run's top
   AI agents were a few months old with star counts nobody could vouch for. Too new is
   not "no": they come back on their own once old enough.) It keeps a repo only if `gen` would produce an entry
   for it, so the cap is spent on tools that ship something installable rather than
   on libraries that share the topic.
2. **`toy-index gen --probe`** writes their entries into a scratch index.
3. **`index-check`** installs, runs and uninstalls each one on Linux and on macOS.
4. **`toy-index merge`** keeps what passed somewhere and failed nowhere, and the run
   opens a pull request. Its description lists every entry, **flags the command names
   to look at**, and lists popular repos whose licence a person has to read.
5. **It merges itself when no name could take another's**: a name already in the index,
   a common system command or a generic word holds the PR for a person (it says so in a
   comment); a short name (`gh`, `jc`, `hl` -- what upstream calls them) is shown but holds
   nothing up. The run waits for the PR's four required checks, merges, and starts the
   site deploy itself (a merge made with the workflow's token starts no push workflows).
   An unreviewed PR therefore no longer stops the index from growing.

GitHub holds the checks of a pull request opened by `github-actions[bot]` until a
maintainer approves them; the discovery run approves its own right after opening it,
and if that ever fails the PR shows **Approve and run** -- one click. The run that
opened it is linked at the top, with every index-check log. **A PR that waits for you**:
check out its branch, take out what you do not want, push, merge. A merged one can still
be corrected afterwards with the same commands on a new branch.

```bash
go run ./cmd/toy-index decline --reason "a GUI app" owner/repo   # never proposed again
go run ./cmd/toy-index delist --reason "maintainer asked, #12" owner/repo
go run ./cmd/toy-index drop owner/repo                           # not now; may come back
```

Each removes the entry from `index/toy-index.json`; `decline` and `delist` also record
the repo, with the reason and date, in `index/declined.txt` / `index/delisted.txt`.

```bash
GITHUB_TOKEN=$(gh auth token) go run ./cmd/toy-index discover --max 20 --out /tmp/c.txt --review /tmp/r.txt
```

runs the first step locally.

### Licences

toy never rehosts anything, so listing a project strictly needs no permission from its
licence. The bar is higher on purpose: the index is something people install from, and
`toy serve` lets anyone re-serve what they downloaded, which is redistribution. So an
entry needs a licence that allows using and redistributing the binaries:
**OSI-approved, or a public-domain dedication** (the allowlist is in
`cmd/toy-index/license.go`; copyleft is fine). Source-available licences (BUSL, SSPL,
Elastic, PolyForm, Commons Clause) are out, and **no licence at all means all rights
reserved**, so that is out too.

GitHub cannot identify every licence file (jq's COPYING, a dual MIT/Apache repo). Those
are not refused for good: **a person reads the file** and, if it allows redistribution,
adds `owner/repo SPDX-id  # where it was read` to **`index/licenses.txt`**. That names the
licence; it does not get anything past the allowlist.

### Listing other people's software: three policies

The overwhelming majority of entries point at **someone else's** repo, and the
author has no idea we listed them. Therefore:

1. **Imply no endorsement.** Listing something says only "this package installs".
   It does not say we audited its code, and it does not say the author has any
   relationship with us. A generated entry is inlined unless upstream ships its own
   `toy.toml`, so what the client displays is the **downgraded** "packaging
   instructions come from the index" marker or the host tier, never `✓ signed` —
   that checkmark belongs only to an author who signed with their own key. We copy `desc` verbatim from upstream; we don't write
   a recommendation on its behalf.
2. **If a maintainer asks to be delisted, delist them. No questions.** Deleting an
   entry only means it stops being findable — `toy install github:owner/repo` still
   installs it, and anyone who forks this file has a complete parallel index. When
   delisting costs that little, there is never a reason to argue with anyone.
   **The same goes for a rights holder.** The index does not judge where a project came
   from: a reimplementation, a fork with a disputed history, anything GitHub hosts in
   public under an open licence is eligible. If someone with a claim to it objects, it
   is delisted the same way, without an argument -- listing costs nothing to undo.
   **Delist with `go run ./cmd/toy-index delist --reason "..." owner/repo`**: it removes
   the entry and records the repo in `index/delisted.txt`, without which the weekly
   discovery would put it back.
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

## What's in it

The JSON file is the list, and https://toy.cirraya.com shows it with search. It
started on 2026-09-01 as 19 hand-picked Rust and Go tools from `repos.txt`; that first
round is where the two lessons below come from.

One from that round still applies to every check of an index: **compute URLs exactly
the way the client does**, prereleases included. tstack/lnav's highest semver tag is
`v0.14.1-rc1`; the client installs `v0.14.0`, so a check that HEADs the rc proves
nothing.

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
