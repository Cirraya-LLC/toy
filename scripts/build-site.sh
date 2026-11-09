#!/usr/bin/env bash
# Two jobs:
#   1. Copy index/toy-index.json to site/index.json (**the same bytes** — never hand-copy
#      a second version; the two would inevitably drift, and drift means the site lists
#      packages that cannot be installed).
#   2. Generate three static pages from site/index.template.html + site/i18n.json
#      (English at the site root, Simplified Chinese at /zh-Hans/, Traditional at /zh-Hant/).
#      **No hand-maintaining three HTML files** (they would drift), and **no pure
#      client-side switching** either: with JS disabled only one language would survive,
#      SEO would only ever see one, and this site's no-JS degradation is tested.
#
# Published by .github/workflows/pages.yml to GitHub Pages at https://toy.cirraya.com.
set -euo pipefail

cd "$(dirname "$0")/.."

src=index/toy-index.json
dst=site/index.json

[ -f "$src" ] || { echo "✗ cannot find $src"; exit 1; }

# Validate before copying: better to publish nothing than to publish an index that blows
# up in the client that downloads it.
read -r version packages < <(
  python3 - "$src" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
print(d.get("version"), len(d.get("packages") or []))
PY
)

[ "$version" = "1" ]  || { echo "✗ $src has version $version, not 1"; exit 1; }
[ "$packages" -gt 0 ] || { echo "✗ $src holds no packages at all"; exit 1; }

cp "$src" "$dst"
cmp -s "$src" "$dst" || { echo "✗ bytes differ after the copy"; exit 1; }

echo "✓ $dst — version $version, $packages entries, byte-identical to $src"

python3 - <<'PY'
import html, json, os, re, sys, tomllib

LANGS = ["en", "zh-Hans", "zh-Hant"]          # the first is the default, served at the site root
OUT   = {"en": ".", "zh-Hans": "zh-Hans", "zh-Hant": "zh-Hant"}
# hreflang wants fully qualified URLs; the language switcher and everything else stay
# relative, so a local preview still navigates locally.
SITE  = "https://toy.cirraya.com"
# Endonyms on purpose: a reader looking for their own language scans for the name written
# in that language, not for an English label they may not read.
NAME  = {"en": "English", "zh-Hans": "简体", "zh-Hant": "繁體"}
HREF  = {l: "/" if OUT[l] == "." else "/" + OUT[l] + "/" for l in LANGS}
# Two pages per language: the home page, and every package in the index at packages/.
PAGES = {"home": ("index.template.html", ""), "packages": ("packages.template.html", "packages/")}
TOP   = 12   # packages shown on the home page; the rest are one link away

i18n = json.load(open("site/i18n.json", encoding="utf-8"))
index = json.load(open("site/index.json", encoding="utf-8"))

# All three languages must be present — a missing string fails the whole build rather
# than shipping a page with holes in it.
for k, v in i18n.items():
    if k.startswith("_"):
        continue
    missing = [l for l in LANGS if not (isinstance(v, dict) and v.get(l))]
    if missing:
        sys.exit(f"✗ {k} in i18n.json is missing {missing}")

def esc(s):
    return html.escape(s, quote=True)

# {{>name}} pulls in site/partials/name.html, so both pages share one header and footer
# instead of two copies that drift.
def include(tpl):
    return re.sub(r"\{\{>(\w+)\}\}",
                  lambda m: open(f"site/partials/{m.group(1)}.html", encoding="utf-8").read(), tpl)

# ---- the package list, written into the pages at build time --------------------------
#
# These rows come from site/index.json, the very bytes the same build has just copied
# and checked, and both are published in the same deploy -- so the list cannot drift
# from the index, which is what kept it out of the HTML before (it used to be fetched
# at runtime). Written out, every package is on the page without JavaScript and for
# search engines too; app.js only filters and sorts what is already there.

UPSTREAM = {"github": "github.com", "gitlab": "gitlab.com", "codeberg": "codeberg.org"}

def upstream(source):
    m = re.fullmatch(r"([a-z]+):([^/\s]+/[^/\s]+)", source or "")
    if m and m.group(1) in UPSTREAM:
        return f"https://{UPSTREAM[m.group(1)]}/{m.group(2)}"
    return source if (source or "").startswith("https://") else ""

def facts(p):
    """command, platforms ("linux"/"macos", or None when only upstream's toy.toml knows), licence"""
    if not p.get("toyfile"):
        return p["id"].split("/")[-1], None, ""
    tf = tomllib.loads(p["toyfile"])
    names = list(tf.get("bin", {})) or list(tf.get("script", {}))
    oses = set()
    for key in tf.get("artifact", {}):
        oses.add({"linux": "linux", "darwin": "macos"}.get(key.split("/")[0], ""))
    if tf.get("script"):
        oses |= {"linux", "macos"}   # a single-file script is not tied to one system
    oses.discard("")
    return (names[0] if names else p["id"].split("/")[-1]), sorted(oses), tf.get("license", "")

PKGS = sorted(index["packages"], key=lambda p: (-(p.get("stars") or 0), p["id"].lower()))
FACTS = {p["id"]: facts(p) for p in PKGS}

