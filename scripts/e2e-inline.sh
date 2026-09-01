#!/usr/bin/env bash
# 索引内联 toyfile 的六条规则，端到端跑真的 toy 二进制。
#
# 为什么单独一个脚本、而且非要起一个真的 HTTP 服务器：这六条里最要紧的第 3 条
# （上游 500 / 超时绝不回落到内联）区分的是「明确不存在」和「这次没问到」，
# 而 file:// 那条路上只有"文件在不在"，造不出 404 / 500 / 连不上这三种形状。
# 曾经有一版把判据写成 strings.Contains(err, "HTTP 404")，于是一台上游回一句
# `HTTP/1.1 500 HTTP 404 Not Found` 就能把一次服务端故障认成"作者没写 toy.toml"，
# 悄悄把用户从作者的声明切到索引的声明 —— 下面那条 500-poison 用例就是钉它的。
#
# 需要 python3（只用标准库）。没有就跳过，不算失败。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! command -v python3 >/dev/null 2>&1; then
  echo "跳过：没有 python3（这个脚本要起一个能造任意状态行的 HTTP 服务器）"
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

# 一个只回指定状态行的服务器。用裸 socket 是因为 http.server 只写它自己那套
# 标准原因短语，写不出 "500 HTTP 404 Not Found" 这种要试的东西。
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

# 上游仓库只用来出 tag；toy.toml 和资产都由上面那个服务器发。
UP="$W/upstream"; mkdir -p "$UP"
( cd "$UP" && git init -q \
  && git -c user.email=t@e.st -c user.name=t commit -q --allow-empty -m init \
  && git tag v1.2.0 )

B="$W/build"; mkdir -p "$B"
printf '#!/bin/sh\necho INLINE-OK\n' > "$B/hello"; chmod +x "$B/hello"
tar -C "$B" -czf "$SRV/dist/hello_1.2.0_${G}_${A}.tar.gz" hello

# 上游作者自己那份（服务器按 MODE 决定给不给），命令名 upstreamcmd
cat > "$SRV/raw/toy.toml" <<EOF
name = "hello"
desc = "上游作者自己写的"
[bin]
upstreamcmd = "hello"
[artifact]
"${G}/${A}" = "hello_{version}_${G}_${A}.tar.gz"
EOF

# 索引内联那份，命令名 inlinecmd —— 两个名字不同，好一眼看出装的是哪一份
INLINE=$(printf 'name = "hello"\ndesc = "索引内联的"\n[bin]\ninlinecmd = "hello"\n[artifact]\n"%s/%s" = "hello_{version}_%s_%s.tar.gz"\n' "$G" "$A" "$G" "$A")

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

echo "== 规则 1：上游有 toy.toml 时，内联那份看都不看 =="
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >"$W/r1.log" 2>&1
chk "装出来的是上游那份的命令名 upstreamcmd"   "test -L '$TOY_HOME/bin/upstreamcmd'"
chk "内联那份的命令名没有出现"                 "! test -e '$TOY_HOME/bin/inlinecmd'"
chk "没有降档"                                 "! grep -q '打包说明来自索引' '$W/r1.log'"

echo "== 规则 2：上游 404 且有内联 → 用内联，降档并说清哪一半是谁的 =="
reset; mode 404; idx "$INLINE"
"$TOY" install alice/hello >"$W/r2.log" 2>&1
chk "装出来的是内联那份的命令名 inlinecmd"     "test -L '$TOY_HOME/bin/inlinecmd'"
chk "装好的命令真的能跑"                       "'$TOY_HOME/bin/inlinecmd' | grep -q INLINE-OK"
chk "清单里的信任等级是 index-toyfile"          "grep -q 'index-toyfile' \"\$(manifest)\""
chk "说了「打包说明来自索引」"                  "grep -q '打包说明来自索引' '$W/r2.log'"
chk "说清了字节仍来自作者的 release"             "grep -q '作者' '$W/r2.log'"

echo "== 规则 3：上游 500 / 伪造原因短语 / 连不上，都绝不回落到内联 =="
for m in 500 500-poison; do
  reset; mode "$m"; idx "$INLINE"
  "$TOY" install alice/hello >"$W/r3-$m.log" 2>&1
  chk "$m 时整次安装失败"       "test $? -ne 0 || ! test -e '$TOY_HOME/bin/inlinecmd'"
  chk "$m 时什么都没装上"       "! test -e '$TOY_HOME/bin/inlinecmd'"
