#!/bin/bash
# 在 GitHub 的 Mac 机器上（Apple 芯片，装了 Xcode）把越狱 iPhone 要用的东西都编译好，打成一个包：
#   dist/xiaoju-ios-arm64.tar.gz（+ .sha256）
# 包里有：
#   bin/xiaoju                 云盘（Go，GOOS=ios，要 cgo 外部链接）
#   bin/tailscaled, tailscale  Tailscale（用户态网络，不要 VPN 权限），用来保留 node.tail823dbd.ts.net 这个地址
#   pgsql/                     PostgreSQL 17（打了 postgres-ios.patch：iOS 内核没有 SysV 共享内存，改用 mmap + 命名 POSIX 信号量）
#   pgdata-template.tar.gz     在 Mac 上 initdb 出来的空数据目录（手机上没有 /bin/sh 时 initdb 跑不了，所以在这里先做好）
#   scripts/, launchd/         手机上的安装、运行脚本
# 所有可执行文件和库都用 entitlements.plist 做 ad-hoc 签名。
# 用法：deploy/ios-jailbreak/build.sh（在仓库根目录运行）。PG_CACHE 目录存在时复用编译好的 PostgreSQL。
set -euo pipefail

PG_VERSION=17.12
PG_SHA256=9cf136a703d8ee2f83465c43be4e9e8e7930c82b0bfba13be7e2537be769ac55
TS_VERSION=v1.104.7
IOS_MIN=14.0
# 手机上的安装位置（rootful / rootless 越狱都一样，/var/mobile 总是可写）。PostgreSQL 把路径编进程序里，必须和这里一致。
PREFIX=/var/mobile/xiaoju

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
WORK=${WORK:-$ROOT/build-ios}
PG_CACHE=${PG_CACHE:-$WORK/pg-cache}
OUT=$WORK/stage
DIST=$ROOT/dist
ENTS=$HERE/entitlements.plist

SDK=$(xcrun --sdk iphoneos --show-sdk-path)
IOS_CC="$(xcrun --sdk iphoneos -f clang) -isysroot $SDK -arch arm64 -miphoneos-version-min=$IOS_MIN"
echo "iOS SDK: $SDK"

rm -rf "$OUT" && mkdir -p "$OUT/bin" "$WORK" "$DIST"

# ---------- 1. Go：云盘和 Tailscale ----------
export CGO_ENABLED=1 GOOS=ios GOARCH=arm64 CC="$IOS_CC" CGO_CFLAGS="-O2" CGO_LDFLAGS=""
echo "== xiaoju"
(cd "$ROOT" && go build -trimpath -ldflags="-s -w" -o "$OUT/bin/xiaoju" ./cmd/xiaoju)
echo "== tailscale $TS_VERSION"
# 跨平台编译时不能设 GOBIN，装到 $GOPATH/bin/ios_arm64/
unset GOBIN
# iOS 上 Tailscale SSH 编不了（feature/ssh 排除了 ios），也用不着
(cd "$WORK" && go install -trimpath -ldflags="-s -w" -tags ts_omit_ssh \
    tailscale.com/cmd/tailscaled@$TS_VERSION tailscale.com/cmd/tailscale@$TS_VERSION)
cp "$(go env GOPATH)/bin/ios_arm64/tailscaled" "$(go env GOPATH)/bin/ios_arm64/tailscale" "$OUT/bin/"
unset CGO_ENABLED GOOS GOARCH CC CGO_CFLAGS CGO_LDFLAGS

# ---------- 2. PostgreSQL ----------
PG_KEY="$PG_VERSION-$(shasum -a 256 "$HERE/postgres-ios.patch" "$HERE/ios_compat.h" "$0" | shasum -a 256 | cut -c1-16)"
if [ -f "$PG_CACHE/$PG_KEY/done" ]; then
    echo "== PostgreSQL $PG_VERSION（用缓存 $PG_KEY）"
