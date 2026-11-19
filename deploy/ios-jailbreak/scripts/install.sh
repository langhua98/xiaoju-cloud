# 在越狱的 iPhone 上装好小橘云盘要的一切：PostgreSQL、云盘程序、Tailscale、开机（越狱后）自动运行。
# 用法（root）：
#   curl -fsSLo /var/mobile/install.sh https://github.com/langhua98/xiaoju-cloud/releases/download/ios-jailbreak-latest/install.sh
#   sh /var/mobile/install.sh                  从 GitHub Release「ios-jailbreak-latest」下载
#   sh /var/mobile/install.sh 本地的.tar.gz     用已经下载好的包
# 可以重复运行：数据库、配置、Tailscale 登录都会保留。
# 不会启动云盘：库是空的，要等 restore.sh 恢复完数据才启动（不然云盘会在空库里建表，就没法恢复了）。
set -eu
if [ -f "$(dirname "$0")/common.sh" ]; then
    . "$(dirname "$0")/common.sh"
else
    # 第一次运行时脚本是单独下载的：从同一个 Release 把 common.sh 拿下来
    tmpc=/var/mobile/.xiaoju-common.sh
    curl -fsSLo "$tmpc" "https://github.com/langhua98/xiaoju-cloud/releases/download/ios-jailbreak-latest/common.sh"
    . "$tmpc"
fi
need_root

step '1/7 检查手机'
uname -a
if [ -n "$JB" ]; then echo "rootless 越狱（东西在 /var/jb）"; else echo "rootful 越狱"; fi
id mobile >/dev/null 2>&1 || die '找不到 mobile 用户？'
free_mb=$(df -m /var/mobile | awk 'NR == 2 { print $4 }')
echo "剩余空间：${free_mb} MB"
[ "${free_mb:-0}" -gt 3000 ] || die '剩余空间不到 3 GB，先清一点。'

step '2/7 安装依赖（Sileo/Procursus 的 apt）'
APT=$(command -v apt-get || echo "$JB/usr/bin/apt-get")
"$APT" update || echo '（apt update 有警告，继续）'
# curl 下载，ffmpeg 做视频缩略图，openssh 让电脑能拷文件过来，ldid 补签名
"$APT" install -y curl tar gzip coreutils openssh ldid || die 'apt 安装失败，看上面的输出。'
"$APT" install -y ffmpeg || echo '装不上 ffmpeg：视频没有缩略图，别的不影响。'

step '3/7 下载云盘、PostgreSQL、Tailscale'
mkdir -p "$XJ" "$INBOX"
pkg=${1:-}
if [ -z "$pkg" ]; then
    base="https://github.com/$REPO/releases/download/$RELEASE_TAG"
    pkg=$XJ/$BUNDLE
    curl -fL --retry 3 -o "$pkg" "$base/$BUNDLE"
    curl -fsSL --retry 3 -o "$pkg.sha256" "$base/$BUNDLE.sha256"
    want=$(cut -d' ' -f1 <"$pkg.sha256")
    got=$(sha256_of "$pkg")
    [ "$want" = "$got" ] || die "下载的包校验不对（要 $want，实际 $got）。"
fi
# 先停掉在跑的程序再覆盖
for l in com.xiaoju.server com.xiaoju.postgres com.xiaoju.tailscaled; do
    daemon_installed "$l" && "$LAUNCHCTL" unload "$(daemon_plist "$l")" 2>/dev/null || true
