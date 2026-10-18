#!/usr/bin/env bash
# Index regression check: for every entry in the index, **actually install it, run it,
# and uninstall it**.
#
# Why this has to exist: the index carries other people's software (§5.1, inlined
# toyfiles) — which means **we have vouched for someone else's packaging**. The day
# upstream changes its asset naming habit (a Rust project going from
# fd-v10.2.0-x86_64-unknown-linux-gnu.tar.gz to something else) that entry rots, and
# users blame toy. So the index needs regression tests exactly like the code does.
#
# Each entry installs into its own throwaway TOY_HOME, deleted afterwards — the real
# ~/.toy is never touched, and one entry's cache or pins can never influence the next.
#
# Usage (options come first, same rule as toy's own CLI):
#   scripts/index-check.sh [--index path-or-URL] [--only id[,id...]] [--timeout secs]
#                          [--config file] [--results file]
#
# Exit codes: 0 all passed, 1 some failed, 2 bad arguments or environment.
set -uo pipefail   # deliberately no -e: one failure must not abort the run; report all

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INDEX="$ROOT/index/toy-index.json"
ONLY=""
SEED_CONFIG=""   # config.toml template seeded into each throwaway TOY_HOME; see --config
RESULTS=""       # --results: one "id<TAB>pass|fail|skip<TAB>detail" line per entry, for scripts
TIMEOUT=300      # per-entry install ceiling. Downloads really can be slow, but a hang
                 # must not drag down the whole run
SMOKE_TIMEOUT=15 # ceiling for a single smoke run

