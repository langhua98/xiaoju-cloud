# launchd 任务 com.xiaoju.tailscaled 跑的就是这个（root）。
# 用户态网络：不建 VPN 网卡、不改系统 DNS，只负责让 Funnel 把 https://<名字>.tail823dbd.ts.net 转到本机 127.0.0.1:8080。
. "$(dirname "$0")/common.sh"
exec >>"$LOGS/tailscaled.log" 2>&1
mkdir -p "$TS_STATE" "$RUN"
chmod 700 "$TS_STATE"
echo "---- $(date '+%Y-%m-%d %H:%M:%S') 启动 tailscaled"
exec "$BIN/tailscaled" --tun=userspace-networking --statedir="$TS_STATE" --socket="$TS_SOCK" --port=41641