done
stage=$XJ/.stage
rm -rf "$stage" && mkdir -p "$stage"
tar -xzf "$pkg" -C "$stage"
rm -rf "$BIN" "$PG" "$SCRIPTS" "$XJ/launchd"
mv "$stage/bin" "$stage/pgsql" "$stage/scripts" "$stage/launchd" "$XJ/"
mv "$stage/VERSION" "$stage/entitlements.plist" "$XJ/"
[ -f "$PGDATA/PG_VERSION" ] || mv "$stage/pgdata-template.tar.gz" "$XJ/"
rm -rf "$stage"
cat "$XJ/VERSION"
. "$SCRIPTS/common.sh"
# 包是在 Mac 上 ad-hoc 签好的。手机上有 ldid 就再签一次（有的越狱只认本机签的）
if command -v ldid >/dev/null 2>&1; then
    for f in "$BIN"/* "$PG"/bin/* "$PG"/lib/*.dylib "$PG"/lib/postgresql/*.dylib; do
        [ -f "$f" ] && ldid -S"$XJ/entitlements.plist" "$f" 2>/dev/null || true
    done
fi

step '4/7 建目录'
mkdir -p "$CONFIG" "$DATA" "$TMPD" "$LOGS" "$BACKUPS" "$RUN" "$TS_STATE"
chown -R mobile:mobile "$XJ"
chown -R root:wheel "$TS_STATE"
chmod 700 "$CONFIG" "$DATA" "$RUN" "$TS_STATE"
chmod 755 "$XJ"

step '5/7 PostgreSQL：数据目录、只听本机、建库和用户 xiaoju'
if [ ! -f "$PGDATA/PG_VERSION" ]; then
    mkdir -p "$PGDATA"
    tar -xzf "$XJ/pgdata-template.tar.gz" -C "$PGDATA"
    rm -f "$XJ/pgdata-template.tar.gz"
    echo '空数据目录已建好。'
fi
# 手机内存小，按 3 GB 内存调；iOS 没有 SysV 共享内存，全用 mmap
cat >"$PGDATA/xiaoju.conf" <<EOF
# install.sh 生成，重装会覆盖。要改的话改 postgresql.auto.conf（ALTER SYSTEM）。
listen_addresses = 'localhost'
port = $PGPORT
unix_socket_directories = '$RUN'
unix_socket_permissions = 0700
shared_memory_type = mmap
dynamic_shared_memory_type = mmap
huge_pages = off
max_connections = 30
shared_buffers = 64MB
work_mem = 4MB
maintenance_work_mem = 32MB
effective_cache_size = 512MB
max_worker_processes = 4
max_parallel_workers_per_gather = 0
max_parallel_maintenance_workers = 0
max_wal_size = 256MB
min_wal_size = 64MB
checkpoint_timeout = 15min
logging_collector = off
log_min_messages = warning
log_line_prefix = '%m [%p] '
EOF
grep -q "^include_if_exists = 'xiaoju.conf'" "$PGDATA/postgresql.conf" ||
    echo "include_if_exists = 'xiaoju.conf'" >>"$PGDATA/postgresql.conf"
chown -R mobile:mobile "$PGDATA"
chmod 700 "$PGDATA"
daemon_install com.xiaoju.postgres
wait_for 60 pg_ready || { tail -30 "$LOGS/postgres.log"; die 'PostgreSQL 一分钟了还没起来，看上面的日志（把它发给 Claude）。'; }
good 'PostgreSQL 起来了'

dbpw=$(env_get XIAOJU_DATABASE_URL "$ENV_FILE" | sed -n 's#^postgres://xiaoju:\([0-9a-f]*\)@.*#\1#p')
[ -n "$dbpw" ] || dbpw=$(new_secret)
if [ -z "$(psql_super "SELECT 1 FROM pg_roles WHERE rolname = 'xiaoju'")" ]; then
    psql_super "CREATE ROLE xiaoju LOGIN PASSWORD '$dbpw'" >/dev/null
else
    psql_super "ALTER ROLE xiaoju PASSWORD '$dbpw'" >/dev/null
fi
if [ -z "$(psql_super "SELECT 1 FROM pg_database WHERE datname = 'xiaoju'")" ]; then
    psql_super 'CREATE DATABASE xiaoju OWNER xiaoju' >/dev/null
    echo '建好了空库 xiaoju（表等恢复时再建）。'
fi
tables=$(psql_super "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" xiaoju)

step '6/7 写环境变量文件'
touch "$ENV_FILE"
env_set XIAOJU_DATABASE_URL "postgres://xiaoju:$dbpw@127.0.0.1:$PGPORT/xiaoju?sslmode=disable" "$ENV_FILE"
env_set XIAOJU_HTTP_ADDR 127.0.0.1:8080 "$ENV_FILE"
env_set XIAOJU_STORAGE telegram "$ENV_FILE"
env_set XIAOJU_TMP_DIR "$TMPD" "$ENV_FILE"
env_set TG_SESSION_FILE "$DATA/tg-session.json" "$ENV_FILE"
env_set TG_BACKUP_SESSION_FILE "$DATA/tg-backup-session.json" "$ENV_FILE"
chown mobile:mobile "$ENV_FILE"
chmod 600 "$ENV_FILE"

step '7/7 注册自动运行的任务'
daemon_install com.xiaoju.tailscaled
daemon_install com.xiaoju.backup
if [ "$tables" -gt 0 ] && [ -n "$(env_get XIAOJU_MASTER_KEY "$ENV_FILE")" ]; then
    daemon_install com.xiaoju.server
    wait_for 60 health && good '云盘已启动' || bad "云盘一分钟了还没起来，看 $LOGS/xiaoju.log"
else
    daemon_remove com.xiaoju.server
    echo '云盘先不启动（数据还没恢复）。'
fi

echo
echo '装好了。下一步：'
echo "  1. 自检（不碰电脑上的云盘）：sh $SCRIPTS/selftest.sh"
echo "  2. 登录 Tailscale（先用一个临时名字）：sh $SCRIPTS/ts.sh up --hostname=node-7p"
echo '  正式搬家的步骤见 deploy/ios-jailbreak/README.md。'