usage() {
  cat >&2 <<'EOF'
Usage: scripts/index-check.sh [options]

  --index <path|URL>    index to check, default index/toy-index.json
  --only  <id[,id...]>  test only these (a PR touching two entries shouldn't run all)
  --timeout <secs>      per-entry install ceiling, default 300
  --config <file>       seed this config.toml into every throwaway TOY_HOME before the
                        index is added. Use it to test non-default hosts ([hosts.*] for
                        a self-hosted gitea), and to let this script self-test offline
                        (a fake upstream over file://)
  --results <file>      also write one line per entry, id<TAB>pass|fail|skip<TAB>detail,
                        for a script to read (the discovery workflow keeps only passes)
  -h, --help            this text

Options must come before arguments. Exit codes: 0 all passed, 1 some failed.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --index)   [ $# -ge 2 ] || { usage; exit 2; }; INDEX="$2"; shift 2 ;;
    --only)    [ $# -ge 2 ] || { usage; exit 2; }; ONLY="$2"; shift 2 ;;
    --timeout) [ $# -ge 2 ] || { usage; exit 2; }; TIMEOUT="$2"; shift 2 ;;
    --config)  [ $# -ge 2 ] || { usage; exit 2; }; SEED_CONFIG="$2"; shift 2 ;;
    --results) [ $# -ge 2 ] || { usage; exit 2; }; RESULTS="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Color only on a TTY (spec §9: off a TTY, degrade to clean lines that paste into an issue).
if [ -t 2 ]; then C_G=$'\033[32m'; C_R=$'\033[31m'; C_Y=$'\033[33m'; C_0=$'\033[0m'
else C_G=''; C_R=''; C_Y=''; C_0=''; fi
say() { printf '%s\n' "$*" >&2; }
die() { printf '%s✗%s %s\n' "$C_R" "$C_0" "$*" >&2; exit 2; }
if [ -n "$RESULTS" ]; then : > "$RESULTS" || die "cannot write --results file $RESULTS"; fi

# tmo is a portable timeout: macOS ships no timeout(1), and the CI matrix includes macos.
# A timeout returns 124 (same as GNU timeout) so it stays distinguishable from the
# program's own exit code.
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

echo "== building toy ==" >&2
TOY="$WORK/toy"
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy ) || die "failed to build toy"

# The index may be a URL, file://, or a bare path. Always materialize one local copy
# first and feed **that copy** to toy: this way "the entries we enumerated" and "the
# entries toy actually installs" are the same bytes, so two fetches can't disagree.
LOCAL_INDEX="$WORK/index.json"
case "$INDEX" in
  http://*|https://*)
    command -v curl >/dev/null 2>&1 || die "the index is a URL, but this machine has no curl"
    curl -fsSL --max-time 60 "$INDEX" -o "$LOCAL_INDEX" || die "cannot fetch index $INDEX" ;;
  file://*)
    cp "${INDEX#file://}" "$LOCAL_INDEX" 2>/dev/null || die "cannot read index $INDEX" ;;
  *)
    cp "$INDEX" "$LOCAL_INDEX" 2>/dev/null || die "cannot read index $INDEX" ;;
esac
say "index: $INDEX"
if [ -n "$SEED_CONFIG" ]; then
  [ -f "$SEED_CONFIG" ] || die "cannot read the --config file $SEED_CONFIG"
  say "seeding $SEED_CONFIG into every TOY_HOME"
fi

# Enumerate the entries. The index is written with MarshalIndent, so each entry is one
# { } block with id / source each on their own line.
# awk rather than jq: jq isn't guaranteed on both runners, and this much structure isn't
# worth a dependency. Splitting on { } (rather than "source is the line after id") keeps
# it independent of field order.
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
  say "the index holds no packages — nothing to check."
  exit 0
fi

# --only: filter by id (case-insensitive, which is how the index itself compares ids).
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
  # If --only names an id this index doesn't have, say so: silently skipping it in CI
  # means "tested nothing and still went green".
  cut -f1 "$WORK/entries.tsv" | tr 'A-Z' 'a-z' | sort -u > "$WORK/have.txt"
  while read -r want; do
    grep -qxF "$want" "$WORK/have.txt" || say "${C_Y}!${C_0} --only names $want, which is not in this index; skipping"
  done < "$WORK/only.txt"
  mv "$WORK/filtered.tsv" "$WORK/entries.tsv"
  if [ ! -s "$WORK/entries.tsv" ]; then
    say "--only matched no entries — nothing to check."
    exit 0
  fi
fi

TOTAL=$(wc -l < "$WORK/entries.tsv" | tr -d ' ')
PLATFORM="$(go env GOOS)/$(go env GOARCH)"
say "$TOTAL entries, platform $PLATFORM"
say ""

# classify sorts a failure into one of three buckets. In an issue that column matters
# most, because the three are handled in completely different ways:
#   entry   -> fix that record in the index (wrong asset name, missing platform,
#              wrong hash, broken inline toyfile)
#   network -> just retry, leave the index alone
#   runtime -> it installed but won't run (upstream's release is broken, or the in-package
#              path on the right of [bin] was guessed wrong)
# The order matters: 404 and 5xx both get wrapped by fetch into the same "original URL
# and every mirror failed" sentence, so classify by **specific cause** first and fall
# through to the generic one last.
classify() {
  local log="$1" code
  # Take only the **first** `HTTP <3 digits>` in the log: that is the real status of the
  # original URL. Later numbers may be ones the server itself stuffed into a reason
  # phrase — an upstream answering `HTTP 500 HTTP 404 Not Found` could disguise a blip
  # as "wrong asset name", and we would go edit an entry that was fine all along (same
  # shape as the §5.1 rule-3 hole that really did get written once).
  code="$(grep -oE 'HTTP [0-9]{3}' "$log" 2>/dev/null | head -n1 | awk '{print $2}')"
  if   grep -q 'toyfile: no build for this platform' "$log"; then
    # Not a failure. Plenty of good tools ship Linux binaries only -- btop, eza and
    # procs all do, and their entries are perfectly correct; they are simply not
    # installable on a Mac. Calling that a defect means this check can never be green
    # on macOS, and a check that cannot be green stops being read.
    printf 'skip\tthe author ships no build for %s, so there is nothing to install here\n' "$(uname -s | tr A-Z a-z)/$(uname -m)"
  elif grep -q 'sha256 checksum mismatch' "$log"; then
    printf 'entry\t[sha256] disagrees with the bytes we downloaded (upstream repacked the release? or the entry was copied wrong)\n'
  elif [ -n "$code" ]; then
    case "$code" in
      404|410) printf 'entry\tdownload %s: the asset name no longer matches; fix the entry'"'"'s [artifact] (upstream most likely changed its naming habit)\n' "$code" ;;
      401|403) printf 'entry\tdownload %s: this release needs a login or has gone private; it cannot be carried\n' "$code" ;;
      *)       printf 'network\tupstream answered HTTP %s; that is a blip, not the entry'"'"'s fault. Retry\n' "$code" ;;
    esac
  elif grep -q 'toyfile: ' "$log"; then
    printf 'entry\tthe inlined toyfile itself is broken\n'
  elif grep -q 'store: link target' "$log"; then
    printf 'runtime\tthe package has no file at the path on the right of [bin] — the archive most likely wraps everything in one directory\n'
  elif grep -q 'shadow' "$log"; then
    printf 'entry\tthe command name from [bin] collides with a program already on this machine; pick another\n'
  elif grep -q 'host: unknown host' "$log"; then
    printf 'entry\tthe host in source is not configured ([hosts.*] in config.toml)\n'
  elif grep -q 'cannot parse this package reference' "$log"; then
    printf 'entry\tthe id or source is malformed\n'
  elif grep -qE 'has no tags at all|is a semver version|no stable release' "$log"; then
    printf 'entry\tupstream has no usable semver tag (none at all, or none shaped like v1.2.3) — toy discovers versions from tags, so this one cannot be carried\n'
  elif grep -qE 'timed out|i/o timeout|context deadline|Client\.Timeout|connection refused|no such host|TLS handshake|Temporary failure|Could not resolve' "$log"; then
    printf 'network\tunreachable or timed out\n'
  elif grep -qE 'ls-remote .* failed' "$log"; then
    printf 'entry\tcannot read upstream tags (repo renamed? deleted? gone private?)\n'
  else
    printf 'network\tunclear (raw output below) — treat as a blip and retry once; if it fails again, chase it as an entry problem\n'
  fi
}