def row(p, lang):
    T = lambda k: i18n[k][lang]
    cmd, oses, lic = FACTS[p["id"]]
    url = upstream(p.get("source", ""))
    ident = (f'<a class="pkg-id" href="{esc(url)}" rel="noopener">{esc(p["id"])}</a>' if url
             else f'<span class="pkg-id">{esc(p["id"])}</span>')
    if p.get("toyfile"):
        tag = f'<span class="tag-inline" title="{esc(T("js_inline_title"))}">{esc(T("js_inline_tag"))}</span>'
    else:
        tag = f'<span class="tag-own" title="{esc(T("pkgs_own_title"))}">{esc(T("pkgs_own_tag"))}</span>'
    stars = p.get("stars") or 0
    meta = ([f'<span class="os">{esc(T("pkgs_os_" + o))}</span>' for o in oses] if oses is not None
            else [f'<span class="os">{esc(T("pkgs_os_unknown"))}</span>'])
    if lic:
        meta.append(f'<span class="lic">{esc(lic)}</span>')
    line = "toy install " + p["id"]
    hay = " ".join([p["id"], cmd, p.get("desc", "")]).lower()
    os_attr = "?" if oses is None else " ".join(oses)
    return (f'<li class="pkg" data-hay="{esc(hay)}" data-os="{esc(os_attr)}" data-stars="{stars}" data-name="{esc(cmd.lower())}">'
            f'<div class="pkg-head"><span class="pkg-cmd">{esc(cmd)}</span>{ident}{tag}'
            f'<span class="pkg-stars">★ {stars:,}</span></div>'
            f'<p class="pkg-desc">{esc(p.get("desc", ""))}</p>'
            f'<div class="pkg-meta">{"".join(meta)}</div>'
            f'<div class="pkg-foot"><pre><code>{esc(line)}</code></pre>'
            f'<button class="copy" type="button" data-copy="{esc(line)}" aria-label="{esc(T("js_copy_aria") + line)}">'
            f'{esc(T("js_copy"))}</button></div></li>')

def rows(pkgs, lang, list_id=""):
    ident = f' id="{list_id}"' if list_id else ""
    return f'<ul class="pkgs"{ident}>' + "".join(row(p, lang) for p in pkgs) + "</ul>"

# ---- pages ----------------------------------------------------------------------------

n = len(PKGS)
for page, (tpl_name, sub) in PAGES.items():
    tpl = include(open(f"site/{tpl_name}", encoding="utf-8").read())
    for lang in LANGS:
        d = OUT[lang]
        depth = (0 if d == "." else 1) + (1 if sub else 0)
        a = "../" * depth                      # back to the site root, for style.css and app.js
        home = "../" if sub else "./"          # this language's home page
        pkgs = "./" if sub else "packages/"    # this language's package list

        # All three hreflangs point at each other, plus x-default for the default language
        # (the self-referential one has to be there too).
        alt = "\n".join(
            f'<link rel="alternate" hreflang="{l}" href="{SITE}{HREF[l]}{sub}">' for l in LANGS
        ) + f'\n<link rel="alternate" hreflang="x-default" href="{SITE}{HREF[LANGS[0]]}{sub}">'

        # The language switcher is three ordinary links — it works with JS disabled — and
        # stays on the same page in the other language.
        items = []
        for l in LANGS:
            on = ' class="is-on" aria-current="page"' if l == lang else ""
            items.append(f'<a href="{HREF[l]}{sub}" hreflang="{l}" lang="{l}"{on}>{NAME[l]}</a>')
        nav = ('<nav class="langnav" aria-label="%s">%s</nav>'
               % (esc(i18n["langnav_label"][lang]), "".join(items)))

        # The strings app.js needs: carried on <body data-i18n>, never hard-coded in the JS.
        js = {k[3:]: v[lang] for k, v in i18n.items() if k.startswith("js_")}

        raw = {"hreflang": alt, "langnav": nav,
               "pkgtop": rows(PKGS[:TOP], lang), "pkgrows": rows(PKGS, lang, "pkglist")}
        title, desc = (("title", "desc") if page == "home" else ("pkgs_title", "pkgs_desc"))
        txt = {"a": a, "home": home, "pkgs": pkgs,
               "jsdata": json.dumps(js, ensure_ascii=False, sort_keys=True),
               "page_title": i18n[title][lang].replace("{n}", str(n)),
               "page_desc": i18n[desc][lang].replace("{n}", str(n)),
               "pkg_all": i18n["pkg_all_link"][lang].replace("{n}", str(n)),
               "pkg_count": i18n["js_count_all"][lang].replace("{n}", str(n))}

        def sub_(m):
            k = m.group(1)
            if k in raw:
                return raw[k]
            if k in txt:
                return esc(txt[k])
            if k not in i18n:
                sys.exit(f"✗ {tpl_name} uses {{{{{k}}}}}, which i18n.json does not define")
            v = i18n[k][lang]
            # Only keys that genuinely carry HTML are inserted raw; everything else is escaped.
            return v.replace("{n}", str(n)) if k.endswith("_html") else esc(v.replace("{n}", str(n)))

        html_out = re.sub(r"\{\{(\w+)\}\}", sub_, tpl)
        dirpath = os.path.join("site", *(x for x in (d if d != "." else "", sub) if x))
        os.makedirs(dirpath, exist_ok=True)
        path = os.path.join(dirpath, "index.html")
        with open(path, "w", encoding="utf-8") as f:
            f.write(html_out)
        print(f"✓ {path} — lang={lang}, {page}")
PY
