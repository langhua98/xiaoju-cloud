# 把电脑上 move-out.ps1 打包、拷到手机 inbox/ 的东西装好：密钥（xiaoju.env）、Telegram 会话文件。要 root。
#   sh import-from-pc.sh            默认读 /var/mobile/xiaoju/inbox/move
#   sh import-from-pc.sh 目录
# 本机专用的几项（数据库地址、监听地址、临时目录、会话文件路径）保留手机上的值。
. "$(dirname "$0")/common.sh"
need_root
src=${1:-$INBOX/move}
[ -f "$src/xiaoju.env" ] || die "$src 里没有 xiaoju.env。先在电脑上运行 move-out.ps1，再按它最后打出的命令拷过来。"
[ -f "$ENV_FILE" ] || die '先运行 install.sh。'

keep='XIAOJU_DATABASE_URL XIAOJU_HTTP_ADDR XIAOJU_STORAGE XIAOJU_TMP_DIR TG_SESSION_FILE TG_BACKUP_SESSION_FILE'
n=0
# Windows 写的文件：去掉 UTF-8 BOM 和行尾的 \r
sed -e '1s/^\xEF\xBB\xBF//' -e 's/\r$//' "$src/xiaoju.env" >"$src/.env.unix"
while IFS= read -r line; do
    case $line in ''|'#'*) continue ;; esac
    k=${line%%=*}; v=${line#*=}
    [ -n "$k" ] && [ "$k" != "$line" ] || continue
    case " $keep " in *" $k "*) continue ;; esac
    [ -n "$v" ] || continue
    case $v in [A-Za-z]:\\*) echo "跳过 $k（是电脑上的路径 $v）"; continue ;; esac
    env_set "$k" "$v" "$ENV_FILE"
    n=$((n + 1))
done <"$src/.env.unix"
rm -f "$src/.env.unix"
chown mobile:mobile "$ENV_FILE"; chmod 600 "$ENV_FILE"
echo "写入了 $n 项。"
[ -n "$(env_get XIAOJU_MASTER_KEY "$ENV_FILE")" ] || bad '没有 XIAOJU_MASTER_KEY！没有主密钥，备份和文件都解不开。'

for f in tg-session.json tg-backup-session.json; do
    if [ -f "$src/$f" ]; then
        cp "$src/$f" "$DATA/$f"; chown mobile:mobile "$DATA/$f"; chmod 600 "$DATA/$f"
        echo "拷好了 $f"
    fi
done
[ -f "$src/move.xjdb" ] && echo "数据库备份在 $src/move.xjdb，下一步：sh $SCRIPTS/restore.sh $src/move.xjdb"