# ran decides whether an execution **actually started**. A non-zero exit code is not a
# failure — plenty of tools return 1 when given no arguments. Only these count as failure:
# a crash signal, or the loader saying it cannot run this file (a glibc binary landing on
# a musl box does exactly that, and it reports `not found` even though the file is right
# there — see §3.1).
# A timeout counts as started: it IS running, it just does not intend to stop.
ran() {
  local rc="$1" out="$2"
  [ "$rc" = 124 ] && return 0
  if [ "$rc" -ge 126 ] 2>/dev/null; then return 1; fi
  # All anchored after a colon: the loader/shell reports exactly the `sh: ./x: not found`
  # shape, while a "file not found" line inside some tool's --help must not be read as
  # that tool failing to start.
  if grep -qE ': (command )?not found|: cannot execute|: No such file or directory|Exec format error|bad interpreter|Bad CPU type|Segmentation fault' "$out"; then
    return 1
  fi
  return 0
}

SKIPN=0
# release_state tells "upstream is still publishing" from "upstream renamed the file",
# for a tag whose release file was missing. toy falls back to the release before it in
# that case, so the install succeeds either way -- which would hide a rename from this
# check forever. Asking is fine here (this is our maintenance job, not the client) and
# happens only on that fallback.
#
# A draft, a missing release, or one published in the last six hours: still
# publishing (some projects publish first and upload after). Published longer ago and
# still without the file: renamed. Only GitHub can be asked; elsewhere it is assumed to
# be still publishing, and a rename there shows up as the same note day after day.
release_state() { # $1=source $2=tag -> prints pending|renamed
  case "$1" in github:*) ;; *) echo pending; return ;; esac
  local url="https://api.github.com/repos/${1#github:}/releases/tags/$2" json
  # Two spellings rather than an optional array: macOS's bash 3.2 treats an empty
  # "${arr[@]}" as unbound under set -u.
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    json=$(curl -fsS --max-time 20 -H "Authorization: Bearer $GITHUB_TOKEN" "$url" 2>/dev/null) || json=""
  else
    json=$(curl -fsS --max-time 20 "$url" 2>/dev/null) || json=""
  fi
  printf '%s' "$json" | python3 -c '
import datetime, json, sys
try:
    r = json.load(sys.stdin)
except Exception:
    print("pending"); sys.exit()
p = r.get("published_at")
if r.get("draft") or not p:
    print("pending"); sys.exit()
age = datetime.datetime.now(datetime.timezone.utc) - datetime.datetime.fromisoformat(p.replace("Z", "+00:00"))
print("renamed" if age.total_seconds() > 6 * 3600 else "pending")' 2>/dev/null || echo pending
}

ID=""; SRC=""; LOG="$WORK/install.log"; HOME_DIR=""
# result appends one machine-readable line for --results. Tabs and newlines in the
# detail would break the format, so they become spaces.
result() { # $1=pass|fail|skip $2=detail
  [ -n "$RESULTS" ] || return 0
  printf '%s\t%s\t%s\n' "$ID" "$1" "$(printf '%s' "$2" | tr '\t\n' '  ')" >> "$RESULTS"
}

