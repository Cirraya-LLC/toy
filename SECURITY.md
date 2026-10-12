# Security

toy installs binaries. Installing a binary means putting somebody else's code on your
machine, so the point of this document is not to reassure you. It is to tell you
**exactly who you are trusting**.

---

## 1. Threat model

### What toy defends against

| Threat | How |
| --- | --- |
| Code execution at install time (`preinstall` / `postinstall` / build scripts / hooks) | These hooks **do not exist by design**. toy downloads, verifies a hash, extracts, and creates symlinks. That is the whole list |
| Package contents swapped out underneath you | A `[sha256]` block is enforced when present; otherwise the first install pins the hash (TOFU) and any later change is refused |
| Upstream tag force-pushed, release asset re-uploaded | Same answer — once a version is pinned it never re-points |
| Author identity taken over | The author's public key is pinned the same way; a changed key stops the install |
| Writing outside the package directory during extraction (tar-slip, symlink escape, hard links, absolute paths, paths hidden in PAX/GNU long-name headers) | The extractor refuses each of these, and **judges symlinks by real path, not lexical path** |
| Zip bombs | A total-byte ceiling, measured by **bytes actually written**, not by the size the archive claims |
| Shadowing a command already on PATH (`bin = "sudo"`) | Refused by default, `--force` overrides |
| A package claiming a name toy itself executes (`bin = "git"`) | Refused unconditionally — **`--force` does not override this one**. See §3 |
| Mirror poisoning | Mirror content is checked against the hash; **with no hash, mirrors are never used at all** |
| Transport downgrade | Once it is https it never falls back to http (the check scans the whole redirect chain, not just the last hop) |
| Terminal injection (control characters rewriting the trust line that was already printed) | Everything printed for a human goes through an escaping layer; tables are laid out in memory first, then emitted line by line |
| Credentials leaking out of a URL | Stripped at the moment the error is constructed, not left to the output layer |

### What toy does not defend against

This section matters more than the one above.

- **The package itself being malicious.** toy's guarantee is "nothing runs while you
  install it", not "what you installed is safe to run". The moment you run `tinyserve`
  it is an ordinary program that can do anything you can do. No sandbox, no permission
  manifest, no behavioral analysis.
- **The author's account being taken over.** Anyone with write access to the repo can
  push a new tag. TOFU only fires when *the same version* changes content, and
  **a new version is new content by definition**.
- **`✓ signed` does not mean "signed by the repo owner".** See §4. This is the one
  thing here that most needs saying precisely.
- **The host itself.** Installing via `github:owner/repo` means trusting GitHub's
  account system and TLS.
