# 几个脚本共用的路径和小工具。其它脚本开头用 `. "$(dirname "$0")/common.sh"` 引进来。
# 只用 POSIX sh 语法（rootless 越狱没有 /bin/sh，脚本都用 `sh 脚本` 的方式运行）。

XJ=/var/mobile/xiaoju            # 全部东西都在这里，rootful / rootless 一样
BIN=$XJ/bin
PG=$XJ/pgsql
PGDATA=$XJ/pgdata
CONFIG=$XJ/config
ENV_FILE=$CONFIG/xiaoju.env
DATA=$XJ/data                    # Telegram 会话文件
TMPD=$XJ/tmp
LOGS=$XJ/logs
BACKUPS=$XJ/backups              # 每天的 pg_dump
RUN=$XJ/run                      # PostgreSQL 和 tailscaled 的 socket
TS_STATE=$XJ/ts                  # Tailscale 的登录状态（root 才能读）
INBOX=$XJ/inbox                  # 从电脑拷过来的文件放这里
SCRIPTS=$XJ/scripts
PGPORT=5432
TS_SOCK=$RUN/tailscaled.sock

REPO=langhua98/xiaoju-cloud
RELEASE_TAG=ios-jailbreak-latest
BUNDLE=xiaoju-ios-arm64.tar.gz

# rootless（palera1n 默认、Dopamine）东西在 /var/jb 下；rootful（checkra1n、palera1n -f）在 / 下
if [ -x /var/jb/usr/bin/apt-get ] || [ -x /var/jb/bin/sh ]; then JB=/var/jb; else JB=; fi
SH=$JB/bin/sh
[ -x "$SH" ] || SH=$JB/usr/bin/sh
DAEMONS=$JB/Library/LaunchDaemons
LAUNCHCTL=$(command -v launchctl 2>/dev/null || echo "$JB/usr/bin/launchctl")
export PATH="$PG/bin:$BIN:$JB/usr/local/bin:$JB/usr/bin:$JB/bin:$JB/usr/sbin:$JB/sbin:/usr/bin:/bin:/usr/sbin:/sbin"

LABELS="com.xiaoju.postgres com.xiaoju.tailscaled com.xiaoju.server com.xiaoju.backup"

step() { printf '\n== %s\n' "$*"; }
die() { printf '出错：%s\n' "$*" >&2; exit 1; }
good() { printf '[好] %s\n' "$*"; }
bad() { printf '[坏] %s\n' "$*"; }

need_root() { [ "$(id -u)" = 0 ] || die "要用 root 运行：先 sudo -i（或 su，默认密码 alpine），再运行这个脚本。"; }

new_secret() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
    else openssl dgst -sha256 "$1" | sed 's/.*= //'; fi
}

# 读 KEY=VALUE 文件里的一项（# 开头是注释）
env_get() { [ -f "$2" ] && sed -n "s/^$1=//p" "$2" | tail -1 | tr -d '\r'; }

# 改/加一项（值里不能有换行）
env_set() {
    f=$3
    touch "$f"
    grep -v "^$1=" "$f" >"$f.tmp" || true
    printf '%s=%s\n' "$1" "$2" >>"$f.tmp"
    mv "$f.tmp" "$f"
}

# 把 xiaoju.env 加载进当前进程（set -a 让变量都 export 给子进程）
load_env() {
    [ -f "$ENV_FILE" ] || die "没找到 $ENV_FILE，先运行 install.sh。"
    # 不用 `. 文件`：值里可能有 & ; $ 空格之类，原样导出
    while IFS= read -r line || [ -n "$line" ]; do
        case $line in ''|'#'*) continue ;; esac
        k=${line%%=*}
        case $k in *[!A-Za-z0-9_]*|'') continue ;; esac
        export "$k=${line#*=}"
    done <"$ENV_FILE"
}

psql_super() { "$PG/bin/psql" -h "$RUN" -p "$PGPORT" -U postgres -d "${2:-postgres}" -v ON_ERROR_STOP=1 -tAc "$1"; }

pg_ready() { "$PG/bin/pg_isready" -q -h "$RUN" -p "$PGPORT" -t 3; }

health() { curl -fsS -m 5 http://127.0.0.1:8080/healthz >/dev/null 2>&1; }

ts() { "$BIN/tailscale" --socket="$TS_SOCK" "$@"; }

# 装 / 卸 / 重启 launchd 任务
daemon_plist() { echo "$DAEMONS/$1.plist"; }
daemon_install() {
    sed -e "s#@SH@#$SH#g" -e "s#@XJ@#$XJ#g" "$XJ/launchd/$1.plist" >"$(daemon_plist "$1")"
    chown root:wheel "$(daemon_plist "$1")"
    chmod 644 "$(daemon_plist "$1")"
    "$LAUNCHCTL" unload "$(daemon_plist "$1")" 2>/dev/null || true
    "$LAUNCHCTL" load -w "$(daemon_plist "$1")"
}
daemon_remove() {
    [ -f "$(daemon_plist "$1")" ] || return 0
    "$LAUNCHCTL" unload "$(daemon_plist "$1")" 2>/dev/null || true
    rm -f "$(daemon_plist "$1")"
}
daemon_installed() { [ -f "$(daemon_plist "$1")" ]; }
daemon_restart() {
    daemon_installed "$1" || return 0
    "$LAUNCHCTL" unload "$(daemon_plist "$1")" 2>/dev/null || true
    "$LAUNCHCTL" load -w "$(daemon_plist "$1")"
}
# launchctl list 里这个任务的 PID（没在跑时为空）
daemon_pid() { "$LAUNCHCTL" list 2>/dev/null | awk -v l="$1" '$3 == l && $1 != "-" { print $1 }'; }

wait_for() { # 秒数 命令…
    n=$1; shift
    while [ "$n" -gt 0 ]; do
        "$@" && return 0
        sleep 2; n=$((n - 2))
    done
    return 1
}