else
    echo "== PostgreSQL $PG_VERSION"
    rm -rf "$PG_CACHE" && mkdir -p "$PG_CACHE/$PG_KEY"
    tarball=$WORK/postgresql-$PG_VERSION.tar.bz2
    [ -f "$tarball" ] || curl -fsSL -o "$tarball" "https://ftp.postgresql.org/pub/source/v$PG_VERSION/postgresql-$PG_VERSION.tar.bz2"
    echo "$PG_SHA256  $tarball" | shasum -a 256 -c -
    common=(--without-readline --without-icu --without-perl --without-python --without-tcl --without-openssl --with-zlib)

    # 2a. Mac 本机版：只用来跑 zic（编时区数据）和 initdb（做空数据目录模板）
    rm -rf "$WORK/pg-host" && mkdir -p "$WORK/pg-host" && tar -xjf "$tarball" -C "$WORK/pg-host" --strip-components=1
    (cd "$WORK/pg-host" && ./configure --prefix="$PG_CACHE/$PG_KEY/host" "${common[@]}" >configure.log \
        && make -j"$(sysctl -n hw.ncpu)" -s && make -s install) || { tail -50 "$WORK/pg-host/configure.log"; exit 1; }

    # 2b. iPhone 版：打补丁，交叉编译
    rm -rf "$WORK/pg-ios" && mkdir -p "$WORK/pg-ios" && tar -xjf "$tarball" -C "$WORK/pg-ios" --strip-components=1
    (cd "$WORK/pg-ios" && patch -p1 <"$HERE/postgres-ios.patch")
    (cd "$WORK/pg-ios" && PREFERRED_SEMAPHORES=NAMED_POSIX ./configure \
        --host=aarch64-apple-darwin --prefix="$PREFIX/pgsql" "${common[@]}" \
        PG_SYSROOT="$SDK" CC="$IOS_CC" CPPFLAGS="-DPG_NO_SYSV_SHMEM -include $HERE/ios_compat.h" >configure.log) \
        || { tail -80 "$WORK/pg-ios/config.log"; exit 1; }
    grep -q 'USE_NAMED_POSIX_SEMAPHORES 1' "$WORK/pg-ios/src/include/pg_config.h" || { echo '没选上命名 POSIX 信号量'; exit 1; }
    (cd "$WORK/pg-ios" && make -j"$(sysctl -n hw.ncpu)" -s \
        && make -s install DESTDIR="$PG_CACHE/$PG_KEY/ios" ZIC="$PG_CACHE/$PG_KEY/host/bin/zic")

    # 2c. 空数据目录模板（C 语言环境、UTF8；本机 socket 免密，TCP 要密码）
    tmpl=$WORK/pgdata-template
    rm -rf "$tmpl"
    "$PG_CACHE/$PG_KEY/host/bin/initdb" -D "$tmpl" -U postgres --locale=C -E UTF8 \
        --auth-local=trust --auth-host=scram-sha-256 --no-instructions
    tar -czf "$PG_CACHE/$PG_KEY/pgdata-template.tar.gz" -C "$tmpl" .
    touch "$PG_CACHE/$PG_KEY/done"
fi
mkdir -p "$OUT/pgsql"
cp -R "$PG_CACHE/$PG_KEY/ios$PREFIX/pgsql/." "$OUT/pgsql/"
cp "$PG_CACHE/$PG_KEY/pgdata-template.tar.gz" "$OUT/"
# 手机上用不到的东西删掉，包小一点
rm -rf "$OUT/pgsql/include" "$OUT/pgsql/lib/pgxs" "$OUT/pgsql/share/doc" "$OUT/pgsql/lib/"*.a
for f in "$OUT/pgsql/bin/"*; do
    case $(basename "$f") in postgres|psql|pg_dump|pg_restore|pg_isready|pg_controldata|pg_resetwal) ;; *) rm -f "$f" ;; esac
done

# ---------- 3. 签名、打包 ----------
echo "== 签名"
find "$OUT/bin" "$OUT/pgsql" -type f \( -perm -u+x -o -name '*.dylib' -o -name '*.so' \) -print0 |
while IFS= read -r -d '' f; do
    if file "$f" | grep -q 'Mach-O'; then
        codesign -f -s - --entitlements "$ENTS" "$f" 2>/dev/null || codesign -f -s - "$f"
    fi
done
file "$OUT/bin/xiaoju" "$OUT/bin/tailscaled" "$OUT/pgsql/bin/postgres"
otool -l "$OUT/bin/xiaoju" | grep -A4 LC_BUILD_VERSION | head -5

mkdir -p "$OUT/scripts" "$OUT/launchd"
cp "$HERE"/scripts/*.sh "$OUT/scripts/"
cp "$HERE"/launchd/*.plist "$OUT/launchd/"
cp "$ENTS" "$OUT/"
{
    echo "built=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=${GITHUB_SHA:-$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)}"
    echo "postgresql=$PG_VERSION"
    echo "tailscale=$TS_VERSION"
} >"$OUT/VERSION"
cat "$OUT/VERSION"

tar -czf "$DIST/xiaoju-ios-arm64.tar.gz" -C "$OUT" .
(cd "$DIST" && shasum -a 256 xiaoju-ios-arm64.tar.gz | tee xiaoju-ios-arm64.tar.gz.sha256)
ls -lh "$DIST"
