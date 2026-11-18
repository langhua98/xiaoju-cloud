# 把小橘云盘搬到越狱 iPhone（iPhone 7 Plus）

视频走了 Hugging Face 中转（Telegram → HF → 手机，见 `deploy/windows/README.md`「视频中转」），
家里这台机器只管索引和元数据，用不着一台电脑常开。这里把**云盘 + PostgreSQL + Tailscale 都装到一台越狱的旧 iPhone 上**，
地址还是 `https://node.tail823dbd.ts.net`，所有 App 都不用改。

```
手机 / 网页 ──HTTPS──▶ Tailscale Funnel（node.tail823dbd.ts.net）──▶ iPhone 7 Plus 127.0.0.1:8080（xiaoju）
                                                                     ├─▶ 127.0.0.1:5432 PostgreSQL 17（元数据）
                                                                     └─▶ Telegram 频道（文件内容，不变）
```

## 这台手机能不能行

| 东西 | 情况 |
|---|---|
| 越狱 | iPhone 7 Plus（A10，arm64）：iOS 12–14 用 checkra1n，iOS 15 用 palera1n。rootful、rootless 都支持。都是**半不完美**越狱：重启后要重新越狱一次，云盘才会起来。 |
| 云盘程序 | Go 编成 iOS 版（GOOS=ios，要 Mac 的 SDK，所以在 GitHub 的 Mac 机器上编）。 |
| PostgreSQL | Sileo/Procursus 里**没有**，自己编。iOS 内核没有 SysV 共享内存（一调用进程就被杀），打了补丁 `postgres-ios.patch` 只用 mmap，信号量用命名 POSIX。补丁在 Linux 上验证过：零 SysV 调用、读写、强杀后崩溃恢复都正常。**真机上还没跑过**，先跑自检。 |
| 地址 | 手机上跑 Tailscale 的 `tailscaled`（用户态网络，不占 VPN），Funnel 开到公网。电脑在 Tailscale 后台改个名，手机改名成 `node`，地址就还是原来的。 |
| 缩略图 | 视频：ffmpeg（Sileo 里有）。照片：iPhone 上没有 vips，**新拍的 HEIC 照片暂时没有缩略图**（旧的不受影响，JPEG/PNG 正常）。 |
| 内存 | 3 GB。PostgreSQL 调成 64 MB 共享缓存、最多 30 个连接；云盘 GOMEMLIMIT 400 MiB。 |

所有东西装在 `/var/mobile/xiaoju`：

| 目录 | 放什么 |
|---|---|
| `bin/` | `xiaoju`、`tailscaled`、`tailscale` |
| `pgsql/` | PostgreSQL 程序 |
| `pgdata/` | 数据库 |
| `config/xiaoju.env` | 环境变量（主密钥、Telegram 令牌），只有 mobile 能读 |
| `data/` | Telegram 会话文件 |
| `ts/` | Tailscale 登录状态 |
| `backups/` | 每天 3:40 的 `pg_dump`，保留 7 天 |
| `logs/` | `xiaoju.log`、`postgres.log`、`tailscaled.log`、`backup.log` |
| `scripts/` | 这些脚本 |

四个 launchd 任务（越狱后自动加载，挂了 10 秒后自动拉起）：`com.xiaoju.postgres`、`com.xiaoju.server`、`com.xiaoju.tailscaled`、`com.xiaoju.backup`。

## 电池（重要）

一直插着充电的老手机，电池会鼓包。鼓包会顶坏屏幕，严重的会起火。

- **限制充电到 80%**：Sileo 里装 **ChargeLimiter**（作者 lich4，支持越狱的 iOS 12–17），设成 20%–80%，或者「到 80% 停充、低于 40% 再充」。这一条最管用。
- 或者用带定时的智能插座，每天只通电几小时。
- 设置 → 电池 → 电池健康：打开「优化电池充电」；关掉「低电量模式」。
- **散热**：摘掉手机壳，竖着放或架空，别放在被子、沙发、路由器上面，别晒太阳。用原装或正规的 5V 1A 充电头就够了，别用快充。
- 设置 → 显示与亮度 → 自动锁定：**永不**（锁屏后 iOS 会让后台程序睡眠）。亮度调到最低。7 Plus 是 LCD 屏，不会烧屏。
- 每周看一眼：屏幕边缘翘起、手机放桌上转圈、后盖鼓起，**马上拔电、关机**，放在不可燃的地方，别再充电。
- 长期打算：电池拆掉换成假电池 + 直接供电，不会再鼓包（要动手拆机，以后再说）。

## 0. 先准备

1. 手机越狱好（checkra1n 或 palera1n），装好 Sileo。
2. Sileo 里装 **NewTerm**（手机上的终端）。打开 NewTerm，变成 root：
   ```sh
   sudo -i
   ```
   （rootful 越狱用 `su`，默认密码 `alpine`，装好后用 `passwd` 和 `passwd mobile` 改掉。）
3. 设置 → 无线局域网：连家里的 Wi-Fi；设置 → 显示与亮度 → 自动锁定：永不。

## 1. 安装

NewTerm 里（root）：

