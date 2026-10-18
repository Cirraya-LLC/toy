# Contributing

Two very different things live in this repository, and they are contributed to in
different ways.

- **The tool** — Go, in `cmd/` and `internal/`.
- **The index** — one JSON file, `index/toy-index.json`, listing packages people
  can install. Most of it describes software we do not own.

The reasoning behind most decisions lives next to the code that carries it: in
comments that say *why*, and in the regression tests, many of which open with what
went wrong and what the test now guards. Keep that reasoning when you change
something, and add to it.

---

## Getting set up

```bash
make check      # gofmt, build, vet, test
make e2e        # the offline end-to-end run
```

`make check` needs nothing but Go. `make e2e` also wants `git` and `python3`, and
it builds a fake forge on the local filesystem — it never touches the network and
never touches your real `~/.toy`.

There are exactly two third-party dependencies, and adding a third needs a real
argument. Most of what a package manager wants is already in the
standard library; `compress/bzip2` and `crypto/ed25519` are both there.

---

## Changing the tool

**Read the eight iron rules first.** A change that breaks one removes the reason
this project exists.

1. **Nothing from a package is ever executed** — no install hook, no build step,
   no post-install anything, and there never will be.
2. **Nothing is built from source.** A release binary, a single-file script, or an
   error.
3. **No dependencies between packages.** Every package is self-contained.
4. **A declared sha256 is enforced**; without one, the first download's hash is
   pinned and a later change is refused.
5. **A command already on PATH is never shadowed** unless the user passes `--force`.
6. **Versions are immutable**: once a hash is recorded for a version, it never
   points anywhere else.
7. **Extraction blocks path traversal**: `../`, absolute paths and symlinks that
   lead out of the package are refused.
8. **The trust level is printed, honestly**, on every install. The client is the
   only security boundary.

Each package under `internal/` owns one thing — `archive` extraction, `fetch`
downloads, `store` the `~/.toy` layout and symlinks, `sign` signatures and pinning,
`ui` everything a human reads. A fix that has to reach across several of them is
usually a sign the problem is somewhere else.

If you are about to change extraction, symlinking, checksums, tag resolution or
terminal output, read the comments on the functions you are touching and the
`*_audit_test.go` files first. They record what went wrong before, and why the code
is shaped the way it is.

**Every non-trivial change wants a test that fails without it.** Comment the fix
out and run the test; if it still passes, the test is decoration. Several of the
regression tests here exist because a green suite proved nothing.

Comments here carry reasoning, not restatement. "This uses a lock" is noise;
"this is under the lock because two concurrent installs each read and rewrite the
whole file, and the later write erased the earlier pin" is the comment that stops
someone removing it.

---

## Adding something to the index

Most entries are **inline entries**: we write the packaging
instructions for a repository that has no `toy.toml` of its own. That is how the
index can be useful before anybody has adopted us, and it is also the part with
real obligations attached.

```bash
make index-gen ARGS="--repos index/repos.txt --probe --out index/toy-index.json"
make index-check ARGS="--only owner/repo"
```

Most new entries come from the weekly discovery run, which opens a pull request on
its own (`index/README.md`, "Automatic discovery"). Reviewing that pull request is the
same job as reviewing yours. Only repositories with a licence that allows
redistribution get in; `index/licenses.txt`, `index/delisted.txt` and
`index/declined.txt` are the three lists a person keeps.

`--probe` is not optional. Without it the path on the right of `[bin]` is a guess,
and for anything packaged by cargo-dist the guess is wrong.

**An entry that cannot install is worse than no entry.** `make index-check` really
installs it, really runs it, and really removes it again. If it does not pass,
the entry does not go in — three of the first fifty candidates installed
successfully and then would not run, and the answer for all three was to leave
them out.

Three obligations that come with listing other people's software:

1. **Never imply endorsement.** Every install prints that the packaging
   instructions came from the index and not from the author. That line is the
   only place a user can notice, so it does not get quieter.
2. **If a maintainer -- or anyone with a claim to the project -- asks for it to be
   delisted, delist it.** The index does not judge a project's provenance. No conditions, no
   discussion. There is a form for it. `go run ./cmd/toy-index delist --reason "..."
   owner/repo` removes the entry and records it, so the weekly discovery leaves it out.
3. **Someone reads the command names on the left of `[bin]`.** Reserved names and
   the PATH shadow check block the dangerous cases automatically, but a name that
   is not on PATH *yet* is available to take. This is the one step in the pipeline
   that stays manual on purpose.

---

## The website

`site/` is three static pages generated from one template plus `site/i18n.json`
(Simplified Chinese, Traditional Chinese, English). `make site` builds them.

The tool is English-only; the website is not. Copy that quotes real CLI output —
the trust table, the sample sessions — stays in English in all three languages,
because translating it would be putting words in the program's mouth.

---

## Reporting things

- A package that stopped installing → **stale index entry** issue. Most likely
  our entry is out of date, not the author's release being broken.
- A vulnerability → private advisory, see `SECURITY.md`. Not a public issue.
- Anything else → an ordinary issue is fine.