fail() { # $1=category $2=explanation
  printf '%s✗%s %s  [%s]  %s\n' "$C_R" "$C_0" "$ID" "$1" "${SRC:-—}" >&2
  say "    $2"
  tail -n 12 "$LOG" 2>/dev/null | sed 's/^/      | /' >&2
  # The raw `error:` line is the only thing in an issue that can name **which URL** or
  # **which file**.
  local raw
  # Not truncated: this line is often long, but "which URL / which file" is all inside
  # it. Cutting by bytes would slice a multi-byte UTF-8 character in half and paste into
  # the issue as mojibake.
  raw="$(grep -m1 '^error:' "$LOG" 2>/dev/null | tr -d '\t')"
  [ -n "$raw" ] || raw="$(tail -n 1 "$LOG" 2>/dev/null | tr -d '\t')"
  printf '%s\t%s\t%s\t%s\t%s\n' "$ID" "${SRC:-—}" "$1" "$2" "$raw" >> "$WORK/failures.txt"
  result fail "$1: $2"
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
  # index add preserves any existing [hosts.*], so seed the template first and add the
  # index second.
  if [ -n "$SEED_CONFIG" ]; then cp "$SEED_CONFIG" "$HOME_DIR/config.toml"; fi

  # Only the index under test. toy comes configured with the published one, and it is
  # looked up first: without this, every entry already live was installed from the
  # published copy rather than from the file being checked -- a PR changing an existing
  # entry was checked against the old version (fastfetch, 2026-10-18).
  "$TOY" index rm default >/dev/null 2>&1 </dev/null || true
  # Point the index at that local copy. toy expands a bare path into file:///absolute.
  if ! "$TOY" index add check "$LOCAL_INDEX" >"$LOG" 2>&1 </dev/null; then
    fail "network" "even configuring the index failed (cannot read a local file?)"; continue
  fi

  tmo "$TIMEOUT" "$LOG" "$TOY" install "$ID"
  rc=$?
  # The PATH shadow check (iron rule 5) fires when THIS MACHINE already carries a
  # command of the same name, and a CI runner ships jq, yq, shellcheck and kind
  # preinstalled. That is toy working, and it says nothing about the entry -- the
  # old advice here, "pick another [bin] name", would have meant renaming jq's
  # command. Install into the throwaway TOY_HOME anyway and carry on: every later
  # step addresses the binary by its full path under that home, so the system copy
  # is never what gets run.
  if [ "$rc" != 0 ] && [ "$rc" != 124 ] && grep -q 'shadow' "$LOG"; then
    shadowed=1
    tmo "$TIMEOUT" "$LOG" "$TOY" install --force "$ID"
    rc=$?
  else
    shadowed=0
  fi
  if [ "$rc" = 124 ]; then
    fail "network" "install exceeded the ${TIMEOUT}s ceiling and was aborted (download too slow, or stuck on one hop)"; continue
  fi
  if [ "$rc" != 0 ]; then
    IFS=$'\t' read -r cls why < <(classify "$LOG")
    if [ "$cls" = skip ]; then
      SKIPN=$(( SKIPN + 1 ))
      result skip "$why"
      printf '%s-%s %s  %s\n' "$C_Y" "$C_0" "$ID" "$why" >&2
      continue
    fi
    fail "$cls" "$why"; continue
  fi

  # toy fell back to the release before the newest tag (its file was missing). Find
  # out whether that tag is still being published or its file was renamed.
  pending=""
  if grep -q ' is tagged, but its release file' "$LOG"; then
    newtag=$(sed -n 's/^warning: [^ ]* \([^ ]*\) is tagged, but its release file.*/\1/p' "$LOG" | head -n 1)
    if [ "$(release_state "$SRC" "$newtag")" = renamed ]; then
      fail "entry" "the newest release $newtag has been published for hours, but has no file the entry names — upstream renamed its assets; regenerate this entry"; continue
    fi
    pending="  [$newtag is tagged but its files are not up yet; checked the release before it]"
  fi

  # Symlinks: this TOY_HOME is brand new, so everything in bin/ came from the entry we
  # just installed.
  links=()
  while IFS= read -r l; do links+=("$l"); done \
    < <(find "$HOME_DIR/bin" -mindepth 1 -maxdepth 1 2>/dev/null | sort)
  if [ "${#links[@]}" -eq 0 ]; then
    fail "entry" "installed, but bin/ is empty — the toyfile's [bin]/[script] declares no command"; continue
  fi

  bad=""
  for l in "${links[@]}"; do
    name="$(basename "$l")"
    [ -L "$l" ] || { bad="$name is not a symlink"; break; }
    tgt="$(readlink "$l")"
    case "$tgt" in /*) abs="$tgt" ;; *) abs="$(dirname "$l")/$tgt" ;; esac
    [ -f "$abs" ] || { bad="$name points at a file that does not exist ($tgt) — the in-package path on the right of [bin] was most likely guessed wrong"; break; }
    [ -x "$abs" ] || { bad="$name points at a file with no execute bit ($tgt)"; break; }
  done
  if [ -n "$bad" ]; then fail "runtime" "$bad"; continue; fi

  # Actually run it: --version, then --help, then bare. The first one that starts counts.
  bad=""
  for l in "${links[@]}"; do
    name="$(basename "$l")"; okrun=""
    for arg in --version --help ""; do
      if [ -n "$arg" ]; then tmo "$SMOKE_TIMEOUT" "$WORK/smoke.log" "$l" "$arg"
      else                   tmo "$SMOKE_TIMEOUT" "$WORK/smoke.log" "$l"; fi
      if ran "$?" "$WORK/smoke.log"; then okrun=1; break; fi
    done
    if [ -z "$okrun" ]; then
      bad="$name will not run: $(head -n 1 "$WORK/smoke.log" | tr -d '\t')"
      cp "$WORK/smoke.log" "$LOG"; break
    fi
  done
  if [ -n "$bad" ]; then fail "runtime" "$bad"; continue; fi

  # Test uninstall too: without a manifest there is no uninstall and no update (spec §6),
  # and that failure mode is only visible once something has really been installed.
  pkgname="$(grep -h -m1 '^name[[:space:]]*=' "$HOME_DIR"/installed/*.toml 2>/dev/null \
             | sed 's/.*=[[:space:]]*"\(.*\)"/\1/')"
  if [ -z "$pkgname" ]; then
    : >"$LOG"; fail "runtime" "installed but wrote no manifest; uninstall and update would both be blind"; continue
  fi
  if ! "$TOY" uninstall "$pkgname" >"$LOG" 2>&1 </dev/null; then
    fail "runtime" "uninstall $pkgname failed"; continue
  fi
  if [ -n "$(find "$HOME_DIR/bin" -mindepth 1 2>/dev/null)" ]; then
    : >"$LOG"; fail "runtime" "symlinks left behind in bin/ after uninstall"; continue
  fi

  note="$pending"
  [ "$shadowed" = 1 ] && note="$note  [this machine already had that command; installed with --force]"
  printf '%s✓%s %s  (%s)%s\n' "$C_G" "$C_0" "$ID" "$(printf '%s ' "${links[@]##*/}" | sed 's/ $//')" "$note" >&2
  PASS=$((PASS+1))
  result pass "$(printf '%s ' "${links[@]##*/}" | sed 's/ $//')"
  rm -rf "$HOME_DIR"
done < "$WORK/entries.tsv"

FAILN=$(( TOTAL - PASS - SKIPN ))
say ""
if [ "$SKIPN" -gt 0 ]; then
  say "════ $PASS passed / $FAILN failed / $SKIPN not built for this platform (of $TOTAL) · $PLATFORM ════"
else
  say "════ $PASS passed / $FAILN failed (of $TOTAL) · $PLATFORM ════"
fi
[ "$FAILN" -eq 0 ] && exit 0

say ""
say "Failure details (paste straight into an issue):"
say ""
while IFS=$'\t' read -r id src cls why raw; do
  say "- **$id** — \`$src\` · platform $PLATFORM"
  say "  - **$cls** — $why"
  [ -n "${raw:-}" ] && say "  - \`$raw\`"
done < "$WORK/failures.txt"
say ""
say "The three categories are handled differently: **entry** = fix that record in"
say "index/toy-index.json; **network** = retry, leave the index alone; **runtime** ="
say "upstream's release is broken, or the in-package path in [bin] was guessed wrong."
exit 1
