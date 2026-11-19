# 本机备份：pg_dump 整个库到 backups/，保留 7 天；顺便把太大的日志截短。
# launchd 任务 com.xiaoju.backup 每天 3:40 跑一次（以 mobile 用户）。手动跑：sh backup.sh
# （云盘自己每天也会把加密备份传到 Telegram 并置顶，那份用 restore.sh 恢复。）
. "$(dirname "$0")/common.sh"
load_env
out="$BACKUPS/xiaoju-$(date +%Y%m%d-%H%M).dump"
if "$PG/bin/pg_dump" "--dbname=$XIAOJU_DATABASE_URL" -Fc -f "$out"; then
    echo "$(date '+%F %T') 备份成功 $out $(wc -c <"$out") 字节" >>"$LOGS/backup.log"
else
    rm -f "$out"
    echo "$(date '+%F %T') 备份失败" >>"$LOGS/backup.log"
fi
find "$BACKUPS" -name 'xiaoju-*.dump' -mtime +7 -exec rm -f {} \;
# 日志超过 20 MB：留一份 .1，原文件清空（程序用追加方式写，清空是安全的）
for f in "$LOGS"/*.log; do
    [ -f "$f" ] || continue
    if [ "$(wc -c <"$f")" -gt 20971520 ]; then cp "$f" "$f.1" && : >"$f"; fi
done
