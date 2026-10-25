# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
versioning follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **When the newest tag has no release file, toy installs the release the forge marks as
  latest**, instead of only trying the tag before it. opencode tags a v2 line it
  publishes elsewhere, and its GitHub releases with binaries are v1.19.x: the one-step
  fallback reached v2.1.3 and gave up. toy reads the forge's public latest-release page
  (`/releases/latest` on GitHub, Gitea and Forgejo, `/-/releases/permalink/latest` on
  GitLab; a new optional `latest` host template) and uses its answer only if it is a
  tag `git ls-remote` listed. Without one, the old fallback still applies.

- **A universal macOS build is recognized** (`tool-macos-universal.tar.gz`): `toy init`
  and the index generator file it under both `darwin/arm64` and `darwin/amd64`, unless a
  build for one architecture exists, which wins. fastfetch switched to a single universal
  build in 2.70.0, and its entry pointed at macOS files that no longer existed.

### Changed

- **The example package is real again.** `Derrity/tinyserve`, the example in every
  doc, pointed at a GitHub account that no longer exists. TinyServe now lives at
  [Cirraya-LLC/TinyServe](https://github.com/Cirraya-LLC/TinyServe), ships its own
  `toy.toml` and self-contained binaries, and the index carries it — its first entry
  that points at an upstream `toy.toml` instead of inlining one.

## [0.1.1] - 2026-10-12

### Added

- **toy comes configured with the public index**, `default` →
  https://toy.cirraya.com/index.json, so `toy search` and `toy install owner/repo` work
  on a new install without `toy index add`. The site and the index are published from
  this repository by GitHub Pages. A `config.toml` written by 0.1.0 has no index —
  `toy index add default https://toy.cirraya.com/index.json` adds it, and the
  no-index messages in `search`, `install` and `doctor` now print exactly that line.

### Fixed

- **A tag pushed before its release files no longer breaks the install.** When toy
  picked the newest version itself and that release's file is a definite 404, it
  installs the release before it and says so, naming both; `toy update` waits quietly.
  A pinned `--version` never falls back, and neither does a 500 or a timeout. (rclone
  v1.75.2 was tagged an hour before its first file existed; every install in that hour
  failed.)

## [0.1.0] - 2026-10-12

First public release. It contains everything below, including the September
groundwork.

### Added

- **Strip the single top-level directory on extract** (equivalent to
  `tar --strip-components=1`): if the whole tree has exactly one top-level entry
  and it's a directory, drop that layer. cargo-dist tarballs wrap everything in
  `<name>-<version>-<triple>/`, and that name pins both the version and the
  platform — something the right-hand side of `[bin]` cannot express, which is why
  ripgrep / fd / bat / delta / eza were all uninstallable. After stripping,
  `[bin] rg = "rg"` just works. Never stripped: two or more top-level entries, a
  top-level file, a top-level symlink, an empty directory — and also not when
  stripping would make a relative symlink inside the package resolve outside the
  destination directory (iron law 7).
- **Glibc machines now fall back to a musl build as the last platform candidate**
  (`linux/amd64/gnu` → `linux/amd64` → `linux/amd64/musl`, same for arm64).
  ripgrep and zoxide ship only a musl build for x86-64, and a static musl binary
  runs fine on a glibc machine. **The reverse is never added**: a musl machine
  falling back to a glibc build genuinely won't start. When this candidate hits,
  the fallback note is printed as usual (iron law 8).
- **Asset names now recognize `i686`** (→ `386`). The 32-bit Linux builds from
  ripgrep / fd / zoxide used to be dropped silently.

### Changed

- FreeBSD / OpenBSD / NetBSD / Solaris / illumos assets moved from "can't tell
  which platform this is" to "recognized, but not an OS toy supports" — the first
  message asks the author to add an `[artifact]` line, and adding one wouldn't
  make it installable anyway.

## Groundwork - 2026-09-01

Not a release of its own; all of it shipped in 0.1.0.

### Added

- **Eleven subcommands**: `install` / `uninstall` / `update` / `list` / `search` /
  `info` / `index` / `keygen` / `publish` / `serve` / `doctor`.
- **Four install paths**: index name (`Derrity/tinyserve`), host prefix
  (`github:owner/repo`), other hosts (`gitlab:x/y` — three lines of config adds
  one), and a bare `toy.toml` URL.
- **Version discovery via `git ls-remote`**, never a host's REST API — so no host's
  rate limit can wall you off, and there's no per-host adapter to write.
- **`toy.toml` names only release assets** — no URLs, no version numbers, no host.
  Pushing a tag *is* the release; the file doesn't change by a single character.
- **Two package shapes**: release binary (unpack, then symlink) and single-file
  script (download + `chmod +x` + symlink). Neither builds, neither executes.
- **Platform candidate fallback**: an `[artifact]` key may carry a third segment
  (`linux/amd64/musl`), and the client tries candidates most-specific first. musl
  detection looks for `/lib/ld-musl-*.so.1`, arm generation reads `/proc/cpuinfo`,
  Rosetta is a static rule — all of it in pure functions. When what you get isn't
  the build made for this machine, it says so.
- **`toy init`**: derives a `toy.toml` from the asset filenames in `dist/`,
  replaces the version with the `{version}` placeholder, and computes `[sha256]` on
  the spot (which promotes the install side straight from TOFU to enforced
  checksums). Anything it guessed is called out as a guess at the end.
- **Four trust levels**, printed truthfully on the install-completed line: author
  signature / host account / packaging instructions from the index / unsigned.
  Meanings in `SECURITY.md`.
- **TOFU keyring**: artifact hashes and author public keys are pinned on first
  install; a later change refuses the install. The model is copied from SSH's
  `known_hosts`.
- **Index**: one JSON file, `https://` and local paths work the same, several can
  be configured. Entries hold only a pointer at the repo — deleting one just means
  "no longer findable by search".
- **`toy serve`**: turn the local cache into a mirror. Content is
  content-addressed, so a mirror doesn't need to be trusted; when bytes really did
  come from a mirror, the install says where from.
- **`toy publish` / `toy keygen`**: ed25519 signatures, both steps optional.
- **Install manifests** (`~/.toy/installed/`): what `uninstall` and `update` stand on.

### Security

An adversarial audit ran before the first release: eight attack surfaces searched in
parallel produced 33 findings; after three-perspective adversarial falsification, 16
were confirmed and 17 overturned. Full write-up in
[`docs/audit-2026-08.md`](docs/audit-2026-08.md).

**critical — iron law 1 bypassed completely:** a package can declare
`[bin] git = "payload"`. Once installed, `~/.toy/bin/git` is on PATH — and toy
itself discovers versions with `git ls-remote`, so the very next toy command
executes a file from that package. The shadow check doesn't stop it: on a machine
that doesn't have git yet (macOS without Xcode CLT, a slim container, a CI base
image — and the bare-URL install path needs no git at all), `LookPath` finds
nothing and waves it through. The fix has two halves and neither works alone:
`store`'s `reservedLinkNames` refuses unconditionally (**the only check in the whole
project that `--force` cannot override**), and `host`'s `lookupGit` walks PATH
itself to an absolute path, skipping `$TOY_HOME/bin`.

