#!/usr/bin/env bash
# End-to-end test: fully offline, never touches the network, never touches the real ~/.toy.
#
# The trick is a fake host: the clone template points at a local git repo (git ls-remote
# accepts local paths) and the raw/release templates point at file:// — so the whole
# install chain (ls-remote for tags -> pick semver -> fetch toy.toml -> download asset ->
# verify checksum -> extract -> symlink) runs offline.
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

# macOS has no sha256sum (only shasum); Linux often has no shasum. Accept either.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

echo "== build =="
( cd "$ROOT" && go build -o "$TOY" ./cmd/toy )
ok "toy builds"

echo "== fresh store: no index configured at all =="
# The built-in index is empty now (store.DefaultIndexURL). Having no index is a valid
# state, not a failure: search must say how to add one, and doctor must not print a ✗
# for it. Use a separate TOY_HOME so the config.toml written further down can't skew it.
FRESH="$WORK/fresh"
check "search on a fresh store says 'no index configured', not 'no matches'" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'No index is configured'"
check "search shows a toy index add example (with file://)" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'toy index add' && \
       TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'file://'"
check "search points out that github: needs no index" \
      "TOY_HOME='$FRESH' '$TOY' search json 2>&1 | grep -q 'toy install github:owner/repo'"
check "the default config.toml it writes carries no built-in index" \
      "! grep -q '^\[\[index\]\]' '$FRESH/config.toml'"
# doctor still fails on an unconfigured PATH, so only assert that "no index" isn't a ✗.
# doctor's exit code is 1 anyway (PATH), and set -o pipefail would fail the whole
# pipeline over it — which is not what's being asserted here. Write to a file, then grep.
TOY_HOME="$FRESH" "$TOY" doctor >"$WORK/doctor-fresh.log" 2>&1 || true
check "doctor does not print ✗ for 'no index configured'" \
      "grep 'no index configured' '$WORK/doctor-fresh.log' | grep -q '✓'"
check "no ✗ line in doctor is about the index" \
      "! grep '^✗' '$WORK/doctor-fresh.log' | grep -qi index"

echo "== build a fake upstream repo =="
UP="$WORK/upstream"
mkdir -p "$UP/dist"
cat > "$UP/toy.toml" <<'EOF'
name    = "hello"
desc    = "sample tool for the end-to-end test"
license = "MIT"

[bin]
hello = "hello"

[artifact]
"linux/amd64"  = "hello_{version}_linux_amd64.tar.gz"
"linux/arm64"  = "hello_{version}_linux_arm64.tar.gz"
"darwin/amd64" = "hello_{version}_darwin_amd64.tar.gz"
"darwin/arm64" = "hello_{version}_darwin_arm64.tar.gz"
EOF

# Build the release asset: one tar.gz holding one executable.
BUILD="$WORK/build"; mkdir -p "$BUILD"
printf '#!/bin/sh\necho "hello from toy e2e"\n' > "$BUILD/hello"
chmod +x "$BUILD/hello"
GOOS_="$(go env GOOS)"; GOARCH_="$(go env GOARCH)"
ASSET="hello_1.2.0_${GOOS_}_${GOARCH_}.tar.gz"
tar -C "$BUILD" -czf "$UP/dist/$ASSET" hello
ok "built release asset $ASSET"

( cd "$UP" && git init -q && git add -A \
  && git -c user.email=t@e.st -c user.name=t commit -q -m init \
  && git tag v1.0.0 && git tag v1.2.0 && git tag release-junk && git tag v2.0.0-rc.1 )
ok "upstream has 4 tags (one non-semver, one prerelease)"

echo "== point a host at the local repo =="
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
ok "wrote config.toml"

echo "== install =="
if "$TOY" install local:test/hello 2>&1 | tee "$WORK/install.log"; then
  ok "install exits 0"
else
  bad "install failed"; cat "$WORK/install.log"
fi

check "picked the highest stable release v1.2.0 (not v2.0.0-rc.1, undisturbed by release-junk)" \
      "grep -q '1\.2\.0' '$WORK/install.log'"