- **Write access to wherever an index is hosted.** An entry may inline a `toy.toml`
  (for upstreams that have not adopted toy yet), so whoever can edit that JSON
  **can change what you end up installing** while `source` stays byte-for-byte the same.
  The blast radius is bounded — they can only swap in a different asset from that repo's
  own releases and pick a different command name; they cannot change the download URL
  (that is built from the host template plus the tag), and they cannot claim a reserved
  name like `git` or a command already on your PATH. A package installed this way prints
  `⚠ packaging instructions come from the index`, and **that line is your only way to
  notice**. The index toy comes configured with (`default`, https://toy.cirraya.com) is
  published from this repository, so for it "write access" means push access to
  `Cirraya-LLC/toy` — and today nearly all of its entries are of this kind. If you do not
  want this tier, `toy index rm default` and add no third-party indexes:
  `github:owner/repo` never goes through an index at all.
- **Your own PATH.** A fake `git` in any directory on your PATH will be executed —
  that is what PATH means. toy only guarantees that **the one directory packages can
  write into** (`~/.toy/bin`) will not be that path.
- **The upstream supply chain.** A poisoned compiler on the author's machine, a tampered
  build in their CI — toy cannot see any of it. All it can promise is that the bytes you
  got are the same bytes the author published.
- **The first install.** The first use in trust on first use is unconditional trust. If
  what you install the first time is already bad, toy will faithfully pin it. On a first
  install, the thing to look at is where `source` points.
- **Denial of service.** There are size ceilings and a redirect ceiling, but no rate
  limiting and no tuned timeout policy.

---

## 2. What the eight iron laws mean for security

### Iron Law 1 — never execute any code from a package, ever

**This is why the project exists. Everything else follows from it.**

No install, no postinstall, no build, no hooks. Not "off by default", not "sandboxed" —
these concepts have no counterpart anywhere in the code.

What that blocks is a whole class of attack rather than one CVE: the Shai-Hulud worm ate
700+ npm packages through `preinstall`; Homebrew formulas are executable Ruby, which is
why 6.0 had to bolt on Tap Trust. Against a distributor with no execution entry point by
design, this class of attack has nothing to talk about — not "harder", but **nothing to
work with**.

This law has one non-obvious boundary, covered in §3: "nothing runs at install time" and
"this package never gets to run" are not the same sentence.

### Iron Law 2 — never build from source

Only ① release binaries and ② single-file scripts get installed; anything else is an
error. Building means owning a toolchain, and a toolchain is a far larger attack surface
than the package itself (and a far larger project).

### Iron Law 3 — no dependencies between packages

No dependency graph means no transitive trust. You install one package and that is what
you installed — there is no such thing here as "I audited the direct dependency, but its
47th transitive dependency was poisoned".

### Iron Law 4 — enforce sha256 when it is declared, pin it on first use when it is not

A checksum mismatch always refuses the install. This is also the **only** reason mirrors
(§8) do not have to be trusted: content can be checked against a hash. So when there is
no hash, the mirror fallback disappears entirely rather than degrading into "use it anyway".

**A cache hit re-hashes the file too.** The cache filenames *are* the content hashes, so
merely checking that a file exists would turn `~/.toy/cache` into a trust root anyone can
write to — and that directory is exactly what `toy serve` hands out.

### Iron Law 5 — never shadow a command already on PATH

Otherwise `bin = "sudo"` is a privilege-escalation hole. Refused by default, `--force`
overrides — a user *can* reason about the consequences of "yes, I am replacing `ls`".

**This law has a blind spot, and that blind spot was a critical. See §3.**

### Iron Law 6 — versions are immutable

Once a hash is recorded for some `ref@version` it never re-points. left-pad, protestware,
a force-pushed tag — all three really happened in other ecosystems, and they share one
shape: **same coordinate, different bytes**.

TOFU records are keyed **by asset name**, not by `ref@version`: the same version resolves
to different builds on a musl machine and a glibc machine, so pinning by version alone
would make a second machine sharing the same `$HOME` report "the content was swapped out"
— a false alarm in the one place you can least afford to cry wolf.

### Iron Law 7 — extraction must refuse path traversal

`../` and absolute paths are refused, and symlinks are not followed out of the destination.

The judgment is made on the **real path**, not the lexical one: one link inside the
archive pointing shallower than itself (`x -> "."`) is enough for lexical validation and
the kernel's resolution to be off by one level, and then a single `..` climbs out. The
induction "links we allowed must point inward, so following them cannot escape either" is
simply false.

Two more that are not obvious: **setuid / setgid bits never survive extraction**; and
**an unrecognized tar member type that declares content must fail loudly rather than be
skipped** — a `continue` lets the tar reader pull those bytes out of the gzip stream and
throw them away, where the byte ceiling never counts them.

### Iron Law 8 — print the trust level honestly, never succeed silently

This is the half of security that is aimed at a human. It covers three things:

1. **The trust level itself** (§4).
2. **What `--force` actually waived** — a silent waiver is the same as not having the
   check, so every waiver is printed, one line each.
3. **Where the bytes came from** — an extra line when the original URL was down and the
   package came from a mirror. Content is hash-addressed, so a mirror does not need to be
   trusted; "does not need to be trusted" is not the same as "does not need to be known".
   Platform-candidate fallback (you did not get the build made for this machine) is the
   same story.

Precisely because that line is the user's only basis for judgment, it is also a target —
see the terminal injection in §5.

---

## 3. The critical: "nothing runs at install time" and "nothing ever runs" are different sentences

A package can declare:

```toml
[bin]
git = "payload"
```

After installing, `~/.toy/bin/git` is on PATH — and `~/.toy/bin` is exactly the directory
`toy doctor` tells users to put **first** on their PATH. **toy discovers versions by
running `git ls-remote`**, so the next `toy install` / `toy update` / `toy doctor`
executes a file from that package.

Not one line ran during the install. Iron Law 1 holds literally. Execution was merely
**deferred by one command**.

**The shadowing check (Iron Law 5) does not stop this**: that check only catches "a
program with the same name already exists on PATH". On a machine that does not have git
yet — a Mac without Xcode CLT, a slim container, a CI base image, all of which are
normal, and the bare-URL install route needs git for nothing at all —
`LookPath("git")` finds nothing, so the check waves it through.

The fix is in two halves and **neither half works alone**:

1. `reservedLinkNames` in `internal/store`: refuse to link any package as `git`. This is
   the **only check in the entire project that `--force` does not waive** — no package has
   a legitimate reason to claim a name from toy's own toolchain, and a user cannot reason
   about the consequences of "confirm the override with --force".
2. `lookupGit()` in `internal/host`: walk PATH itself and resolve an absolute path,
   **skipping `$TOY_HOME/bin`** and skipping relative PATH entries. When the fake git is
   the only git on PATH, refuse to execute and say plainly that this is most likely an
   attack.

The first half only stops newly installed packages. The second half also refuses to run a
git that is already sitting in `bin/`.

**For whoever changes this code next**: the moment toy execs one more external program
**by name**, that name must be added to `reservedLinkNames`. `internal/host` may not
import `internal/store` (import cycle), so the bin-directory derivation exists in both
places — change one and forget the other and this defense fails silently, and failing
looks exactly like working.

Full reproductions live in `cmd/toy/ironlaw1_audit_test.go`,
`internal/store/link_audit_test.go`, and `internal/host/exec_audit_test.go`.

---

## 4. What each of the four trust levels actually proves

The last line of an install prints one of the four below. **Read them for what they do
not prove.**

### `✓ signed · key a3f9… · registered in index X (this is not a claim about github.com/Y themselves)`

The signed message is `id + "\n" + source + "\n" + the raw bytes of toy.toml`.

**All three are publicly readable, and not one byte of them binds this public key to the
repo's owner.**

An index is a JSON file anyone can fork and anyone can send a PR against, and copying a
public key costs nothing. So somebody can fork an index, re-sign the same `id` with
**their own** key, and point `source` at their own repo — and a first-time installer still
sees the checkmark.

**All it can prove is: the key registered in the index signed this version of the
`toy.toml`.** Together with TOFU (the public key is pinned on first install, and a changed
key refuses to install), what you get is **continuity**: whatever you installed from this
id last time was signed by the same person as this time.

Therefore:

- That line always carries the "this is not a claim about X themselves" tail.
- **Never print the repo owner after the checkmark.** `✓ signed · Derrity (key a3f9…)`
  reads as "Derrity signed this", which is vouching for a key nobody has identified.
- **On a first install, the thing to read is where `source` points, not whether there is
  a checkmark.**

Real attribution needs an index with an intake path that verifies the submitter has push
access to that repo. Until an index runs on a platform, the checkmark means only what is
written above. See `ROADMAP.md`.

### `· from github.com/Y · unsigned`

What you are trusting is **the host's account system**: GitHub says this repo belongs to
this account, and TLS says you reached GitHub. No author signature, and none needed —
this is the default state for the vast majority of packages, and it is an honest state,
not a downgrade.

### `⚠ packaging instructions come from the index · the bytes are from the author's own release, but which asset gets installed and what the command is called come from the index`

The author never wrote a `toy.toml`, so the index entry inlines one. Which means:
**the bytes are the author's, the packaging instructions are not.**

This tier sits below "host account" and above "unsigned", and **an inlined toyfile can
never reach `✓ signed`** — the signer is whoever submitted the index record, not the author.

We do not fudge this as "partially trusted": the user has to be able to judge the risk,
and the concrete shape of the risk is *who decided what command name this gets symlinked as*.

### `⚠ unsigned · first install, sha256 pinned (trust on first use)`

A bare URL. **You trusted nothing** — but if the content changes, that will be stopped.

---

## 5. The output is an attack surface too

That trust line is the user's only basis for judgment (Iron Law 8), which makes it a target.

A package's `name` and `desc`, and server text echoed back in an error, are all attacker
controlled. A `desc` containing `\r\x1b[32m✓ signed` can overwrite the line that was just
printed. Subtler: **the raw byte `0x9b`, the 8-bit form of CSI** — on a terminal that
interprets 8-bit C1, `CSI 1A` + `CSI 2K` is cursor-up one line plus erase the whole line.

So: everything printed for a human goes through the escaping layer, tables are laid out in
memory and then emitted line by line, and every cell is capped by rune count (an
absurdly long `name` can push a column off the screen without a single control character).
The trust column in `toy list` is **the first column** — nothing can push the first column
off the right edge.

Only `toy search --json` writes to stdout; that output is for machines.

---

## 6. Reporting a vulnerability

**Please do not open a public issue.**

Use GitHub private reporting: **Security → Report a vulnerability** on the repo
(Private vulnerability reporting).

These make a report move much faster:

- **A reproduction that runs.** Every confirmed finding in this project has a test behind
  it; we will write one from your repro and then flip it into a regression test.
- **A concrete bad outcome**, not "this looks insecure". This project overturned 17
  findings in its audit, almost all of them for "plausible, but unreachable" or "that is
  what the user explicitly asked for".
- Which column of §1 you think it belongs in. If it belongs in "what toy does not defend
  against" but you think it should not, that itself is a report worth having.

**What we will do**: acknowledge, triage, fix, and write down in `CHANGELOG.md` what it
was. Credit is public by default; say the word and we leave it out.

**What we will not do**: there is no bounty and no SLA. This is a small tool maintained by
one person; do not expect a 24-hour response.

**If we rule your report unreachable, we will write down why** rather than just closing it.
`docs/audit-2026-08.md` has 17 of those verdicts, which is the treatment they deserve.
