#!/usr/bin/env bash
# The six rules for index-inlined toyfiles, end to end against the real toy binary.
#
# Why this needs its own script, and why it insists on a real HTTP server: the most
# important of the six is rule 3 (an upstream 500 or timeout must NEVER fall back to the
# inline copy), and that rule turns on the difference between "definitively absent" and
# "we failed to ask". Over file:// there is only "does the file exist", so 404 / 500 /
# connection-refused cannot be told apart at all.
# One version of this judged by strings.Contains(err, "HTTP 404") — so an upstream
# answering `HTTP/1.1 500 HTTP 404 Not Found` could pass a server-side outage off as
# "the author never wrote a toy.toml", quietly switching the user from the author's
# declaration to the index's. The 500-poison case below is what nails that shut.
#
# Needs python3 (stdlib only). Without it the script skips; that is not a failure.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! command -v python3 >/dev/null 2>&1; then
  echo "skipped: no python3 (this script needs an HTTP server that can emit arbitrary status lines)"
  exit 0
fi

W="$(mktemp -d)"
trap 'rm -rf "$W"; kill $SRVPID 2>/dev/null' EXIT
SRV="$W/srv"; mkdir -p "$SRV/raw" "$SRV/dist"
export TOY_HOME="$W/home"
TOY="$W/toy"
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy ) || exit 1

pass=0; fail=0
ok(){  printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad(){ printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail+1)); }
chk(){ if eval "$2" >/dev/null 2>&1; then ok "$1"; else bad "$1"; fi; }
mode(){ echo "$1" > "$SRV/MODE"; }

# A server that emits exactly the status line it is told to. Raw sockets, because
# http.server only ever writes its own canonical reason phrases — it cannot produce
# something like "500 HTTP 404 Not Found", which is precisely what has to be tested.
cat > "$W/server.py" <<'PY'
import http.server, socketserver, sys, os, time
ROOT, PORT = sys.argv[1], int(sys.argv[2])
def mode():
    try: return open(os.path.join(ROOT, "MODE")).read().strip()
    except Exception: return "ok"
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def raw(self, status, body=b""):
        self.wfile.write(("HTTP/1.1 %s\r\nContent-Length: %d\r\n\r\n" % (status, len(body))).encode())
        self.wfile.write(body)
    def do_GET(self):
        p = self.path.split("?")[0]
        if p.endswith("/toy.toml"):
            m = mode()
            if m == "404":        return self.raw("404 Not Found")
            if m == "500":        return self.raw("500 Internal Server Error")
            if m == "500-poison": return self.raw("500 HTTP 404 Not Found")
        fp = os.path.join(ROOT, p.lstrip("/"))
        if os.path.isfile(fp): return self.raw("200 OK", open(fp, "rb").read())
        return self.raw("404 Not Found")
class S(socketserver.ThreadingTCPServer):
    allow_reuse_address = True; daemon_threads = True
S(("127.0.0.1", PORT), H).serve_forever()
PY