check "symlink lands under \$TOY_HOME/bin"    "test -L '$TOY_HOME/bin/hello'"
check "the installed command actually runs"   "hello | grep -q 'hello from toy e2e'"
check "an install manifest was written"       "ls '$TOY_HOME/installed'/*/*/*.toml >/dev/null 2>&1 || ls -R '$TOY_HOME/installed' | grep -q toml"
check "cache filename is the sha256 (content-addressed)" "ls '$TOY_HOME/cache' | grep -qE '^[0-9a-f]{64}'"
check "trust level was printed"               "grep -qE 'unsigned|✓ signed|sha256 pinned' '$WORK/install.log'"
# The toy.toml above declares only two-segment keys, which is what almost every project
# does. On linux the first candidate is always the linux/amd64/gnu key we synthesize
# ourselves, so "warn whenever the first candidate misses" would add a bogus warning to
# every ordinary install — and a warning that fires every time drowns out the real musl
# and Rosetta ones.
check "a package with only two-segment keys raises no bogus fallback warning" \
      "! grep -q '^warning:' '$WORK/install.log'"

echo "== list / info =="
# Spec §9: human-facing output goes to stderr, stdout is reserved for the machine-readable
# form (--json). Hence the 2>&1 here — the install check above already does the same.
check "list shows it"                         "'$TOY' list 2>&1 | grep -q hello"
check "info works"                            "'$TOY' info local:test/hello"
# list and uninstall both accept the installed short name; info used to accept only a ref
# — the same string meaning two different things in one CLI, while the user is copying
# that name straight out of list. info must accept it too for installed packages.
check "info accepts the short name that list printed" "'$TOY' info hello 2>&1 | grep -q hello"
check "a genuinely malformed ref still reports a parse error" "! '$TOY' info 'not a ref!!' 2>&1 | grep -q '^name '"

echo "== a mirror must be disclosed =="
# When the original URL is down and the bytes came from a mirror, say so (iron law 8).
# Mirrors are only reachable when toy.toml declares [sha256] — without a hash there is no
# way to verify that the mirror handed back the right thing.
MIRROR="$WORK/mirror"; mkdir -p "$MIRROR"
SUM="$(sha256_of "$UP/dist/$ASSET")"
cp "$UP/dist/$ASSET" "$MIRROR/$SUM"
cat > "$UP/toy.toml" <<EOF
name    = "mirrored"
desc    = "exercises the mirror fallback"
license = "MIT"

[bin]
mirrored = "hello"

[artifact]
"$GOOS_/$GOARCH_" = "hello_{version}_${GOOS_}_${GOARCH_}.tar.gz"

[sha256]
"$GOOS_/$GOARCH_" = "$SUM"
EOF
( cd "$UP" && git add -A && git -c user.email=t@e.st -c user.name=t commit -q -m mirrored )
# Point the release template at a directory that does not exist -> the original URL is
# guaranteed to fail and only the mirror path is left.
# mirrors must be written BEFORE any table header: in TOML a bare key after [[index]]
# belongs to that table, so putting it below turns it into a field of the index entry,
# which is then silently ignored (i.e. no mirror configured at all).
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
check "installed from the mirror"             "test -L '$TOY_HOME/bin/mirrored'"
check "said the bytes came from a mirror"     "grep -q 'came from the mirror' '$WORK/mirror.log'"
check "named the mirror it used"              "grep -q '$MIRROR' '$WORK/mirror.log'"
"$TOY" uninstall mirrored >/dev/null 2>&1 || true
# Restore the original config; the cases below still need it.
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

