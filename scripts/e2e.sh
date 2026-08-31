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

echo "== 全新的 store：一个索引都没有 =="
# 内置索引现在是空的（store.DefaultIndexURL）。没有索引是合法状态，不是故障：
# search 要说清楚怎么加，doctor 不许为这件事打 ✗。用一个单独的 TOY_HOME 测，
# 免得被下面那份写死的 config.toml 影响。
FRESH="$WORK/fresh"
check "全新 store 的 search 说「还没配索引」而不是「没有匹配」" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q '还没配任何索引'"
check "search 给出了 toy index add 的示例（含 file://）" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'toy index add' && \
       TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'file://'"
check "search 提醒 github: 那条路不需要索引" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'toy install github:owner/repo'"
check "写出来的默认 config.toml 里没有内置索引" \
      "! grep -q '^\[\[index\]\]' '$FRESH/config.toml'"
# doctor 仍会因为 PATH 没配而失败，所以只断言「没配索引」这一条不是 ✗。
# doctor 的退出码本来就是 1（PATH 没配），而 set -o pipefail 会让整条管道跟着失败 ——
# 那不是这里要断言的东西，所以先落成文件再 grep。
TOY_HOME="$FRESH" "$TOY" doctor >"$WORK/doctor-fresh.log" 2>&1 || true
check "doctor 对「没配索引」不打 ✗" \
      "grep '没有配索引' '$WORK/doctor-fresh.log' | grep -q '✓'"
check "doctor 里没有任何一行 ✗ 是在说索引" \
      "! grep '^✗' '$WORK/doctor-fresh.log' | grep -q 索引"

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
# 上面那个 toy.toml 只写了两段 key（绝大多数项目都是这样）。linux 上首选候选永远是
# 我们自己合成的 linux/amd64/gnu，所以「没命中首选就报回退」会让每一次普通安装都
# 多一句假警告 —— 而一条每次都响的警告会把真的 musl / Rosetta 警告一起淹掉。
check "只发两段 key 的包不报假回退"            "! grep -q '退回' '$WORK/install.log'"

echo "== list / info =="
# 规格 §9：给人看的输出走 stderr，stdout 留给机器可读的那份（--json）。
# 所以这里要 2>&1 —— 上面那条 install 的检查本来就是这么写的。
check "list 列得出来"                         "'$TOY' list 2>&1 | grep -q hello"
check "info 跑得通"                           "'$TOY' info local:test/hello"
# list 和 uninstall 都认已装的那个短名字，info 以前只认 ref —— 同一个字符串在同一个
# CLI 里有两种含义，而用户是从 list 里抄的那个名字。装过的包 info 也得认。
check "info 认 list 里印出来的那个短名字"        "'$TOY' info hello 2>&1 | grep -q hello"
check "真的写坏的 ref 仍然报解析错误"             "! '$TOY' info 'not a ref!!' 2>&1 | grep -q '^名字'"

echo "== 走镜像要如实说 =="
# 原始地址挂掉、字节从镜像拿到时必须打印出来（铁律 8）。镜像只在 toy.toml 写了
# [sha256] 时才会被用到 —— 没有 hash 就没法验证镜像给的是不是那个东西。
MIRROR="$WORK/mirror"; mkdir -p "$MIRROR"
SUM="$(sha256sum "$UP/dist/$ASSET" | cut -d' ' -f1)"
cp "$UP/dist/$ASSET" "$MIRROR/$SUM"
cat > "$UP/toy.toml" <<EOF
name    = "mirrored"
desc    = "验证镜像回退"
license = "MIT"

[bin]
mirrored = "hello"

[artifact]
"$GOOS_/$GOARCH_" = "hello_{version}_${GOOS_}_${GOARCH_}.tar.gz"

[sha256]
"$GOOS_/$GOARCH_" = "$SUM"
EOF
( cd "$UP" && git add -A && git -c user.email=t@e.st -c user.name=t commit -q -m mirrored )
# release 模板指向一个不存在的目录 → 原始地址必然失败，只剩镜像那条路。
# mirrors 必须写在任何表头【之前】：TOML 里 [[index]] 之后的裸键值属于那张表，
# 写在下面的话它会变成索引条目的一个字段，然后被静默忽略（镜像等于没配）。
cat > "$TOY_HOME/config.toml" <<EOF
mirrors = ["file://$MIRROR"]

[[index]]
name = "local"
url  = "file://$ROOT/index/toy-index.json"

[hosts.local]
raw     = "file://$UP/{path}"
clone   = "$UP"
release = "file://$UP/gone/{asset}"
web     = "file://$UP"
EOF
rm -f "$TOY_HOME/cache/"*
"$TOY" install local:test/mirrored >"$WORK/mirror.log" 2>&1 || true
check "从镜像装成功了"                        "test -L '$TOY_HOME/bin/mirrored'"
check "说明了这份字节来自镜像"                 "grep -q '来自镜像' '$WORK/mirror.log'"
check "说明里带着那个镜像地址"                 "grep -q '$MIRROR' '$WORK/mirror.log'"
"$TOY" uninstall mirrored >/dev/null 2>&1 || true
# 复原成原来那份 config，后面的用例还要用。
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
# 光看退出码不够：任何一处先报错（资产名拼错、tag 没找着）都能让它非零，
# 那样这条断言就在为一件没发生的事发绿灯。必须是【因为哈希】被拒的。
check "拒绝的理由确实是 sha256 对不上"          "grep -q 'sha256' '$WORK/bad.log'"
check "报错里把期望值和实际值都摆出来了"          "grep -q '1111111111111111111111111111111111111111111111111111111111111111' '$WORK/bad.log' && grep -qE '实际|actual' '$WORK/bad.log'"
check "哈希不对时什么都没装上"                   "! test -e '$TOY_HOME/bin/badsum'"

