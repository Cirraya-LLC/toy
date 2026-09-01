#!/usr/bin/env bash
# 索引回归检查：把索引里的每一条**真的装一遍、跑一遍、卸一遍**。
#
# 为什么必须有这个：索引会收录别人的软件（§5.1 内联 toyfile），也就是说
# **我们替别人的打包方式做了担保**。上游哪天改了资产命名习惯（Rust 项目
# 从 fd-v10.2.0-x86_64-unknown-linux-gnu.tar.gz 换成别的），那条条目就烂了，
# 而用户会怪 toy。所以索引和代码一样要有回归测试。
#
# 每条都在自己那份一次性的 TOY_HOME 里装，装完就整个删掉 —— 不碰真实的 ~/.toy，
# 也不让上一条的缓存/钉住记录影响下一条。
#
# 用法（选项写在前面，和 toy 自己的 CLI 一个规矩）：
#   scripts/index-check.sh [--index 路径或URL] [--only id[,id...]] [--timeout 秒]
#                          [--config 文件]
#
# 退出码：全过 0，有失败 1，参数/环境有问题 2。
set -uo pipefail   # 故意不开 -e：一条失败不能中断，要跑完全部再汇总

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INDEX="$ROOT/index/toy-index.json"
ONLY=""
SEED_CONFIG=""   # 每份一次性 TOY_HOME 的 config.toml 模板，见 --config
TIMEOUT=300      # 单条安装的上限。下载真的可能很慢，但卡住不能拖垮整轮
SMOKE_TIMEOUT=15 # 单次试跑的上限

usage() {
  cat >&2 <<'EOF'
用法: scripts/index-check.sh [选项]

  --index <路径或URL>   要检查的索引，默认 index/toy-index.json
  --only  <id[,id...]>  只测这几条（PR 只改了两条时不该跑全量）
  --timeout <秒>        单条安装的上限，默认 300
  --config <文件>       先把这份 config.toml 放进每个一次性 TOY_HOME，再加索引。
                        用来测非默认托管方（自建 gitea 的 [hosts.*]），以及给这个
                        脚本自己做离线自测（假上游走 file://）
  -h, --help            这段话

选项必须写在参数前面。退出码：全过 0，有失败 1。
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --index)   [ $# -ge 2 ] || { usage; exit 2; }; INDEX="$2"; shift 2 ;;
    --only)    [ $# -ge 2 ] || { usage; exit 2; }; ONLY="$2"; shift 2 ;;
    --timeout) [ $# -ge 2 ] || { usage; exit 2; }; TIMEOUT="$2"; shift 2 ;;
    --config)  [ $# -ge 2 ] || { usage; exit 2; }; SEED_CONFIG="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "不认识的参数：$1" >&2; usage; exit 2 ;;
  esac
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# 颜色只在 TTY 上开（规格 §9：非 TTY 要降级成干净的行，好直接贴进 issue）。
if [ -t 2 ]; then C_G=$'\033[32m'; C_R=$'\033[31m'; C_Y=$'\033[33m'; C_0=$'\033[0m'
else C_G=''; C_R=''; C_Y=''; C_0=''; fi
say() { printf '%s\n' "$*" >&2; }
die() { printf '%s✗%s %s\n' "$C_R" "$C_0" "$*" >&2; exit 2; }

# tmo 是可移植的超时：macOS 自带的东西里没有 timeout(1)，而 CI 矩阵里有 macos。
# 超时返回 124（和 GNU timeout 一致），好和程序自己的退出码区分开。
tmo() {
  local secs="$1" out="$2"; shift 2
  local flag="$WORK/timedout"
  rm -f "$flag"
  "$@" >"$out" 2>&1 </dev/null &
  local pid=$!
  ( sleep "$secs"
    if kill -0 "$pid" 2>/dev/null; then
      : >"$flag"; kill -TERM "$pid" 2>/dev/null
      sleep 2; kill -KILL "$pid" 2>/dev/null
    fi ) >/dev/null 2>&1 &
  local watcher=$!
  local rc=0; wait "$pid" 2>/dev/null || rc=$?
  kill "$watcher" >/dev/null 2>&1
  wait "$watcher" >/dev/null 2>&1
  [ -e "$flag" ] && return 124
  return "$rc"
}

echo "== 构建 toy ==" >&2
TOY="$WORK/toy"
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy ) || die "toy 构建失败"

