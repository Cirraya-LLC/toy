#!/usr/bin/env bash
# HEAD every download URL in the index, for every platform each entry declares.
#
# index-check.sh is the stronger check -- it installs, runs and removes -- but it
# can only do that for the platform it is running on. This one covers every
# platform an entry claims, which is where a whole class of breakage lives: an
# entry generated and probed on linux can point at a darwin asset that does not
# exist, and nothing notices until a mac user tries it. That is exactly how the
# mikefarah/yq entry was broken on 5 of its 6 platforms.
#
# A 200 only says the bytes are there. It says nothing about whether they run, or
# whether the path on the right of [bin] is right inside that archive.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INDEX="$ROOT/index/toy-index.json"
ONLY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --index) INDEX="$2"; shift 2 ;;
    --only)  ONLY="$2";  shift 2 ;;
    -h|--help) echo "usage: index-urls.sh [--index PATH] [--only id[,id...]]"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -f "$INDEX" ] || { echo "no index at $INDEX" >&2; exit 2; }

TOY="$(mktemp -d)/toy"
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy ) || exit 2
HOME_DIR="$(mktemp -d)"
trap 'rm -rf "$HOME_DIR" "$(dirname "$TOY")"' EXIT
export TOY_HOME="$HOME_DIR"
# Only the index under test: toy comes configured with the published one, which would
# otherwise answer first for every entry already live (same as index-check.sh).
"$TOY" index rm default >/dev/null 2>&1 || true
"$TOY" index add local "file://$INDEX" >/dev/null 2>&1

TOY="$TOY" INDEX="$INDEX" ONLY="$ONLY" python3 - <<'PY'
import json, os, re, subprocess, sys

toy, index, only = os.environ["TOY"], os.environ["INDEX"], os.environ["ONLY"]
wanted = {s.strip().lower() for s in only.split(",") if s.strip()}
pkgs = json.load(open(index))["packages"]
if wanted:
    pkgs = [p for p in pkgs if p["id"].lower() in wanted]

ok = bad = 0
no_platform = []
fails = []
for p in pkgs:
    tf = p.get("toyfile", "")
    tmpl = {}
    for line in tf.splitlines():
        m = re.match(r'"([a-z0-9]+/[a-z0-9]+(?:/[a-z0-9]+)?)"\s*=\s*"([^"]+)"', line.strip())
        if m:
            tmpl[m.group(1)] = m.group(2)
    if not tmpl:
        # A pointer entry: the toyfile lives upstream and this check cannot see it.
        no_platform.append(p["id"])
        continue
    r = subprocess.run([toy, "info", "--json", p["id"]], capture_output=True, text=True)
    try:
        info = json.loads(r.stdout)
    except Exception:
        fails.append((p["id"], "-", "could not resolve a version"))
        bad += 1
        continue
    tag, ver = info.get("tag", ""), info.get("version", "")
    owner, repo = p["source"].split(":", 1)[1].split("/", 1)

    def check(tag, ver):
        out = []
        for plat, t in sorted(tmpl.items()):
            asset = t.replace("{version}", ver).replace("{tag}", tag)
            url = f"https://github.com/{owner}/{repo}/releases/download/{tag}/{asset}"
            code = subprocess.run(
                ["curl", "-sSIL", "--max-time", "30", "-o", "/dev/null", "-w", "%{http_code}", url],
                capture_output=True, text=True).stdout.strip()
            out.append((plat, code, asset))
        return out

    results = check(tag, ver)
    # The client's fallback, mirrored: when nothing the newest tag should have is there,
    # it installs the release the forge marks as latest (insPerformOrPrevious). Check
    # that release instead, or every repo that tags a line it never releases fails here
    # while installing fine.
    if results and all(code == "404" for _, code, _ in results):
        loc = subprocess.run(
            ["curl", "-s", "--max-time", "30", "-o", "/dev/null", "-w", "%{redirect_url}",
             f"https://github.com/{owner}/{repo}/releases/latest"],
            capture_output=True, text=True).stdout.strip()
        latest = loc.rstrip("/").rsplit("/", 1)[-1] if "/releases/tag/" in loc else ""
        if latest and latest != tag:
            r2 = subprocess.run([toy, "info", "--json", f'{p["id"]}@{latest}'], capture_output=True, text=True)
            try:
                v2 = json.loads(r2.stdout).get("version", "")
            except Exception:
                v2 = ""
            if v2:
                print(f"  {p['id']}: {tag} has no files; checking {latest}, the release GitHub marks as latest")
                results = check(latest, v2)
    for plat, code, asset in results:
        if code == "200":
            ok += 1
        else:
            bad += 1
            fails.append((p["id"], plat, f"HTTP {code}  {asset}"))

print(f"\n════ {ok} URLs returned 200 / {bad} did not ({len(pkgs)} entries) ════")
if no_platform:
    print(f"({len(no_platform)} entries carry no inline toyfile, so their assets cannot be checked here)")
for pid, plat, why in fails:
    print(f"  ✗ {pid}  {plat}  {why}")
sys.exit(1 if bad else 0)
PY