echo "== cargo-dist shape: the single top-level directory gets stripped =="
# That directory name carries both the version and the target triple, while [bin] is a
# single string that does not vary by platform and does not expand {version} — it simply
# cannot express it. Without stripping, ripgrep / fd / bat / delta / eza — the largest
# single block of modern CLI tools — are all unpackageable.
NEST="$WORK/nest"; mkdir -p "$NEST/tinytool-3.1.0-${GOOS_}_${GOARCH_}/doc"
printf '#!/bin/sh\necho "nested ok"\n' > "$NEST/tinytool-3.1.0-${GOOS_}_${GOARCH_}/tinytool"
chmod +x "$NEST/tinytool-3.1.0-${GOOS_}_${GOARCH_}/tinytool"
echo man > "$NEST/tinytool-3.1.0-${GOOS_}_${GOARCH_}/doc/tinytool.1"
tar -C "$NEST" -czf "$UP/dist/tinytool_1.2.0_${GOOS_}_${GOARCH_}.tar.gz" "tinytool-3.1.0-${GOOS_}_${GOARCH_}"
cat > "$UP/toy.toml" <<EOF
name = "tinytool"
desc = "whole tree wrapped in one directory"
license = "MIT"
[bin]
tinytool = "tinytool"
[artifact]
"$GOOS_/$GOARCH_" = "tinytool_{version}_${GOOS_}_${GOARCH_}.tar.gz"
EOF
( cd "$UP" && git add -A && git -c user.email=t@e.st -c user.name=t commit -q -m nest )
if "$TOY" install local:test/tinytool >"$WORK/nest.log" 2>&1; then
  ok "[bin] written as the post-strip \"tinytool\" installs fine"
else
  bad "a package wrapped in one directory failed to install"; cat "$WORK/nest.log"
fi
check "the installed command actually runs"   "tinytool | grep -q 'nested ok'"
PKGDIR="$(dirname "$(readlink "$TOY_HOME/bin/tinytool")")"
check "package dir has tinytool at the top; the wrapper directory is gone" \
      "test -f '$PKGDIR/tinytool' && test ! -e '$PKGDIR/tinytool-3.1.0-${GOOS_}_${GOARCH_}'"
check "nothing else was lost (a layer is stripped, not content)" "test -f '$PKGDIR/doc/tinytool.1'"
"$TOY" uninstall tinytool >/dev/null 2>&1 || true

echo "== iron law: never shadow a command already on PATH =="
cat > "$UP/toy.toml" <<'EOF'
name = "evil"
desc = "tries to shadow ls"
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
  bad "a package shadowing ls got installed (iron law 5 broken)"
else
  ok "refused the package that would shadow ls"
fi
check "the refusal explains itself"           "grep -q 'shadow' '$WORK/evil.log'"

echo "== iron law: a checksum mismatch must be refused =="
cat > "$UP/toy.toml" <<'EOF'
name = "badsum"
desc = "checksum deliberately wrong"
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
  bad "a checksum mismatch got installed (iron law 4 broken)"
else
  ok "refused the package whose checksum did not match"
fi
# The exit code alone is not enough: any earlier error (wrong asset name, no tag found)
# also makes it non-zero, and then this assertion is a green light for something that
# never happened. It has to be refused BECAUSE of the checksum.
check "the refusal really is a sha256 mismatch" "grep -q 'sha256' '$WORK/bad.log'"
check "the error shows both the expected and the actual value" "grep -q '1111111111111111111111111111111111111111111111111111111111111111' '$WORK/bad.log' && grep -qE 'expected .*got' '$WORK/bad.log'"
check "nothing was installed on a checksum mismatch" "! test -e '$TOY_HOME/bin/badsum'"

echo "== toy init: infer toy.toml from dist filenames, then actually install it =="
# This command's whole value is "the author never hand-writes [artifact]", so the
# acceptance criterion is the full chain: what init generates must be swallowed by
# install verbatim, not merely look plausible.
UP2="$WORK/upstream2"
mkdir -p "$UP2/dist"
printf '#!/bin/sh\necho "gadget lives"\n' > "$BUILD/gadget"
chmod +x "$BUILD/gadget"
for P in linux_x86_64 linux_arm64 darwin_x86_64 darwin_arm64 windows_x86_64; do
  tar -C "$BUILD" -czf "$UP2/dist/gadget_2.1.0_${P}.tar.gz" gadget