# 索引可以是 URL、file:// 或裸路径。一律先落成本地一份，再把**那一份**喂给 toy：
# 这样「枚举出来的条目」和「toy 真正装的条目」是同一批字节，不会被两次拉取的差异骗到。
LOCAL_INDEX="$WORK/index.json"
case "$INDEX" in
  http://*|https://*)
    command -v curl >/dev/null 2>&1 || die "索引是个 URL，但这台机器上没有 curl"
    curl -fsSL --max-time 60 "$INDEX" -o "$LOCAL_INDEX" || die "拉不到索引 $INDEX" ;;
  file://*)
    cp "${INDEX#file://}" "$LOCAL_INDEX" 2>/dev/null || die "读不了索引 $INDEX" ;;
  *)
    cp "$INDEX" "$LOCAL_INDEX" 2>/dev/null || die "读不了索引 $INDEX" ;;
esac
say "索引：$INDEX"
if [ -n "$SEED_CONFIG" ]; then
  [ -f "$SEED_CONFIG" ] || die "读不了 --config 指的 $SEED_CONFIG"
  say "每个 TOY_HOME 先放一份 $SEED_CONFIG"
fi

# 枚举条目。索引是 MarshalIndent 出来的，一个条目一个 { } 块，id / source 各占一行。
# 用 awk 而不是 jq：jq 不是两个 runner 都保证有的东西，而这点结构不值一个依赖。
# 按 { } 划块（而不是「id 的下一行就是 source」）是为了不依赖字段顺序。
awk '
  function qval(s) {
    sub(/^[^:]*:[[:space:]]*"/, "", s)
    sub(/",?[[:space:]]*$/, "", s)
    return s
  }
  /^[[:space:]]*\{/ { id=""; src=""; next }
  /^[[:space:]]*\}/ { if (id != "") print id "\t" src; id=""; src=""; next }
  /"id"[[:space:]]*:/     { id  = qval($0); next }
  /"source"[[:space:]]*:/ { src = qval($0); next }
' "$LOCAL_INDEX" > "$WORK/entries.tsv"

if [ ! -s "$WORK/entries.tsv" ]; then
  say "索引里一条包都没有 —— 没什么可检查的。"
  exit 0
fi

# --only：按 id 过滤（大小写不敏感，索引本身就是这么比 id 的）。
if [ -n "$ONLY" ]; then
  printf '%s\n' "$ONLY" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' \
    | grep -v '^$' | tr 'A-Z' 'a-z' | sort -u > "$WORK/only.txt"
  : > "$WORK/filtered.tsv"
  while IFS=$'\t' read -r id src; do
    low="$(printf '%s' "$id" | tr 'A-Z' 'a-z')"
    if grep -qxF "$low" "$WORK/only.txt"; then
      printf '%s\t%s\n' "$id" "$src" >> "$WORK/filtered.tsv"
    fi
  done < "$WORK/entries.tsv"
  # --only 点名了索引里没有的 id 要说一声：CI 里静默跳过等于"什么都没测还打了勾"。
  cut -f1 "$WORK/entries.tsv" | tr 'A-Z' 'a-z' | sort -u > "$WORK/have.txt"
  while read -r want; do
    grep -qxF "$want" "$WORK/have.txt" || say "${C_Y}!${C_0} --only 里的 $want 不在这份索引里，跳过"
  done < "$WORK/only.txt"
  mv "$WORK/filtered.tsv" "$WORK/entries.tsv"
  if [ ! -s "$WORK/entries.tsv" ]; then
    say "--only 没匹配到任何条目 —— 没什么可检查的。"
    exit 0
  fi
fi

TOTAL=$(wc -l < "$WORK/entries.tsv" | tr -d ' ')
PLATFORM="$(go env GOOS)/$(go env GOARCH)"
say "共 $TOTAL 条，平台 $PLATFORM"
say ""

# classify 把一次失败归到三类。issue 里最要紧的就是这一栏 —— 三种的处理完全不同：
#   条目 → 改索引里那条记录（资产名对不上、平台没覆盖、hash 写错、内联 toyfile 坏了）
#   网络 → 重试就行，别动索引
#   运行 → 装上了但跑不起来（上游那个 release 坏了，或 [bin] 右边的包内路径猜错了）
# 顺序有讲究：404 和 5xx 都会被 fetch 包进同一句「原始地址和所有镜像都拿不到」，
# 所以要先按**具体原因**分，最后才落到笼统的那条上。
classify() {
  local log="$1" code
  # 只认日志里**第一个** `HTTP <三位数>`：那是原始地址的真实状态码。后面出现的
  # 数字可能是服务器自己塞进 reason phrase 的 —— 一台上游回一句
  # `HTTP 500 HTTP 404 Not Found` 就能把一次抖动伪装成"资产名写错了"，然后我们
  # 去改一条其实没问题的条目（§5.1 第 3 条那个真的写出来过的洞的同一个形状）。
  code="$(grep -oE 'HTTP [0-9]{3}' "$log" 2>/dev/null | head -n1 | awk '{print $2}')"
  if   grep -q '没有适配当前平台的构建' "$log"; then
    printf '条目\t作者没发这个平台的构建，或条目的 [artifact] 平台 key 写错了\n'
  elif grep -q 'sha256 校验失败' "$log"; then
    printf '条目\t[sha256] 和实际下载到的字节对不上（上游重打了包？还是条目抄错了）\n'
  elif [ -n "$code" ]; then
    case "$code" in
      404|410) printf '条目\t下载 %s：资产名对不上，改条目的 [artifact]（上游多半换了命名习惯）\n' "$code" ;;
      401|403) printf '条目\t下载 %s：这个 release 要登录或已经私有了，收录不了\n' "$code" ;;
      *)       printf '网络\t上游回了 HTTP %s，这是抖动不是条目的错，重试\n' "$code" ;;
    esac
  elif grep -qE '这个仓库没有发布二进制|只能有一个|缺少 name|bin 目标路径不安全' "$log"; then
    printf '条目\t内联的 toyfile 本身有问题\n'
  elif grep -q '链接目标' "$log"; then
    printf '运行\t包里没有 [bin] 右边写的那个文件 —— 压缩包多半套了一层目录\n'
  elif grep -q '遮蔽 PATH' "$log"; then
    printf '条目\t[bin] 起的命令名和这台机器上已有的程序撞了，换个名字\n'
  elif grep -q '不认识这个托管方' "$log"; then
    printf '条目\tsource 的托管方没配（config.toml 的 [hosts.*]）\n'
  elif grep -q '无法解析这个包引用' "$log"; then
    printf '条目\tid 或 source 的格式坏了\n'
  elif grep -q '一个 tag 都没有' "$log"; then
    printf '条目\t上游一个 tag 都没有 —— toy 靠 tag 发现版本，这条收录不了\n'
  elif grep -qE '超时了|i/o timeout|context deadline|Client\.Timeout|connection refused|no such host|TLS handshake|Temporary failure|Could not resolve' "$log"; then
    printf '网络\t连不上/超时\n'
  elif grep -qE 'ls-remote .* 失败' "$log"; then
    printf '条目\t读不到上游的 tag（仓库改名了？删了？转私有了？）\n'
  else
    printf '网络\t说不清（原文见下）—— 先当抖动重试一次，还失败就按条目问题查\n'
  fi
}

