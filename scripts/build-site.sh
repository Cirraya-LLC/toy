#!/usr/bin/env bash
# Two jobs:
#   1. Copy index/toy-index.json to site/index.json (**the same bytes** — never hand-copy
#      a second version; the two would inevitably drift, and drift means the site lists
#      packages that cannot be installed).
#   2. Generate three static pages from site/index.template.html + site/i18n.json
#      (Simplified Chinese / Traditional Chinese / English).
#      **No hand-maintaining three HTML files** (they would drift), and **no pure
#      client-side switching** either: with JS disabled only one language would survive,
#      SEO would only ever see one, and this site's no-JS degradation is tested.
#
# Cloudflare Pages: build command `bash scripts/build-site.sh`, output directory `site`.
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
import html, json, os, re, sys

LANGS = ["zh-Hans", "zh-Hant", "en"]          # the first is the default, served at the site root
OUT   = {"zh-Hans": ".", "zh-Hant": "zh-Hant", "en": "en"}
# Endonyms on purpose: a reader looking for their own language scans for the name written
# in that language, not for an English label they may not read.
NAME  = {"zh-Hans": "简体", "zh-Hant": "繁體", "en": "English"}
HREF  = {l: "/" if OUT[l] == "." else "/" + OUT[l] + "/" for l in LANGS}

i18n = json.load(open("site/i18n.json", encoding="utf-8"))
tpl  = open("site/index.template.html", encoding="utf-8").read()

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

for lang in LANGS:
    d = OUT[lang]
    a = "" if d == "." else "../"

    # All three hreflangs point at each other, plus x-default for the default language
    # (the self-referential one has to be there too).
    alt = "\n".join(
        f'<link rel="alternate" hreflang="{l}" href="{HREF[l]}">' for l in LANGS
    ) + f'\n<link rel="alternate" hreflang="x-default" href="{HREF[LANGS[0]]}">'

    # The language switcher is three ordinary links — it works with JS disabled.
    items = []
    for l in LANGS:
        on = ' class="is-on" aria-current="page"' if l == lang else ""
        items.append(f'<a href="{HREF[l]}" hreflang="{l}" lang="{l}"{on}>{NAME[l]}</a>')
    nav = ('<nav class="langnav" aria-label="%s">%s</nav>'
           % (esc(i18n["langnav_label"][lang]), "".join(items)))

    # The strings app.js needs: carried on <body data-i18n>, never hard-coded in the JS.
    js = {k[3:]: v[lang] for k, v in i18n.items() if k.startswith("js_")}

    raw = {"hreflang": alt, "langnav": nav}
    txt = {"a": a, "jsdata": json.dumps(js, ensure_ascii=False, sort_keys=True)}

    def sub(m):
        k = m.group(1)
        if k in raw:
            return raw[k]
        if k in txt:
            return esc(txt[k])
        if k not in i18n:
            sys.exit(f"✗ the template uses {{{{{k}}}}}, which i18n.json does not define")
        v = i18n[k][lang]
        # Only keys that genuinely carry HTML are inserted raw; everything else is escaped.
        return v if k.endswith("_html") else esc(v)

    page = re.sub(r"\{\{(\w+)\}\}", sub, tpl)

    dirpath = os.path.join("site", d) if d != "." else "site"
    os.makedirs(dirpath, exist_ok=True)
    path = os.path.join(dirpath, "index.html")
    with open(path, "w", encoding="utf-8") as f:
        f.write(page)
    print(f"✓ {path} — lang={lang}")
PY