done
reset; idx "$INLINE"; kill $SRVPID 2>/dev/null; sleep 0.5
"$TOY" install alice/hello >"$W/r3-down.log" 2>&1
chk "服务器连不上时什么都没装上" "! test -e '$TOY_HOME/bin/inlinecmd'"
start_server

echo "== 规则 4：内联那份走一样严的校验 =="
reset; mode 404
idx "$(printf 'name = "hello"\ndesc = "d"\n[bin]\nx = "/etc/passwd"\n[artifact]\n"%s/%s" = "a.tar.gz"\n' "$G" "$A")"
"$TOY" install alice/hello >"$W/r4.log" 2>&1
chk "[bin] 指向绝对路径的内联条目被拒" "! test -e '$TOY_HOME/bin/x'"

# §14.1 那条 critical 换了一个投递方式：恶意的 [bin] 名字现在能从索引条目来，
# 不必控制上游仓库。保留名单必须照样挡住，而且 --force 也不放行。
reset; mode 404
idx "$(printf 'name = "hello"\ndesc = "d"\n[bin]\ngit = "hello"\n[artifact]\n"%s/%s" = "hello_{version}_%s_%s.tar.gz"\n' "$G" "$A" "$G" "$A")"
"$TOY" install alice/hello >"$W/r4-git.log" 2>&1
chk "内联条目声明 [bin] git 被拒（§14.1）"     "! test -e '$TOY_HOME/bin/git'"
"$TOY" install --force alice/hello >>"$W/r4-git.log" 2>&1
chk "这一条 --force 也不放行"                  "! test -e '$TOY_HOME/bin/git'"

echo "== 规则 5：内联 + key + sig 齐全也不许显示成「✓ 已签名」 =="
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
  chk "publish --inline 产出的条目 sig 和 toyfile 都在" "true"
  chk "没有打出「✓ 已签名」的勾"        "! grep -q '✓ 已签名' '$W/r5.log'"
  chk "仍然是「打包说明来自索引」这一档"  "grep -q '打包说明来自索引' '$W/r5.log'"
  chk "清单里的信任等级不是 signed"       "! grep -q 'trust = \"signed\"' \"\$(manifest)\""
else
  bad "publish --inline 没产出条目"; cat "$W/pub.err"
fi

echo "== 规则 6：host → index-toyfile 算降档，要停下来要 --force =="
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >/dev/null 2>&1
chk "先按上游那份装上"                  "test -L '$TOY_HOME/bin/upstreamcmd'"
mode 404
"$TOY" install --reinstall alice/hello >"$W/r6a.log" 2>&1
chk "同版重装时降档被拦下"              "! test -e '$TOY_HOME/bin/inlinecmd'"
chk "拦下来的理由说的是信任等级降了"      "grep -q '信任等级比上一次低' '$W/r6a.log'"
chk "被拦时上游那份还在"                "test -L '$TOY_HOME/bin/upstreamcmd'"
"$TOY" install --reinstall --force alice/hello >"$W/r6b.log" 2>&1
chk "--force 才放行"                    "test -L '$TOY_HOME/bin/inlinecmd'"
chk "--force 逐条说明了放行了什么"        "grep -q '放行' '$W/r6b.log'"

# 升级到新版本这条路（不带 --reinstall，最常见的真实场景）
reset; mode ok; idx "$INLINE"
"$TOY" install alice/hello >/dev/null 2>&1
tar -C "$B" -czf "$SRV/dist/hello_1.3.0_${G}_${A}.tar.gz" hello
( cd "$UP" && git tag v1.3.0 )
mode 404
"$TOY" install alice/hello >"$W/r6c.log" 2>&1
chk "升新版时上游 toy.toml 没了 → 被拦下" "! test -e '$TOY_HOME/bin/inlinecmd'"
"$TOY" install --force alice/hello >"$W/r6d.log" 2>&1
chk "--force 放行升级并装到 1.3.0"        "test -L '$TOY_HOME/bin/inlinecmd' && grep -q '1.3.0' \"\$(manifest)\""

# 反方向：作者后来自己加了 toy.toml —— 不拦，但要说一句
mode ok
tar -C "$B" -czf "$SRV/dist/hello_1.4.0_${G}_${A}.tar.gz" hello
( cd "$UP" && git tag v1.4.0 )
"$TOY" install alice/hello >"$W/r6e.log" 2>&1
chk "上游后来有了自己的 toy.toml：不拦"    "test -L '$TOY_HOME/bin/upstreamcmd'"
chk "并且主动说明打包说明不再由索引提供"    "grep -q '不再由索引提供' '$W/r6e.log'"

echo
echo "通过 $pass，失败 $fail"
[ $fail -eq 0 ]
