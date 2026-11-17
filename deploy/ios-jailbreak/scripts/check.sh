# 看一眼各部分是否正常。要 root。  sh check.sh
. "$(dirname "$0")/common.sh"
need_root
cat "$XJ/VERSION" 2>/dev/null | tr '\n' ' '; echo
for l in $LABELS; do
    if ! daemon_installed "$l"; then echo "[--] $l 没装"; continue; fi
    p=$(daemon_pid "$l")
    case $l in
        com.xiaoju.backup) echo "[好] $l 每天 3:40" ;;
        *) if [ -n "$p" ]; then good "$l 运行中（PID $p）"; else bad "$l 没在运行"; fi ;;
    esac
done
if pg_ready; then good 'PostgreSQL 接受连接'; else bad 'PostgreSQL 连不上'; fi
if health; then good '云盘 /healthz'; else bad '云盘 /healthz 不通'; fi
"$BIN/tailscale" --socket="$TS_SOCK" funnel status 2>/dev/null | sed 's/^/   /'
last=$(ls -t "$BACKUPS"/xiaoju-*.dump 2>/dev/null | head -1)
if [ -n "$last" ]; then echo "[好] 本机备份 $(basename "$last")，$(( $(wc -c <"$last") / 1048576 )) MB"; else echo '[--] 还没有本机备份（每天 3:40）'; fi
echo "剩余空间：$(df -m /var/mobile | awk 'NR == 2 { print $4 }') MB"
echo
echo "最近的云盘日志（$LOGS/xiaoju.log）："
tail -15 "$LOGS/xiaoju.log" 2>/dev/null
