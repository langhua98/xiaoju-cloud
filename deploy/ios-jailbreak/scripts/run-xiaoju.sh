# launchd 任务 com.xiaoju.server 跑的就是这个（以 mobile 用户）。挂了 launchd 10 秒后自动拉起。
# 每次启动都重新读 xiaoju.env，改完 `sh update.sh --restart` 就生效。日志在 logs/xiaoju.log。
. "$(dirname "$0")/common.sh"
exec >>"$LOGS/xiaoju.log" 2>&1
# 等数据库起来（开机时两个任务同时启动）
wait_for 120 pg_ready || echo "$(date '+%F %T') 两分钟了数据库还没起来，照样启动云盘"
load_env
# 手机只有 3 GB 内存：让 Go 早点回收，别被 iOS 的内存管理（jetsam）杀掉
export GOMEMLIMIT="${GOMEMLIMIT:-400MiB}"
cd "$XJ" || exit 1
echo "---- $(date '+%Y-%m-%d %H:%M:%S') 启动云盘"
exec "$BIN/xiaoju" serve
