# 把小橘云盘搬到家里的 Windows 电脑（方案 A）

2026-11-10 Neon 免费额度用完，云盘全部 503。这里把**云盘服务和 PostgreSQL 都装在家里一台常开的 Windows 电脑上**，
数据库只监听本机，用隧道把云盘开到公网，路由器上不开端口。

```
手机 / 网页 ──HTTPS──▶ Cloudflare Tunnel 或 Tailscale Funnel ──▶ 家里电脑 127.0.0.1:8080（xiaoju.exe）
                                                                     ├─▶ 127.0.0.1:5432 PostgreSQL 17（元数据）
                                                                     └─▶ Telegram 频道（文件内容，不变）
```

所有东西装在 `C:\xiaoju`：

| 目录 | 放什么 |
|---|---|
| `bin\xiaoju.exe` | 云盘程序，GitHub Release「windows-latest」里下载（main 每次改 Go 代码会自动重新编译，`.github/workflows/windows.yml`） |
| `config\xiaoju.env` | 环境变量（主密钥、Telegram 令牌、数据库密码），只有管理员和 SYSTEM 能读 |
| `config\age-key.txt` | 导入密钥用的私钥 |
| `pgdata\` | PostgreSQL 数据（`install.ps1 -PgData D:\xiaoju-pgdata` 可以放到别的盘） |
| `backups\` | 每天 3:40 的 `pg_dump`，保留 14 天 |
| `logs\` | 云盘日志 `xiaoju-日期.log`（保留 14 天）、`backup.log` |
| `scripts\` | 这些脚本 |

开机后两个计划任务（以 SYSTEM 运行，不用登录）：`xiaoju-cloud`（云盘，挂了 10 秒后自动重启）、`xiaoju-pg-backup`（每日备份）。
PostgreSQL 是系统服务 `postgresql-x64-17`，自动启动。

## 0. 先准备

- 电脑：Windows 10/11 64 位，C 盘（或 `-PgData` 那块盘）留 10 GB 以上。
- **断电后自动开机**：开机进 BIOS，找 “Restore on AC Power Loss” / “AC Back” / “断电恢复”，设成 **Power On**。（这个脚本改不了）
- 设置 → Windows 更新 → 活动时间：设一个大范围，免得白天自动重启（重启了也会自己起来，只是会断一会儿）。
- 用网线比 Wi-Fi 稳。

## 1. 安装

1. 在电脑上下载这个仓库：<https://github.com/langhua98/xiaoju-cloud> → Code → Download ZIP，解压。
2. 开始菜单右键 → 「终端（管理员）」或「Windows PowerShell（管理员）」，运行：
   ```powershell
   cd <解压的目录>\deploy\windows
   powershell -ExecutionPolicy Bypass -File install.ps1
   ```
   它会：关掉睡眠和休眠 → 装 PostgreSQL 17（只监听本机）→ 建空库 `xiaoju` 和用户 `xiaoju`（随机强密码）→
   下载 age、ffmpeg、vips（照片和视频缩略图用）和 `xiaoju.exe` → 写 `xiaoju.env` → 生成密钥对 → 注册开机任务。
   **不会启动云盘**：库里一旦有表就没法恢复了，要等第 3 步恢复完。
3. 最后会打出一行 `age1…` 开头的**公钥**（也存在 `C:\xiaoju\age-public-key.txt`），下一步要用。

可以重复运行，装过的会跳过。

## 2. 把 GitHub 上的密钥导过来

主密钥和 Telegram 令牌都存在 GitHub 仓库的 Secrets 里（Hugging Face 设置页里看不到值）。用工作流加密导出，只有这台电脑能解开：

1. 打开 <https://github.com/langhua98/xiaoju-cloud/actions/workflows/export-secrets.yml> → **Run workflow**，
   `age_public_key` 粘贴上一步的公钥，运行。
2. 跑完后点进这次运行，页面底部 Artifacts 下载 **secrets**（一个 zip，1 天后自动删除）。
3. 导入：
   ```powershell
   powershell -ExecutionPolicy Bypass -File C:\xiaoju\scripts\import-secrets.ps1 $HOME\Downloads\secrets.zip
   ```
   数据库地址不导入（Neon 那个不要了），用本机的。

> 你自己另外保存过主密钥的话，也可以直接编辑 `C:\xiaoju\config\xiaoju.env` 填进去（名字和 GitHub Secrets 一样）。
> **主密钥丢了，备份和文件都解不开。**

## 3. 恢复数据

```powershell
powershell -ExecutionPolicy Bypass -File C:\xiaoju\scripts\restore.ps1 -Check   # 只下载、解密、检查，不写库
powershell -ExecutionPolicy Bypass -File C:\xiaoju\scripts\restore.ps1          # 正式恢复，然后启用开机任务、启动云盘
```

从 Telegram 主频道（不行就用备份频道）置顶的每日备份恢复。最后一次备份之后的改动会丢（最多约 24 小时）。
恢复完在电脑浏览器打开 <http://127.0.0.1:8080/healthz>，显示 `ok` 就对了。

## 4. 开到公网：选一种隧道

两种都支持 WebSocket（手机遥控的 `/api/agent/v1/relay/ws`、视频流），不用在路由器上开端口。
被控端的心跳间隔是自己摸出来的，隧道掐空闲连接掐得早也只是心跳勤一点。

### 4a. 有自己的域名托管在 Cloudflare：Cloudflare Tunnel

1. `winget install --id Cloudflare.cloudflared`
2. <https://one.dash.cloudflare.com> → Networks → Tunnels → Create a tunnel → Cloudflared，起名 `xiaoju`。
   页面给的命令里选 Windows，复制 `cloudflared.exe service install <一长串令牌>`，在管理员 PowerShell 里运行（装成开机自启的服务）。
3. 下一步 Public Hostname：子域名比如 `pan`，域名选你的，Service 填 **HTTP** `127.0.0.1:8080`。保存。
4. 新地址就是 `https://pan.<你的域名>`。