PORT=$(python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()")
G="$(go env GOOS)"; A="$(go env GOARCH)"

# The upstream repo exists only to carry tags; toy.toml and the assets come from the
# server above.
UP="$W/upstream"; mkdir -p "$UP"
( cd "$UP" && git init -q \
  && git -c user.email=t@e.st -c user.name=t commit -q --allow-empty -m init \
  && git tag v1.2.0 )

B="$W/build"; mkdir -p "$B"
printf '#!/bin/sh\necho INLINE-OK\n' > "$B/hello"; chmod +x "$B/hello"
tar -C "$B" -czf "$SRV/dist/hello_1.2.0_${G}_${A}.tar.gz" hello

# The author's own copy (the server hands it over or not, per MODE). Command: upstreamcmd.
cat > "$SRV/raw/toy.toml" <<EOF
name = "hello"
desc = "written by the upstream author"
[bin]
upstreamcmd = "hello"
[artifact]
"${G}/${A}" = "hello_{version}_${G}_${A}.tar.gz"
EOF

# The index-inlined copy, command inlinecmd — different names so it is obvious at a
# glance which of the two got installed.
INLINE=$(printf 'name = "hello"\ndesc = "inlined by the index"\n[bin]\ninlinecmd = "hello"\n[artifact]\n"%s/%s" = "hello_{version}_%s_%s.tar.gz"\n' "$G" "$A" "$G" "$A")

writecfg(){ mkdir -p "$TOY_HOME"; cat > "$TOY_HOME/config.toml" <<EOF
[[index]]
name = "local"
url  = "file://$W/idx.json"

[hosts.fake]
raw     = "http://127.0.0.1:$PORT/raw/{path}"
clone   = "$UP"
release = "http://127.0.0.1:$PORT/dist/{asset}"
web     = "http://127.0.0.1:$PORT/{owner}/{repo}"
EOF
}
idx(){ python3 -c "
import json,sys
e={'id':'alice/hello','source':'fake:alice/hello','desc':'d'}
if sys.argv[1]: e['toyfile']=sys.argv[1]
json.dump({'version':1,'packages':[e]},open('$W/idx.json','w'),ensure_ascii=False)
" "$1"; }
reset(){ rm -rf "$TOY_HOME"; writecfg; }
manifest(){ ls "$TOY_HOME"/installed/*.toml 2>/dev/null | head -1; }

start_server(){
  python3 "$W/server.py" "$SRV" "$PORT" & SRVPID=$!
  for _ in $(seq 1 40); do (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && break; sleep 0.2; done
}
start_server

echo "== rule 1: upstream has a toy.toml, so the inline copy is never even looked at =="
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >"$W/r1.log" 2>&1
chk "installed the upstream command name upstreamcmd" "test -L '$TOY_HOME/bin/upstreamcmd'"
chk "the inline command name never appeared"          "! test -e '$TOY_HOME/bin/inlinecmd'"
chk "no trust downgrade"                              "! grep -q 'packaging instructions come from the index' '$W/r1.log'"

echo "== rule 2: upstream 404 with an inline copy -> use it, downgrade, and say which half is whose =="
reset; mode 404; idx "$INLINE"
"$TOY" install alice/hello >"$W/r2.log" 2>&1
chk "installed the inline command name inlinecmd"     "test -L '$TOY_HOME/bin/inlinecmd'"
chk "the installed command actually runs"             "'$TOY_HOME/bin/inlinecmd' | grep -q INLINE-OK"
chk "the manifest records trust level index-toyfile"  "grep -q 'index-toyfile' \"\$(manifest)\""
chk "said the packaging instructions come from the index" "grep -q 'packaging instructions come from the index' '$W/r2.log'"
chk "made clear the bytes still come from the author's release" "grep -q \"the bytes are from the author's own release\" '$W/r2.log'"

echo "== rule 3: upstream 500, a poisoned reason phrase, or unreachable -> NEVER fall back =="
for m in 500 500-poison; do
  reset; mode "$m"; idx "$INLINE"
  "$TOY" install alice/hello >"$W/r3-$m.log" 2>&1
  chk "$m: the install fails outright" "test $? -ne 0 || ! test -e '$TOY_HOME/bin/inlinecmd'"
  chk "$m: nothing got installed"      "! test -e '$TOY_HOME/bin/inlinecmd'"
done
reset; idx "$INLINE"; kill $SRVPID 2>/dev/null; sleep 0.5
"$TOY" install alice/hello >"$W/r3-down.log" 2>&1
chk "server unreachable: nothing got installed" "! test -e '$TOY_HOME/bin/inlinecmd'"
start_server

echo "== rule 4: the inline copy is validated just as strictly =="
reset; mode 404
idx "$(printf 'name = "hello"\ndesc = "d"\n[bin]\nx = "/etc/passwd"\n[artifact]\n"%s/%s" = "a.tar.gz"\n' "$G" "$A")"
"$TOY" install alice/hello >"$W/r4.log" 2>&1
chk "an inline entry whose [bin] points at an absolute path is refused" "! test -e '$TOY_HOME/bin/x'"

# The §14.1 critical now has a second delivery route: a malicious [bin] name can come
# from an index entry, no control of the upstream repo required. The reserved-name list
# must still stop it — and --force must NOT waive this one.
reset; mode 404
idx "$(printf 'name = "hello"\ndesc = "d"\n[bin]\ngit = "hello"\n[artifact]\n"%s/%s" = "hello_{version}_%s_%s.tar.gz"\n' "$G" "$A" "$G" "$A")"
"$TOY" install alice/hello >"$W/r4-git.log" 2>&1
chk "an inline entry declaring [bin] git is refused (§14.1)" "! test -e '$TOY_HOME/bin/git'"
"$TOY" install --force alice/hello >>"$W/r4-git.log" 2>&1
chk "--force does not waive this one"                        "! test -e '$TOY_HOME/bin/git'"

echo "== rule 5: inline + key + sig must still never render as '✓ signed' =="
reset; mode 404
"$TOY" keygen >/dev/null 2>&1
mkdir -p "$W/pub"; printf '%s' "$INLINE" > "$W/pub/toy.toml"
if "$TOY" publish --source fake:alice/hello --inline "$W/pub" >"$W/pub.json" 2>"$W/pub.err" && [ -s "$W/pub.json" ]; then
  python3 -c "
import json
e=json.load(open('$W/pub.json'))
if isinstance(e,list): e=e[0]
e['id']='alice/hello'; e['source']='fake:alice/hello'
assert 'sig' in e and 'toyfile' in e, e
json.dump({'version':1,'packages':[e]},open('$W/idx.json','w'),ensure_ascii=False)"
  "$TOY" install alice/hello >"$W/r5.log" 2>&1
  chk "publish --inline produced an entry carrying both sig and toyfile" "true"
  chk "no '✓ signed' checkmark was printed"        "! grep -q '✓ signed' '$W/r5.log'"
  chk "still the 'packaging instructions come from the index' tier" "grep -q 'packaging instructions come from the index' '$W/r5.log'"
  chk "the manifest trust level is not signed"     "! grep -q 'trust = \"signed\"' \"\$(manifest)\""
else
  bad "publish --inline produced no entry"; cat "$W/pub.err"
fi

echo "== rule 6: host -> index-toyfile is a downgrade; stop and demand --force =="
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >/dev/null 2>&1
chk "install from the upstream copy first"        "test -L '$TOY_HOME/bin/upstreamcmd'"
mode 404
"$TOY" install --reinstall alice/hello >"$W/r6a.log" 2>&1
chk "the downgrade is blocked on a same-version reinstall" "! test -e '$TOY_HOME/bin/inlinecmd'"
chk "the reason given is the drop in trust level"  "grep -q 'trusts less than the last one did' '$W/r6a.log'"
chk "the upstream install survives being blocked"  "test -L '$TOY_HOME/bin/upstreamcmd'"
"$TOY" install --reinstall --force alice/hello >"$W/r6b.log" 2>&1
chk "--force lets it through"                      "test -L '$TOY_HOME/bin/inlinecmd'"
chk "--force lists item by item what it waived"    "grep -q -- '--force waived:' '$W/r6b.log'"

# The upgrade path (no --reinstall) — the common real-world scenario.
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >/dev/null 2>&1
tar -C "$B" -czf "$SRV/dist/hello_1.3.0_${G}_${A}.tar.gz" hello
( cd "$UP" && git tag v1.3.0 )
mode 404
"$TOY" install alice/hello >"$W/r6c.log" 2>&1
chk "upstream toy.toml vanished on upgrade -> blocked" "! test -e '$TOY_HOME/bin/inlinecmd'"
"$TOY" install --force alice/hello >"$W/r6d.log" 2>&1
chk "--force allows the upgrade and lands on 1.3.0"    "test -L '$TOY_HOME/bin/inlinecmd' && grep -q '1.3.0' \"\$(manifest)\""

# The other direction: the author later adds a toy.toml. Not blocked, but say so.
mode ok
tar -C "$B" -czf "$SRV/dist/hello_1.4.0_${G}_${A}.tar.gz" hello
( cd "$UP" && git tag v1.4.0 )
"$TOY" install alice/hello >"$W/r6e.log" 2>&1
chk "upstream now has its own toy.toml: not blocked"   "test -L '$TOY_HOME/bin/upstreamcmd'"
chk "and it volunteers that the index no longer supplies the packaging" "grep -q 'no longer come from the index' '$W/r6e.log'"

echo
echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