```sh
apt-get install -y curl
curl -fsSLo /var/mobile/install.sh https://github.com/langhua98/xiaoju-cloud/releases/download/ios-jailbreak-latest/install.sh
sh /var/mobile/install.sh
```

它会：装 curl、ffmpeg、openssh、ldid → 下载 Release「ios-jailbreak-latest」的包（校验 SHA-256）→ 建好空数据库 →
写 `xiaoju.env` → 注册自动运行的任务。**不会启动云盘**（数据还没恢复）。可以重复运行，数据和配置都保留。

## 2. 自检（不影响电脑上的云盘）

```sh
sh /var/mobile/xiaoju/scripts/ts.sh up --hostname=node-7p
```

打出一个链接，在任意设备的浏览器里打开，用你的 Tailscale 账号登录。然后：

```sh
sh /var/mobile/xiaoju/scripts/selftest.sh
```

测 PostgreSQL 读写、强杀后自动恢复、云盘程序建表、ffmpeg、Tailscale，最后用 Funnel 开一个测试页
`https://node-7p.tail823dbd.ts.net/`，在别的设备上打开，看到 `xiaoju-7p-ok` 就对了。
全是 `[好]` 就能搬；有 `[坏]` 把整段输出发给 Claude。测完关掉测试页：`sh /var/mobile/xiaoju/scripts/ts.sh funnel reset`。

建议自检通过后让手机就这样插着放一两天，看会不会被系统杀掉、会不会发烫。

## 3. 正式搬家（停机大约 15 分钟）

挑没人用的时候。电脑上开管理员 PowerShell：

1. 下载搬家脚本：
   ```powershell
   iwr https://github.com/langhua98/xiaoju-cloud/releases/download/ios-jailbreak-latest/move-out.ps1 -OutFile C:\xiaoju\scripts\move-out.ps1
   ```
2. 停掉电脑上的云盘，导出完整备份（一条数据都不丢）：
   ```powershell
   powershell -ExecutionPolicy Bypass -File C:\xiaoju\scripts\move-out.ps1 -Final -Phone node-7p
   ```
3. 照它最后打出的 `scp …` 命令拷到手机（密码是手机 mobile 用户的密码）。

手机上（root）：

4. ```sh
   sh /var/mobile/xiaoju/scripts/import-from-pc.sh
   sh /var/mobile/xiaoju/scripts/restore.sh /var/mobile/xiaoju/inbox/move/move.xjdb
   ```
   最后显示「云盘已启动」就对了。

换地址（让 `node.tail823dbd.ts.net` 指向手机）：

5. 电脑上：`tailscale funnel reset`（关掉电脑的 Funnel）。
6. 打开 <https://login.tailscale.com/admin/machines>，找到电脑那台 `node` → 右边 `…` → Edit machine name，改成 `node-pc`。
7. 手机上：
   ```sh
   sh /var/mobile/xiaoju/scripts/ts.sh set --hostname=node
   sh /var/mobile/xiaoju/scripts/ts.sh funnel --bg 8080
   ```
   如果后台里手机的名字没变成 `node`，在后台把它手动改成 `node`。
8. 用手机流量打开 `https://node.tail823dbd.ts.net/healthz`，显示 `ok` 就搬完了。

电脑可以关了。电脑上的 Tailscale 先留着（叫 `node-pc`），想搬回来时用得上。

### 搬回电脑

手机上：`launchctl unload /var/jb/Library/LaunchDaemons/com.xiaoju.server.plist`（rootful 去掉 `/var/jb`）。
手机上这段时间的新数据：手机上 `xiaoju dbbackup` 导出，电脑上建空库后 `restore.ps1 -File` 恢复。
然后电脑上 `move-out.ps1 -Undo`，Tailscale 后台两台换回名字，电脑上 `tailscale funnel --bg 8080`。

## 4. 检查

```sh
sh /var/mobile/xiaoju/scripts/check.sh
```

- [ ] 网页能登录，文件列表正常，能下载一个旧文件，照片有缩略图。
- [ ] 小橘推送、法莉娅 App 能连上（地址没变，不用改）。
- [ ] 看视频走中转（xiaoju.env 里有 `XIAOJU_RELAY_URL`，是从电脑拷过来的）。
- [ ] 第二天 `backups/` 里有备份；Telegram 频道里有新的置顶备份。

## 日常

| 要做什么 | 命令（NewTerm 里 root，都在 `/var/mobile/xiaoju/scripts`） |
|---|---|
| 看状态 | `sh check.sh` |
| 更新到最新版 | `sh update.sh`（会打断正在进行的上传） |
| 改了 `xiaoju.env` 后重启 | `sh update.sh --restart` |
| 马上备份一次 | `sh backup.sh` |
| 手机重启了 | 重新越狱（checkra1n / palera1n），越狱完所有任务自己起来 |

## 实在不行的备选

PostgreSQL 在这台手机上跑不起来（自检第一项就 `[坏]`）的话：一台旧安卓手机装 Termux，`pkg install postgresql` 是现成的，
云盘程序编成 `GOOS=android` 就行，步骤和这里几乎一样。