# ran 判断一次执行有没有**真的跑起来**。退出码非 0 不算失败 —— 很多工具没参数就返回 1。
# 算失败的只有：崩溃信号、装载器说这文件跑不了（glibc 二进制落到 musl 机器上就是这个，
# 而且它报的是 `not found`，文件明明就在那儿 —— 见 §3.1）。
# 超时算跑起来了：它在跑，只是不打算结束。
ran() {
  local rc="$1" out="$2"
  [ "$rc" = 124 ] && return 0
  if [ "$rc" -ge 126 ] 2>/dev/null; then return 1; fi
  # 都锚在冒号后面：装载器/shell 报的就是 `sh: ./x: not found` 这个形状，
  # 而某个工具的 --help 里出现一句 "file not found" 不该被当成它跑不起来。
  if grep -qE ': (command )?not found|: cannot execute|: No such file or directory|Exec format error|bad interpreter|Bad CPU type|Segmentation fault' "$out"; then
    return 1
  fi
  return 0
}

ID=""; SRC=""; LOG="$WORK/install.log"; HOME_DIR=""
fail() { # $1=类别 $2=说明
  printf '%s✗%s %s  [%s]  %s\n' "$C_R" "$C_0" "$ID" "$1" "${SRC:-—}" >&2
  say "    $2"
  tail -n 12 "$LOG" 2>/dev/null | sed 's/^/      | /' >&2
  # 原文里那句「错误:」是 issue 里唯一说得出「是哪个 URL / 哪个文件」的东西。
  local raw
  # 不截断：这行常常很长，但「是哪个 URL / 哪个文件」全在里面。按字节截会把
  # 一个 UTF-8 汉字劈成半个，贴进 issue 就是一串乱码。
  raw="$(grep -m1 '^错误:' "$LOG" 2>/dev/null | tr -d '\t')"
  [ -n "$raw" ] || raw="$(tail -n 1 "$LOG" 2>/dev/null | tr -d '\t')"
  printf '%s\t%s\t%s\t%s\t%s\n' "$ID" "${SRC:-—}" "$1" "$2" "$raw" >> "$WORK/failures.txt"
  rm -rf "$HOME_DIR"
}

