# 把数据恢复进手机上的空库 xiaoju，然后启动云盘。要 root。
#   sh restore.sh --check 文件.xjdb   只解密、检查，不写库（先跑这个）
#   sh restore.sh 文件.xjdb           用电脑上 move-out.ps1 导出的备份恢复（搬家用这个，一条不丢）
#   sh restore.sh                     不给文件：从 Telegram 频道置顶的每日备份恢复（最多丢约 24 小时）
# 库里已经有表时 xiaoju 会拒绝恢复，不会覆盖。
. "$(dirname "$0")/common.sh"
need_root
check=
[ "${1:-}" = --check ] && { check=--check; shift; }
load_env
for k in XIAOJU_MASTER_KEY TG_APP_ID TG_APP_HASH TG_BOT_TOKEN TG_CHANNEL_ID; do
    eval "v=\${$k:-}"
    [ -n "$v" ] || die "xiaoju.env 里没有 $k，先运行 import-from-pc.sh。"
done
pg_ready || die "PostgreSQL 没在运行，看 $LOGS/postgres.log"
daemon_remove com.xiaoju.server
cd "$XJ" || exit 1
step "xiaoju dbrestore $check ${1:-（从 Telegram 下载）}"
# shellcheck disable=SC2086
"$BIN/xiaoju" dbrestore $check "$@" || die 'dbrestore 失败，看上面的输出。'
if [ -n "$check" ]; then
    good '备份完好。确认没问题就去掉 --check 再跑一次，正式恢复。'
    exit 0
fi
chown -R mobile:mobile "$DATA" "$TMPD"
good '恢复完成，启动云盘…'
daemon_install com.xiaoju.server
if wait_for 120 health; then good '云盘已启动：http://127.0.0.1:8080/healthz'; else bad "两分钟了还没起来，看 $LOGS/xiaoju.log"; tail -20 "$LOGS/xiaoju.log"; fi
