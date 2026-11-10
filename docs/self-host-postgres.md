# 小橘云盘：自建 PostgreSQL 设计要求

## 背景
- 云盘服务（langhua98/xiaoju-cloud，Go）现在跑在 Hugging Face Space，数据库用 Neon 免费版。
- 2026-11-10 Neon 免费额度用完，服务启动时连不上库就退出，云盘全部 503。
- 文件内容存在 Telegram 频道，不在数据库里；数据库存文件名、目录、令牌、分块索引等元数据。
- 数据库每天自动加密备份，置顶在 Telegram 主频道和备份频道。

## 目标
在北卡家里一台常开的电脑上跑 PostgreSQL，替代 Neon，并恢复数据。

## 数据库要求
- PostgreSQL 16 或 17（项目没有用任何扩展，标准版就行）。
- 一个专用数据库和用户，比如库名 `xiaoju`、用户 `xiaoju`，用强密码。不要用 postgres 超级用户给云盘连。
- 表由云盘自己建：服务启动时自动执行 `migrations/` 里的迁移（目前到 0019），不用手动建表。
- 连接串通过环境变量 `XIAOJU_DATABASE_URL` 给云盘，格式：
  `postgres://xiaoju:<密码>@<主机>:<端口>/xiaoju?sslmode=require`
- 数据放在有空间的磁盘上，做好本机定期备份（`pg_dump` 每天一次即可，云盘自己也会往 Telegram 备份）。

## 电脑要求
- 24 小时开机，关掉系统睡眠或休眠；断电后能自动开机、自动启动 PostgreSQL（设为系统服务）。
- 稳定的家庭网络。

## 网络：两种方案选一种

### 方案 A（推荐）：整个云盘服务也搬到这台电脑
- 云盘服务和 PostgreSQL 都在本机，数据库只监听 `127.0.0.1`，不对外。连接串用 `localhost`，可以 `sslmode=disable`。
- 用 Cloudflare Tunnel（或 Tailscale Funnel）把云盘的 HTTP 端口开到公网，不在路由器上开端口。
- 手机遥控要用 WebSocket（`/api/agent/v1/relay/ws`、视频流），隧道要支持 WebSocket（Cloudflare Tunnel 默认支持），而且空闲连接不能太快被掐（手机心跳最长约 9 分钟一次）。
- 云盘原来在 Hugging Face 上的环境变量和 secrets 都要搬过来（尤其是主密钥 `XIAOJU_MASTER_KEY` 和 Telegram 相关的那些），名字照 Hugging Face Space 设置页里的抄。主密钥丢了，备份和文件都解不开。
- 网址变了的话，手机 App、小橘推送、网页里配的云盘地址都要改。

### 方案 B：云盘留在 Hugging Face，只把数据库放家里
- Hugging Face 要能从公网连进 PostgreSQL，所以需要：
  - 路由器转发一个非默认端口到 5432（或者用支持 TCP 的隧道）；
  - PostgreSQL 开 SSL（`ssl = on`），`pg_hba.conf` 只允许 `xiaoju` 用户用 `scram-sha-256` + SSL 连接；
  - 家里公网 IP 会变的话，配一个动态域名（DDNS）。
- 把 Hugging Face Space 里的 secret `XIAOJU_DATABASE_URL` 换成新的连接串，然后重启 Space。
- 缺点：数据库直接暴露在公网，要盯紧安全。

## 恢复数据（两种方案都要做）
1. 建好空库和用户（不要提前建表）。
2. 用云盘自带的命令，从 Telegram 置顶备份恢复。环境变量要和线上一样（`XIAOJU_DATABASE_URL` 指向新库，加上主密钥和 Telegram 相关变量）：
   - `xiaoju dbrestore --check`：只下载、解密、检查，不写库，先跑这个确认备份完好；
   - `xiaoju dbrestore`：恢复进空库（库里已有表会拒绝，不会覆盖）。
3. 启动云盘，确认能登录、能看到文件。
4. 会丢最后一次每日备份之后的改动（最多约 24 小时）。

## 完成后的检查
- 网页能登录，文件列表正常，能下载一个旧文件。
- `/api/agent/v1/relay` 能收发（手机遥控能用）。
- 重启电脑后，数据库和云盘都能自动起来。

具体做法（Windows，方案 A）：[deploy/windows/README.md](../deploy/windows/README.md)。
