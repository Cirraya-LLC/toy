# Contributing

Two very different things live in this repository, and they are contributed to in
different ways.

- **The tool** — Go, in `cmd/` and `internal/`.
- **The index** — one JSON file, `index/toy-index.json`, listing packages people
  can install. Most of it describes software we do not own.

the design notes is the specification. It is long because it records *why* things are
the way they are, not just what they are. You do not need to read it front to
back; the sections referenced below are the ones that matter for a given change.

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
argument (the design notes §10). Most of what a package manager wants is already in the
standard library; `compress/bzip2` and `crypto/ed25519` are both there.

---

## Changing the tool

**Read the eight iron rules first** (the design notes §1). They are short, and a change
that breaks one is a change that removes the reason this project exists. The one
that surprises people: **nothing from a package is ever executed** — there is no
install hook, no build step, no post-install anything, and there never will be.

Then, before you open the file you came here to edit:

- **§13** says which package owns what. The boundaries are real; a fix in the
  wrong package is usually a sign the problem is somewhere else.
- **§14** is a list of things that went wrong and what was done about them. If
  you are about to change extraction, symlinking, checksums, tag resolution or
  terminal output, the relevant subsection will save you a bad afternoon.

**Every non-trivial change wants a test that fails without it.** Comment the fix
out and run the test; if it still passes, the test is decoration. Several
subsections of §14 exist because a green suite proved nothing.

Comments here carry reasoning, not restatement. "This uses a lock" is noise;
"this is under the lock because two concurrent installs each read and rewrite the
whole file, and the later write erased the earlier pin" is the comment that stops
someone removing it.

---

## Adding something to the index

Most entries are **inline entries** (the design notes §5.1): we write the packaging
instructions for a repository that has no `toy.toml` of its own. That is how the
index can be useful before anybody has adopted us, and it is also the part with
real obligations attached.

```bash
make index-gen ARGS="--repos repos.txt --probe --out index/toy-index.json"
make index-check ARGS="--only owner/repo"
```

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
2. **If a maintainer asks to be delisted, delist them.** No conditions, no
   discussion. There is a form for it.
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
