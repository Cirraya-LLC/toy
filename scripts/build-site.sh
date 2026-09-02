#!/usr/bin/env bash
# 两件事：
#   1. 把 index/toy-index.json 复制成 site/index.json（**同一份字节**，不许手抄一份 ——
#      两份必然漂移，而漂移意味着网页上写着有的包装不上）。
#   2. 用 site/index.template.html + site/i18n.json 生成三份静态页（简体 / 繁体 / 英文）。
#      **不手维护三份 HTML**（必然漂移），**也不做纯客户端切换**（禁 JS 就只剩一种语言，
#      SEO 也只收得到一份，而这个站的无 JS 降级是验过的）。
#
# Cloudflare Pages：构建命令 `bash scripts/build-site.sh`，输出目录 `site`。
set -euo pipefail

cd "$(dirname "$0")/.."

src=index/toy-index.json
dst=site/index.json

[ -f "$src" ] || { echo "✗ 找不到 $src"; exit 1; }

# 校验放在复制之前：宁可不发布，也不要发布一份客户端拉下来会炸的索引。
read -r version packages < <(
  python3 - "$src" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
print(d.get("version"), len(d.get("packages") or []))
PY
)

[ "$version" = "1" ]  || { echo "✗ $src 的 version 是 $version，不是 1"; exit 1; }
[ "$packages" -gt 0 ] || { echo "✗ $src 里一条包都没有"; exit 1; }

cp "$src" "$dst"
cmp -s "$src" "$dst" || { echo "✗ 复制之后字节对不上"; exit 1; }

echo "✓ $dst —— version $version，$packages 条，和 $src 逐字节一致"

python3 - <<'PY'
import html, json, os, re, sys

LANGS = ["zh-Hans", "zh-Hant", "en"]          # 第一个是默认语言，落在站根
OUT   = {"zh-Hans": ".", "zh-Hant": "zh-Hant", "en": "en"}
NAME  = {"zh-Hans": "简体", "zh-Hant": "繁體", "en": "English"}
HREF  = {l: "/" if OUT[l] == "." else "/" + OUT[l] + "/" for l in LANGS}

i18n = json.load(open("site/i18n.json", encoding="utf-8"))
tpl  = open("site/index.template.html", encoding="utf-8").read()

# 三语必须齐 —— 缺一条就整个构建失败，而不是生成一份漏字的页面。
for k, v in i18n.items():
    if k.startswith("_"):
        continue
    missing = [l for l in LANGS if not (isinstance(v, dict) and v.get(l))]
    if missing:
        sys.exit(f"✗ i18n.json 的 {k} 缺 {missing}")

def esc(s):
    return html.escape(s, quote=True)

for lang in LANGS:
    d = OUT[lang]
    a = "" if d == "." else "../"

    # hreflang 三份互相指认，含 x-default 指向默认语言（自指那条也要有）。
    alt = "\n".join(
        f'<link rel="alternate" hreflang="{l}" href="{HREF[l]}">' for l in LANGS
    ) + f'\n<link rel="alternate" hreflang="x-default" href="{HREF[LANGS[0]]}">'

    # 语言切换器就是三个普通链接 —— 禁 JS 也能切。
    items = []
    for l in LANGS:
        on = ' class="is-on" aria-current="page"' if l == lang else ""
        items.append(f'<a href="{HREF[l]}" hreflang="{l}" lang="{l}"{on}>{NAME[l]}</a>')
    nav = ('<nav class="langnav" aria-label="%s">%s</nav>'
           % (esc(i18n["langnav_label"][lang]), "".join(items)))

    # app.js 要的那几条文案：挂在 <body data-i18n> 上，不在 JS 里硬编码。
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
            sys.exit(f"✗ 模板里的 {{{{{k}}}}} 在 i18n.json 里没有")
        v = i18n[k][lang]
        # 只有确实需要 HTML 的 key 才原样插入，其余一律转义。
        return v if k.endswith("_html") else esc(v)

    page = re.sub(r"\{\{(\w+)\}\}", sub, tpl)

    dirpath = os.path.join("site", d) if d != "." else "site"
    os.makedirs(dirpath, exist_ok=True)
    path = os.path.join(dirpath, "index.html")
    with open(path, "w", encoding="utf-8") as f:
        f.write(page)
    print(f"✓ {path} —— lang={lang}")
PY