注意 Cloudflare 免费版**一个请求最多 100 MB**。网页上传是 8 MB 一段，没问题；别的客户端一次传超过 100 MB 的大文件会失败。

### 4b. 没有域名：Tailscale Funnel（免费，地址固定）

1. `winget install --id Tailscale.Tailscale`，装完登录（浏览器里点一下）。
2. 管理员 PowerShell：`tailscale funnel --bg 8080`。第一次会给一个链接，打开后同意开启 HTTPS 和 Funnel，再运行一次。
   `--bg` 会记住设置，重启后自动恢复。
3. 新地址是 `https://<电脑名>.<你的 tailnet>.ts.net`（`tailscale funnel status` 能看到）。

## 5. 改云盘地址

旧地址 `https://langhua1998-xiaoju-cloud.hf.space` 写死在这些地方（换成第 4 步的新地址；现在用的是 `https://node.tail823dbd.ts.net`，本仓库的 `web/config.js`、`ios/project.yml` 已经改了）：

- 网页：本仓库 `web/config.js`，或 GitHub 仓库变量 `XIAOJU_API_URL`，然后重新跑 “Deploy web to GitHub Pages”。
  网页还是在 GitHub Pages 上，`XIAOJU_CORS_ORIGINS`、刷脸（通行密钥，绑的是网页域名）都不用改。
- 小橘推送 iOS：`langhua98/meta-work` 的 `src/xiaoju-push/App/Store.swift`（App 里也能直接改服务端地址）。
- 小橘遥控被控端：`langhua98/xiaoju-remote` 的 `App/Store.swift`、`android/.../Prefs.kt`（App 里也能改）。
- 电脑脚本：两个仓库的 `scripts/遥控.py`、`scripts/upload_package.py` 等。
- 谷歌云盘同步（用了的话）：Google Cloud Console 里 OAuth 客户端的重定向地址改成 `<新地址>/gdrive/callback`，
  `C:\xiaoju\config\xiaoju.env` 里 `XIAOJU_PUBLIC_URL` 改成新地址，`update.ps1 -RestartOnly`。

## 6. 停掉 Hugging Face 上的旧服务（重要）

Neon 的免费额度下个月 1 号会恢复。到时候 Hugging Face 上的旧云盘又能连上 Neon 的**旧数据**，
和家里的同时跑、同时往 Telegram 写，还会把旧库的备份置顶到频道，下次恢复就恢复成旧的了。所以搬完一定要：

1. GitHub 仓库 Settings → Secrets and variables → Actions → **Variables** 加 `HF_DEPLOY` = `off`（以后推送不再部署到 Hugging Face）。
2. Hugging Face Space 设置页 → **Pause**（或者删掉 Space）。
3. 可选：Secrets 里删掉 `XIAOJU_DATABASE_URL`（Neon 的），Neon 项目也可以删。

## 7. 检查

```powershell
powershell -ExecutionPolicy Bypass -File C:\xiaoju\scripts\check.ps1
```

- [ ] 网页能登录，文件列表正常，能下载一个旧文件，照片有缩略图。
- [ ] 手机遥控能用（`/api/agent/v1/relay` 收发、常驻连接）。
- [ ] 重启电脑，不登录，等两分钟，用手机打开云盘：数据库、云盘、隧道都自己起来了。
- [ ] 第二天 `C:\xiaoju\backups` 里有备份；Telegram 频道里有新的置顶备份。

## 日常

| 要做什么 | 命令（管理员 PowerShell，都在 `C:\xiaoju\scripts`） |
|---|---|
| 看状态 | `check.ps1` |
| 更新到最新版 | `update.ps1`（会打断正在进行的上传，挑没人用的时候） |
| 改了 `xiaoju.env` 后重启 | `update.ps1 -RestartOnly` |
| 马上备份一次 | `backup.ps1` |
| 用本机备份恢复 | `pg_restore -h 127.0.0.1 -U xiaoju -d xiaoju --clean --if-exists C:\xiaoju\backups\<文件>.dump`（先停云盘） |

运行脚本都用 `powershell -ExecutionPolicy Bypass -File <脚本>`。