done
echo notanasset > "$UP2/dist/checksums.txt"
( cd "$UP2" && "$TOY" init --name gadget >"$WORK/init.log" 2>&1 )
check "init exits 0 and wrote a toy.toml"     "test -f '$UP2/toy.toml'"
check "the version was replaced by the {version} placeholder" "grep -q 'gadget_{version}_linux_x86_64.tar.gz' '$UP2/toy.toml'"
check "no hard-coded version was left in toy.toml" "! grep -q '2\.1\.0' '$UP2/toy.toml'"
check "x86_64 was recognized as amd64"        "grep -q '\"linux/amd64\"' '$UP2/toy.toml'"
check "aarch64/arm64 was recognized too"      "grep -q '\"darwin/arm64\"' '$UP2/toy.toml'"
check "[sha256] was computed (iron law 4: TOFU upgraded to enforced verification)" \
      "grep -q '^\[sha256\]' '$UP2/toy.toml' && grep -qE '^\"linux/amd64\" = \"[0-9a-f]{64}\"' '$UP2/toy.toml'"
check "Windows was skipped and the reason was stated" "! grep -q 'windows' '$UP2/toy.toml' && grep -q 'Windows' '$WORK/init.log'"
check "non-assets like checksums.txt stayed out of [artifact]" "! grep -q 'checksums' '$UP2/toy.toml'"
check "it says out loud which parts were guessed" "grep -q 'were guessed' '$WORK/init.log'"
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
  ok "the toy.toml init generated installs verbatim"
else
  bad "the toy.toml init generated does not install"; cat "$WORK/gadget.log"
fi
check "the installed command actually runs"   "gadget | grep -q 'gadget lives'"
check "an existing toy.toml is not overwritten" "( cd '$UP2' && ! '$TOY' init --name gadget )"
check "--force does overwrite it"             "( cd '$UP2' && '$TOY' init --force --name gadget )"
check "recognizing nothing is an error, not an empty stub" "( cd '$WORK' && mkdir -p emptydist/dist && touch emptydist/dist/README && cd emptydist && ! '$TOY' init && ! test -f toy.toml )"
"$TOY" uninstall gadget >/dev/null 2>&1 || true
# Restore the config; the remaining cases still point at $UP.
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

echo "== uninstall =="
"$TOY" uninstall hello >/dev/null 2>&1 || true
check "the symlink is gone"                   "! test -e '$TOY_HOME/bin/hello'"
check "the cache stays (content-addressed, and it doubles as a mirror dir)" "ls '$TOY_HOME/cache' | grep -qE '^[0-9a-f]{64}'"

echo "== doctor / keygen / serve =="
check "doctor runs"                           "'$TOY' doctor"
# The platform is NEUTRAL INFORMATION, not a check: a musl box is neither right nor
# wrong, and a red ✗ would send someone off to fix something that isn't broken.
# Write to a file, then grep: with `doctor | grep -q` the grep exits on first match,
# doctor takes SIGPIPE, and set -o pipefail fails the pipeline — which has nothing at
# all to do with what is being asserted here.
"$TOY" doctor >"$WORK/doctor.log" 2>&1 || true
check "doctor prints the current platform"    "grep -q '^platform ' '$WORK/doctor.log'"
check "doctor prints the candidate order"     "grep -q 'tried in this order' '$WORK/doctor.log'"
check "the platform lines carry no ✓/✗"       "! grep -E '^[✓✗].*(tried in this order|platform )' '$WORK/doctor.log'"
check "keygen generates a key pair"           "'$TOY' keygen && test -f '$TOY_HOME/keys/key.sec'"
check "the secret key is mode 0600"           "test \"\$(stat -c %a '$TOY_HOME/keys/key.sec' 2>/dev/null || stat -f %Lp '$TOY_HOME/keys/key.sec')\" = 600"
check "keygen refuses to overwrite an existing key" "! '$TOY' keygen"

echo
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
