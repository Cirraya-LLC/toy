#!/usr/bin/env bash
# 把 index/toy-index.json 复制成 site/index.json（**同一份字节**，不许手抄一份 ——
# 两份必然漂移，而漂移意味着网页上写着有的包装不上）。
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
