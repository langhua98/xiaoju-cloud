# 用法跟 tailscale 命令一样，只是自动带上 tailscaled 的 socket。要 root。例如：
#   sh ts.sh up --hostname=node-7p      第一次登录（会打出一个链接，在任意设备浏览器里打开登录）
#   sh ts.sh status
#   sh ts.sh funnel --bg 8080           把云盘开到公网 https://<名字>.tail823dbd.ts.net
#   sh ts.sh funnel status
#   sh ts.sh set --hostname=node        改名字（搬家那一步用）
. "$(dirname "$0")/common.sh"
need_root
exec "$BIN/tailscale" --socket="$TS_SOCK" "$@"
