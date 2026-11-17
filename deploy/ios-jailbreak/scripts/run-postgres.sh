# launchd 任务 com.xiaoju.postgres 跑的就是这个（以 mobile 用户）。挂了 launchd 10 秒后自动拉起。
# 不用 pg_ctl：它要通过 /bin/sh 启动 postgres，rootless 越狱没有 /bin/sh。
. "$(dirname "$0")/common.sh"
exec >>"$LOGS/postgres.log" 2>&1
echo "---- $(date '+%Y-%m-%d %H:%M:%S') 启动 PostgreSQL"
ulimit -n 4096 2>/dev/null || true
exec "$PG/bin/postgres" -D "$PGDATA"