PASS=0
N=0
: > "$WORK/failures.txt"

while IFS=$'\t' read -r ID SRC; do
  [ -n "$ID" ] || continue
  N=$((N+1))
  HOME_DIR="$WORK/home-$N"
  mkdir -p "$HOME_DIR"
  export TOY_HOME="$HOME_DIR"
  # index add 会保留已有的 [hosts.*]，所以模板先放、索引后加。
  if [ -n "$SEED_CONFIG" ]; then cp "$SEED_CONFIG" "$HOME_DIR/config.toml"; fi

  # 索引指向本地那一份拷贝。裸路径 toy 自己会补成 file:///绝对路径。
  if ! "$TOY" index add check "$LOCAL_INDEX" >"$LOG" 2>&1 </dev/null; then
    fail "网络" "配索引就失败了（本地文件都读不了？）"; continue
  fi

  tmo "$TIMEOUT" "$LOG" "$TOY" install "$ID"
  rc=$?
  if [ "$rc" = 124 ]; then
    fail "网络" "安装超过 ${TIMEOUT}s 上限，已中止（下载太慢，或者卡在某一跳上）"; continue
  fi
  if [ "$rc" != 0 ]; then
    IFS=$'\t' read -r cls why < <(classify "$LOG")
    fail "$cls" "$why"; continue
  fi

  # 软链：这份 TOY_HOME 是全新的，所以 bin/ 里的每一个都来自刚装的这一条。
  links=()
  while IFS= read -r l; do links+=("$l"); done \
    < <(find "$HOME_DIR/bin" -mindepth 1 -maxdepth 1 2>/dev/null | sort)
  if [ "${#links[@]}" -eq 0 ]; then
    fail "条目" "装完了但 bin/ 是空的 —— toyfile 的 [bin]/[script] 没声明任何命令"; continue
  fi

  bad=""
  for l in "${links[@]}"; do
    name="$(basename "$l")"
    [ -L "$l" ] || { bad="$name 不是软链"; break; }
    tgt="$(readlink "$l")"
    case "$tgt" in /*) abs="$tgt" ;; *) abs="$(dirname "$l")/$tgt" ;; esac
    [ -f "$abs" ] || { bad="$name 指向的文件不存在（$tgt）—— [bin] 右边的包内路径多半猜错了"; break; }
    [ -x "$abs" ] || { bad="$name 指向的文件没有执行位（$tgt）"; break; }
  done
  if [ -n "$bad" ]; then fail "运行" "$bad"; continue; fi

  # 真的跑一下：--version → --help → 裸执行，第一个「跑起来了」就算过。
  bad=""
  for l in "${links[@]}"; do
    name="$(basename "$l")"; okrun=""
    for arg in --version --help ""; do
      if [ -n "$arg" ]; then tmo "$SMOKE_TIMEOUT" "$WORK/smoke.log" "$l" "$arg"
      else                   tmo "$SMOKE_TIMEOUT" "$WORK/smoke.log" "$l"; fi
      if ran "$?" "$WORK/smoke.log"; then okrun=1; break; fi
    done
    if [ -z "$okrun" ]; then
      bad="$name 跑不起来：$(head -n 1 "$WORK/smoke.log" | tr -d '\t')"
      cp "$WORK/smoke.log" "$LOG"; break
    fi
  done
  if [ -n "$bad" ]; then fail "运行" "$bad"; continue; fi

  # 卸载也顺带测一遍：没有清单就没法 uninstall / update（规格 §6），
  # 而那种坏法只有真装过一次才看得见。
  pkgname="$(grep -h -m1 '^name[[:space:]]*=' "$HOME_DIR"/installed/*.toml 2>/dev/null \
             | sed 's/.*=[[:space:]]*"\(.*\)"/\1/')"
  if [ -z "$pkgname" ]; then
    : >"$LOG"; fail "运行" "装完了却没写安装清单，uninstall/update 都会瞎"; continue
  fi
  if ! "$TOY" uninstall "$pkgname" >"$LOG" 2>&1 </dev/null; then
    fail "运行" "uninstall $pkgname 失败"; continue
  fi
  if [ -n "$(find "$HOME_DIR/bin" -mindepth 1 2>/dev/null)" ]; then
    : >"$LOG"; fail "运行" "uninstall 之后 bin/ 里还有残留软链"; continue
  fi

  printf '%s✓%s %s  (%s)\n' "$C_G" "$C_0" "$ID" "$(printf '%s ' "${links[@]##*/}" | sed 's/ $//')" >&2
  PASS=$((PASS+1))
  rm -rf "$HOME_DIR"
done < "$WORK/entries.tsv"

FAILN=$(( TOTAL - PASS ))
say ""
say "════ $PASS 通过 / $FAILN 失败（共 $TOTAL）· $PLATFORM ════"
[ "$FAILN" -eq 0 ] && exit 0

say ""
say "失败明细（可以直接贴进 issue）："
say ""
while IFS=$'\t' read -r id src cls why raw; do
  say "- **$id** — \`$src\` · 平台 $PLATFORM"
  say "  - **$cls** — $why"
  [ -n "${raw:-}" ] && say "  - \`$raw\`"
done < "$WORK/failures.txt"
say ""
say "三种类别的处理不一样：**条目** = 改 index/toy-index.json 那一条；"
say "**网络** = 重试，别动索引；**运行** = 上游那个 release 坏了，或 [bin] 的包内路径猜错了。"
exit 1
