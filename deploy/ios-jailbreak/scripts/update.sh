# 更新到最新版：重新跑一遍 install.sh（会下载 Release「ios-jailbreak-latest」，数据和配置都保留，然后重启）。要 root。
#   sh update.sh             更新并重启
#   sh update.sh --restart   不下载，只重启云盘（改了 xiaoju.env 以后用）
# 注意：重启会打断正在进行的上传，挑没人用的时候。
. "$(dirname "$0")/common.sh"
need_root
if [ "${1:-}" = --restart ]; then
    daemon_installed com.xiaoju.server || die '云盘还没启用（数据还没恢复）。'
    daemon_restart com.xiaoju.server
    if wait_for 60 health; then good '云盘已重启。'; else bad "一分钟了还没起来，看 $LOGS/xiaoju.log"; fi
    exit 0
fi
cp "$SCRIPTS/install.sh" "$XJ/.install.sh"
exec "$SH" "$XJ/.install.sh"
