#!/usr/bin/env bash
# 端到端测试：完全离线，不碰网络，不碰真实的 ~/.toy。
#
# 思路是造一个假的托管方：clone 模板指向本地 git 仓库（git ls-remote 认本地路径），
# raw 和 release 模板指向 file:// —— 于是整条 install 链路（ls-remote 拿 tag →
# 挑 semver → 取 toy.toml → 下载资产 → 验哈希 → 解压 → 软链）都能离线跑通。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export TOY_HOME="$WORK/toyhome"
export PATH="$TOY_HOME/bin:$PATH"
TOY="$WORK/toy"

pass=0; fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=$((fail+1)); }
check(){ if eval "$2" >/dev/null 2>&1; then ok "$1"; else bad "$1"; fi; }

echo "== 构建 =="
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy )
ok "toy 构建成功"

echo "== 造一个假的上游仓库 =="
UP="$WORK/upstream"
mkdir -p "$UP/dist"
cat > "$UP/toy.toml" <<'EOF'
name    = "hello"
desc    = "端到端测试用的样本工具"
license = "MIT"

[bin]
hello = "hello"

[artifact]
"linux/amd64"  = "hello_{version}_linux_amd64.tar.gz"
"linux/arm64"  = "hello_{version}_linux_arm64.tar.gz"
"darwin/amd64" = "hello_{version}_darwin_amd64.tar.gz"
"darwin/arm64" = "hello_{version}_darwin_arm64.tar.gz"
EOF

# 造 release 资产：一个 tar.gz，里面一个可执行文件
BUILD="$WORK/build"; mkdir -p "$BUILD"
printf '#!/bin/sh\necho "hello from toy e2e"\n' > "$BUILD/hello"
chmod +x "$BUILD/hello"
GOOS_="$(go env GOOS)"; GOARCH_="$(go env GOARCH)"
ASSET="hello_1.2.0_${GOOS_}_${GOARCH_}.tar.gz"
tar -C "$BUILD" -czf "$UP/dist/$ASSET" hello
ok "造出 release 资产 $ASSET"

( cd "$UP" && git init -q && git add -A \
  && git -c user.email=t@e.st -c user.name=t commit -q -m init \
  && git tag v1.0.0 && git tag v1.2.0 && git tag release-junk && git tag v2.0.0-rc.1 )
ok "上游仓库有 4 个 tag（含一个非 semver 和一个预发布）"

echo "== 配一个指向本地的托管方 =="
mkdir -p "$TOY_HOME"
cat > "$TOY_HOME/config.toml" <<EOF
[[index]]
name = "local"
url  = "file://$ROOT/index/toy-index.json"

[hosts.local]
raw     = "file://$UP/{path}"
clone   = "$UP"
release = "file://$UP/dist/{asset}"
web     = "file://$UP"
EOF
ok "写好 config.toml"

echo "== 装 =="
if "$TOY" install local:test/hello 2>&1 | tee "$WORK/install.log"; then
  ok "install 退出码 0"
else
  bad "install 失败"; cat "$WORK/install.log"
fi

check "选中了最高的正式版 v1.2.0（不是 v2.0.0-rc.1，也没被 release-junk 干扰）" \
      "grep -q '1\.2\.0' '$WORK/install.log'"
check "软链建在 \$TOY_HOME/bin 下"          "test -L '$TOY_HOME/bin/hello'"
check "装好的命令真的能跑"                    "hello | grep -q 'hello from toy e2e'"
check "写了安装清单"                          "ls '$TOY_HOME/installed'/*/*/*.toml >/dev/null 2>&1 || ls -R '$TOY_HOME/installed' | grep -q toml"
check "缓存文件名就是 sha256（内容寻址）"      "ls '$TOY_HOME/cache' | grep -qE '^[0-9a-f]{64}'"
check "打印了信任等级"                        "grep -qE '未签名|已签名|已钉住' '$WORK/install.log'"

echo "== list / info =="
check "list 列得出来"                         "'$TOY' list | grep -q hello"
check "info 跑得通"                           "'$TOY' info local:test/hello"

echo "== 铁律：不许遮蔽 PATH 上已有的命令 =="
cat > "$UP/toy.toml" <<'EOF'
name = "evil"
desc = "试图遮蔽 ls"
license = "MIT"
[bin]
ls = "hello"
[artifact]
"linux/amd64"  = "hello_{version}_linux_amd64.tar.gz"
"linux/arm64"  = "hello_{version}_linux_arm64.tar.gz"
"darwin/amd64" = "hello_{version}_darwin_amd64.tar.gz"
"darwin/arm64" = "hello_{version}_darwin_arm64.tar.gz"
EOF
( cd "$UP" && git add -A && git -c user.email=t@e.st -c user.name=t commit -q -m evil && git tag -f v1.2.0 -m x >/dev/null 2>&1 || true )
if "$TOY" install local:test/evil >"$WORK/evil.log" 2>&1; then
  bad "遮蔽 ls 的包竟然装上了（铁律 5 被破坏）"
else
  ok "拒绝了会遮蔽 ls 的包"
fi
check "拒绝的理由说得清楚"                    "grep -qE '遮蔽|shadow' '$WORK/evil.log'"

echo "== 铁律：sha256 对不上必须拒绝 =="
cat > "$UP/toy.toml" <<'EOF'
name = "badsum"
desc = "哈希故意写错"
license = "MIT"
[bin]
badsum = "hello"
[artifact]
"linux/amd64"  = "hello_{version}_linux_amd64.tar.gz"
"linux/arm64"  = "hello_{version}_linux_arm64.tar.gz"
"darwin/amd64" = "hello_{version}_darwin_amd64.tar.gz"
"darwin/arm64" = "hello_{version}_darwin_arm64.tar.gz"
[sha256]
"linux/amd64"  = "1111111111111111111111111111111111111111111111111111111111111111"
"linux/arm64"  = "1111111111111111111111111111111111111111111111111111111111111111"
"darwin/amd64" = "1111111111111111111111111111111111111111111111111111111111111111"
"darwin/arm64" = "1111111111111111111111111111111111111111111111111111111111111111"
EOF
( cd "$UP" && git add -A && git -c user.email=t@e.st -c user.name=t commit -q -m badsum )
if "$TOY" install local:test/badsum >"$WORK/bad.log" 2>&1; then
  bad "哈希对不上竟然装上了（铁律 4 被破坏）"
else
  ok "拒绝了哈希对不上的包"
fi

echo "== 卸载 =="
"$TOY" uninstall hello >/dev/null 2>&1 || true
check "软链没了"                              "! test -e '$TOY_HOME/bin/hello'"
check "缓存留着（内容寻址，同时是镜像目录）"    "ls '$TOY_HOME/cache' | grep -qE '^[0-9a-f]{64}'"

echo "== doctor / keygen / serve =="
check "doctor 跑得通"                         "'$TOY' doctor"
check "keygen 生成了密钥"                     "'$TOY' keygen && test -f '$TOY_HOME/keys/key.sec'"
check "私钥权限是 0600"                       "test \"\$(stat -c %a '$TOY_HOME/keys/key.sec' 2>/dev/null || stat -f %Lp '$TOY_HOME/keys/key.sec')\" = 600"
check "keygen 不覆盖已有密钥"                 "! '$TOY' keygen"

echo
printf '通过 %d，失败 %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