The rest of what was confirmed and fixed:

- Changing one `source` line in the index made `toy update` swap an installed
  package for the contents of a different repo — no confirmation, no TOFU warning,
  no change in trust level. The author key's TOFU pin hangs off an id derived from
  source, so changing source is changing identity.
- `toy search --json` reported "the index has a key" as `signed: true` without ever
  verifying a signature.
- A package's `name` could push the trust column of `toy list` off the screen and
  forge a `✓ signed` in its place.
- Output escaping missed the bare byte `0x9b` (the 8-bit form of CSI), which on a
  terminal interpreting 8-bit C1 could erase and rewrite the trust line above. The
  same code point written as valid UTF-8 *was* escaped correctly — so the outcome
  depended on whether the string happened to contain other control characters.
- Printing the repo owner after `✓ signed` vouches for a key nobody can identify.
- `uninstall` could delete another package's command; two packages could delete each
  other's `pkg` directories.
- `[artifact]` / `[script]` values could be written as `file:///…`, making toy read
  an arbitrary local file, install it onto PATH, and cache it into the directory
  `toy serve` hands out.
- HTTP redirects silently downgraded https to http (the check has to scan the whole
  via chain); artifact downloads had no size limit (the check has to count bytes
  actually written); credentials in a URL leaked into the error object — including
  one from `net/http` itself: when `CheckRedirect` fails it puts the original
  `Location` back into `url.Error.URL` without going through the stripper.
- Extraction: the three-step symlink escape (`x -> "."`, `x/up -> ".."`), because
  lexical validation resolves against the lexical parent while the kernel resolves
  against the real one; GNU sparse files were silently dropped as irregular members,
  leaving a command that doesn't exist; and `continue`-ing past an unrecognized tar
  member type that declared content made the reader decompress those bytes out of
  the gzip stream and throw them away, uncounted against the size limit.
- Fingerprints went from 8 hex characters to at least 128 bits: measured at roughly
  36k keygens/sec/core, 32 bits extrapolates to about 33 core-hours — and what has
  to be stopped here isn't a collision, it's a **preimage**.
- The private-key permission check became "not one bit for group or other" instead
  of "exactly 0600" — the old message was telling users to make a read-only private
  key writable.
- **Silent downgrades** caused by upstream deleting the newest tag now say so.
- A failed reinstall no longer wipes out the version that was working (the manifest
  survived and `list` still showed it — only the symlink dangled).

### Changed

- **`--force` split in two** (three independent perspectives hit this
  independently): `--reinstall` handles everyday reinstalls and touches no safety
  check; `--force` only accepts changes the safety checks reported, **and prints
  each thing it waved through**.
- **TOFU now pins per asset name**, not per `ref@version`: the same version is two
  different sets of bytes on a musl machine and a glibc machine, so pinning by
  version alone turns a shared `$HOME` into a false poisoning alarm.
- **v1 preconfigures no index** (`DefaultIndexURL` is empty). The public index isn't
  live yet, and shipping an address that won't resolve means every new user eats a
  red cross the first time they run `doctor`. "No index" is a legitimate state:
  `doctor` reports it neutrally instead of ✗, and `install github:owner/repo` works
  as always.
- Index lookup is now case-insensitive; every segment of a `pkg` path gets a
  disambiguating suffix (on APFS `Foo` and `foo` are the same directory, and without
  the suffix two different packages share one install directory — uninstalling one
  deletes the other's executable).

### Removed

- Dropped 613 lines of hand-written TOML parsing from `internal/store` (unnecessary
  once the dependency came back).

[Unreleased]: https://github.com/Cirraya-LLC/toy/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/Cirraya-LLC/toy/releases/tag/v0.1.1
[0.1.0]: https://github.com/Cirraya-LLC/toy/releases/tag/v0.1.0
