# 搬家前的自检：只在手机上测，不碰电脑上的云盘，也不连 Telegram。要 root。
#   sh selftest.sh
# 测这些：PostgreSQL 能读写、被强杀后能自己恢复、云盘程序能在这台手机上跑并建好全部表、
# ffmpeg、Tailscale、内存和空间。最后（Tailscale 登录了的话）用 Funnel 开一个测试网页，在别的设备上打开看看。
# 全是 [好] 就可以准备正式搬家；有 [坏] 把整段输出发给 Claude。
. "$(dirname "$0")/common.sh"
need_root
fail=0
no() { bad "$*"; fail=1; }

step '手机'
uname -a
echo "内存：$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1048576 )) MB"
echo "剩余空间：$(df -m /var/mobile | awk 'NR == 2 { print $4 }') MB"
cat "$XJ/VERSION" 2>/dev/null

step 'PostgreSQL'
if pg_ready; then good '在运行'; else no "没在运行，看 $LOGS/postgres.log 最后几行："; tail -20 "$LOGS/postgres.log"; fi
if pg_ready; then
    psql_super 'SELECT version()'
    if psql_super 'DROP TABLE IF EXISTS selftest; CREATE TABLE selftest AS SELECT g AS id, md5(g::text) AS v FROM generate_series(1, 200000) g; SELECT count(*) FROM selftest' >/dev/null; then
        good "写入、读出 20 万行"
    else no '读写失败'; fi
    psql_super 'DROP TABLE IF EXISTS selftest' >/dev/null 2>&1

    # 云盘程序 + 建表：用一个临时库，测完删掉
    load_env
    dbpw=$(echo "$XIAOJU_DATABASE_URL" | sed -n 's#^postgres://xiaoju:\([0-9a-f]*\)@.*#\1#p')
    psql_super 'DROP DATABASE IF EXISTS xiaoju_selftest' >/dev/null
    psql_super 'CREATE DATABASE xiaoju_selftest OWNER xiaoju' >/dev/null
    if XIAOJU_DATABASE_URL="postgres://xiaoju:$dbpw@127.0.0.1:$PGPORT/xiaoju_selftest?sslmode=disable" \
        "$BIN/xiaoju" migrate 2>&1 | tail -1 | grep -q 'migrate up'; then
        good "云盘程序能运行，建好了 $(psql_super "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'" xiaoju_selftest) 张表"
    else
        no '云盘程序跑不起来：'
        XIAOJU_DATABASE_URL="postgres://xiaoju:$dbpw@127.0.0.1:$PGPORT/xiaoju_selftest?sslmode=disable" "$BIN/xiaoju" migrate 2>&1 | tail -5
    fi
    psql_super 'DROP DATABASE IF EXISTS xiaoju_selftest' >/dev/null

    # 强杀主进程，看 launchd 会不会拉起来、数据库会不会自己恢复（云盘已经在用了就不测）
    if daemon_installed com.xiaoju.server; then
        echo '（云盘已经在用，跳过强杀测试）'
    else
        pid=$(daemon_pid com.xiaoju.postgres)
        if [ -n "$pid" ]; then
            kill -9 "$pid"
            sleep 3
            if wait_for 60 pg_ready; then good '强杀后自己起来了（崩溃恢复正常）'; else no "强杀后没起来，看 $LOGS/postgres.log"; fi
        else no 'launchctl 里找不到 PostgreSQL 的进程号'; fi
    fi
fi

step '其它程序'
if command -v ffmpeg >/dev/null 2>&1; then good "ffmpeg（视频缩略图）：$(ffmpeg -version 2>/dev/null | head -1)"; else bad '没有 ffmpeg：视频没有缩略图（不影响别的）'; fi
echo '照片缩略图：iPhone 上没有 vips，HEIC 照片暂时出不了新缩略图（旧的不受影响），JPEG/PNG 用 ffmpeg 做。'
command -v sshd >/dev/null 2>&1 || [ -x "$JB/usr/sbin/sshd" ] && good 'openssh（电脑往手机拷文件用）' || bad '没有 openssh'

step 'Tailscale'
if [ -n "$(daemon_pid com.xiaoju.tailscaled)" ]; then good 'tailscaled 在运行'; else no "tailscaled 没在运行，看 $LOGS/tailscaled.log"; fi
st=$("$BIN/tailscale" --socket="$TS_SOCK" status --json 2>/dev/null)
name=$(printf '%s' "$st" | tr ',' '\n' | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -1)
if printf '%s' "$st" | grep -q '"BackendState": *"Running"'; then
    good "已登录：$name"
    if "$BIN/tailscale" --socket="$TS_SOCK" funnel --bg "text:xiaoju-7p-ok" >/dev/null 2>&1; then
        good "Funnel 测试页开好了，用别的设备（关掉 Wi-Fi 用流量更准）打开：https://$name/"
        echo "   看到 xiaoju-7p-ok 就对了。测完关掉：sh $SCRIPTS/ts.sh funnel reset"
    else
        no "Funnel 开不了：sh $SCRIPTS/ts.sh funnel --bg text:ok 看报错（可能要在 Tailscale 后台给这台设备开 Funnel 权限）"
    fi
else
    bad "还没登录：sh $SCRIPTS/ts.sh up --hostname=node-7p"
fi

echo
if [ "$fail" = 0 ]; then echo '自检通过。'; else echo '有 [坏] 的项，把上面整段发给 Claude。'; fi
exit "$fail"
