# The website

A purely static site: **no npm, no framework, no font CDN, no third-party requests**.
The entire build step is one python3 heredoc (inside `scripts/build-site.sh`).

It does two things, and **the first one is a feature, not decoration**:

1. **Hosts the index** — `/index.json` is the address the client actually fetches
2. **Landing page + package browsing** — discovery is this product's differentiator,
   and that section reads the same `/index.json` at runtime

Three languages: `en` (site root) / `zh-Hans` (`/zh-Hans/`) / `zh-Hant` (`/zh-Hant/`).

Live at **https://toy.cirraya.com** (GitHub Pages).

---

## Deploying

`.github/workflows/pages.yml` does it on every push to `main` that touches `site/`, the
index, or the build script: it runs `scripts/build-site.sh`, drops the sources
(`README.md`, `index.template.html`, `i18n.json`) and publishes the rest to GitHub Pages.
The repository's Pages source is "GitHub Actions" and its custom domain is
`toy.cirraya.com`, a DNS `CNAME` to `cirraya-llc.github.io`. If that DNS sits behind
Cloudflare it must stay **DNS only**: proxied, GitHub cannot issue the certificate.

**Pushing to `main` deploys the index**, so push access to this repository is the
credential that decides what `toy search` finds. Treat it that way.

GitHub Pages serves `/index.json` as `application/json` with
`Access-Control-Allow-Origin: *` and `Cache-Control: max-age=600`, and none of that is
configurable there. Ten minutes is fine for an index.

The build script does two things:

1. **Copies** `index/toy-index.json` to `site/index.json` and validates that it's
   legal JSON, that `version` is 1, and that there's at least one entry.
   **It has to be a copy, never a hand-transcribed second version** — two copies
   will drift, and drift means **the web page advertises a package that won't install**.
   After copying, the script `cmp`s them byte for byte and fails the entire build on
   a mismatch: better not to publish at all than to publish an index that blows up
   when a client pulls it.
2. Generates three pages from `index.template.html` + `i18n.json` (next section).

**Everything generated is in `.gitignore`**: `site/index.json`, `site/index.html`,
`site/zh-Hans/`, `site/zh-Hant/`. What's maintained by hand in the repo is only
`index.template.html`, `i18n.json`, `style.css`, `app.js`, `404.html`.

---

## How the three languages work

```
site/index.template.html   one template, copy slots written as {{key}}
site/i18n.json             one content file, one key in three languages
        ↓  the python3 block in scripts/build-site.sh
site/index.html            en (default, site root)
site/zh-Hans/index.html
site/zh-Hant/index.html
```

**We do not hand-maintain three HTML files** — they will drift, and this site carries
three statements that must not break (see the last section); those are the first
things drift would take out. **We also don't do client-side switching** — with JS
disabled you'd be down to one language, search engines would only index one, and this
site's no-JS degradation is tested and worth keeping.

A few rules:

- **Values whose key ends in `_html` are inserted raw; everything else goes through
  `html.escape`.** The `<code>` / `<strong>` / `<a>` in those values is trusted
  content we wrote ourselves — but "treat every value as HTML" is a different thing
  entirely. The suffix makes every raw insertion point visible in the template.
- **A missing translation in any of the three fails the whole build.** We do not
  generate a page with holes in it.
- **The language switcher is three ordinary links** (`.langnav`), not a JS dropdown —
  it works with JS off.
- **All three pages carry complete `hreflang`** (including `x-default` pointing at
  English) plus their own self-referential entry. Those are the only absolute URLs
  (`https://toy.cirraya.com/...`, which hreflang requires); every link a reader follows
  is relative, so a local preview stays local.
- Subdirectory pages reference `../style.css` / `../app.js`; **the index is always an
  absolute `/index.json`** (which is what the client fetches too).
- **`app.js` hardcodes copy in no language.** The few strings it needs (the copy
  button, the "packaging instructions come from the index" marker, counts, error
  messages) are written into `<body data-i18n="…">` by the generator, and `app.js`
  `JSON.parse`s them once at startup. If it can't get them it falls back to the key
  itself — a button showing a key beats a blank button.

**Translation is not transliteration.** The English version is written for an
HN / Lobsters audience: sentences restructured, terminology in the idiom the English
world actually uses (supply chain, install hooks, content-addressed, trust on first
use). The Traditional Chinese version uses Taiwan/HK tech vocabulary
(軟體 / 檔案 / 執行 / 相依 / 雜湊 / 二進位檔 / 指令碼) — and **never uses 文件 to
mean "file"**.

**The left column of the trust table is the literal string the client prints, and it
exists in English only** (the CLI is English everywhere) — so those four `<code>`
lines are identical across all three pages, and all three say so underneath the
table. "Translating" them into Simplified or Traditional Chinese would be forging
client output on a web page. **If you change `Trust()` in `internal/ui`, come back
and sync these four lines**, or the site starts saying something toy doesn't say.

## The **one constant** to change after deploying

`internal/store/store.go`:

```go
const DefaultIndexURL = ""    // ← becomes "https://toy.cirraya.com/index.json"
```

Before that change, a new user has to run `toy index add` themselves; after it,
`toy search` works out of the box.

**Change it only once that URL really answers with the index** (DNS resolved,
certificate issued), then cut a release. Shipping an address that isn't live yet means every
new user sees a red cross the first time they run `toy doctor` — and that isn't
their fault. "No index configured" is a legitimate state: `doctor` reports it
neutrally rather than ✗, and `toy install github:owner/repo` never goes through the
index anyway.

## After the index changes

```bash
make index-gen ARGS="--repos repos.txt --probe --out index/toy-index.json"
make index-check          # installs, runs and uninstalls every entry for real — nothing that fails ships
git commit && git push    # Pages rebuilds automatically
```

GitHub Pages caches for ten minutes, so an index update is live within about that
long after the deploy finishes.

## Local preview

```bash
make site && cd site && python3 -m http.server 8099
# http://127.0.0.1:8099/  /zh-Hant/  /en/
```

Then use it as a real index, which is the one thing about this site worth verifying:

```bash
export TOY_HOME=$(mktemp -d)
toy index add local http://127.0.0.1:8099/index.json
toy search disk && toy install muesli/duf && duf --version
```

## Rules for changing copy

**Every piece of copy has to be backed by something in `SECURITY.md` or
`README.md`.** These three above all — breaking any one of them is worse than not
having the site at all:

- **No `curl | bash` install script. Ever.** That is precisely the thing this project
  exists to reject.
- **Every package must be marked as an inlined entry**. Dropping
  that marker from the site means we imply endorsement on the web page while the
  client honestly downgrades — that is us lying, and "we don't lie" is this product's
  most expensive asset.
- **`✓ signed` must spell out what it does not prove.** The signed message is
  `id + source + the raw toy.toml`, and not one byte of it binds the public key to
  the repo's owner.