echo "== toy init：从 dist 的文件名反推 toy.toml，然后真的装它 =="
# 这条命令的价值全在「作者不用手写 [artifact]」，所以验收标准是整条链路：
# init 生成的文件必须能被 install 原样吃下去，不是"看起来像那么回事"。
UP2="$WORK/upstream2"
mkdir -p "$UP2/dist"
printf '#!/bin/sh\necho "gadget lives"\n' > "$BUILD/gadget"
chmod +x "$BUILD/gadget"
for P in linux_x86_64 linux_arm64 darwin_x86_64 darwin_arm64 windows_x86_64; do
  tar -C "$BUILD" -czf "$UP2/dist/gadget_2.1.0_${P}.tar.gz" gadget
done
echo notanasset > "$UP2/dist/checksums.txt"
( cd "$UP2" && "$TOY" init --name gadget >"$WORK/init.log" 2>&1 )
check "init 退出码 0 并写出了 toy.toml"        "test -f '$UP2/toy.toml'"
check "版本号被换成了 {version} 占位符"          "grep -q 'gadget_{version}_linux_x86_64.tar.gz' '$UP2/toy.toml'"
check "toy.toml 里没有留下写死的版本号"          "! grep -q '2\.1\.0' '$UP2/toy.toml'"
check "x86_64 被认成了 amd64"                  "grep -q '\"linux/amd64\"' '$UP2/toy.toml'"
check "aarch64/arm64 也认出来了"                "grep -q '\"darwin/arm64\"' '$UP2/toy.toml'"
check "算出了 [sha256]（铁律 4：从 TOFU 升到强制校验）" \
      "grep -q '^\[sha256\]' '$UP2/toy.toml' && grep -qE '^\"linux/amd64\" = \"[0-9a-f]{64}\"' '$UP2/toy.toml'"
check "Windows 被跳过且说明了原因"               "! grep -q 'windows' '$UP2/toy.toml' && grep -q 'Windows' '$WORK/init.log'"
check "checksums.txt 这种非资产没进 [artifact]"  "! grep -q 'checksums' '$UP2/toy.toml'"
check "明说了哪些地方是猜的"                     "grep -q '是猜的' '$WORK/init.log'"
( cd "$UP2" && git init -q && git add -A \
  && git -c user.email=t@e.st -c user.name=t commit -q -m init && git tag v2.1.0 )
cat > "$TOY_HOME/config.toml" <<EOF
[[index]]
name = "local"
url  = "file://$ROOT/index/toy-index.json"

[hosts.local]
raw     = "file://$UP2/{path}"
clone   = "$UP2"
release = "file://$UP2/dist/{asset}"
web     = "file://$UP2"
EOF
if "$TOY" install local:test/gadget >"$WORK/gadget.log" 2>&1; then
  ok "init 生成的 toy.toml 能被 install 原样装上"
else
  bad "init 生成的 toy.toml 装不上"; cat "$WORK/gadget.log"
fi
check "装出来的命令真的能跑"                    "gadget | grep -q 'gadget lives'"
check "已存在 toy.toml 时不覆盖"                "( cd '$UP2' && ! '$TOY' init --name gadget )"
check "--force 才覆盖"                          "( cd '$UP2' && '$TOY' init --force --name gadget )"
check "一个都认不出来时报错而不是写空壳"          "( cd '$WORK' && mkdir -p emptydist/dist && touch emptydist/dist/README && cd emptydist && ! '$TOY' init && ! test -f toy.toml )"
"$TOY" uninstall gadget >/dev/null 2>&1 || true
# 复原 config，后面的用例还指着 $UP。
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

echo "== 卸载 =="
"$TOY" uninstall hello >/dev/null 2>&1 || true
check "软链没了"                              "! test -e '$TOY_HOME/bin/hello'"
check "缓存留着（内容寻址，同时是镜像目录）"    "ls '$TOY_HOME/cache' | grep -qE '^[0-9a-f]{64}'"

echo "== doctor / keygen / serve =="
check "doctor 跑得通"                         "'$TOY' doctor"
# 平台是【中性信息】不是检查项：musl 机器既不对也不错，给它一个红叉会让人去修一件没坏的事。
# 先落成文件再 grep：`doctor | grep -q` 里 grep 一命中就退出，doctor 吃到 SIGPIPE，
# set -o pipefail 会把整条管道判失败 —— 那和这里要断言的东西没有半点关系。
"$TOY" doctor >"$WORK/doctor.log" 2>&1 || true
check "doctor 打印了当前平台"                   "grep -q '^平台 ' '$WORK/doctor.log'"
check "doctor 打印了候选顺序"                   "grep -q '候选顺序' '$WORK/doctor.log'"
check "平台那几行不带 ✓/✗"                     "! grep -E '^[✓✗].*(候选顺序|^平台)' '$WORK/doctor.log'"
check "keygen 生成了密钥"                     "'$TOY' keygen && test -f '$TOY_HOME/keys/key.sec'"
check "私钥权限是 0600"                       "test \"\$(stat -c %a '$TOY_HOME/keys/key.sec' 2>/dev/null || stat -f %Lp '$TOY_HOME/keys/key.sec')\" = 600"
check "keygen 不覆盖已有密钥"                 "! '$TOY' keygen"

echo
printf '通过 %d，失败 %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
