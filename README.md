# xiaoju-cloud

以 **Telegram 私有频道** 作为存储后端的自托管云盘（MVP 阶段）。

- 文件按固定大小切块，每块经 **AES-256-CTR 加密**后作为一条文档消息存进私有频道；
- 目录树、文件名、分块与消息的映射全部存在 **PostgreSQL**，频道里只能看到加密后的 `<uuid>.partNNNN`；
- 走 **MTProto**（[gotd/td](https://github.com/gotd/td)），单块最大 2GB，可按偏移读取，支持 HTTP Range（断点续传、视频拖动）；
- 存储驱动可插拔：`telegram`（生产）/ `local`（本地开发，无需 Telegram）。

## 架构

```
客户端 ──HTTP──▶ api ──▶ service ──▶ storage.Driver ──▶ Telegram 私有频道 / 本地磁盘
                          │  (切块 + 加密 + Range 解密)
                          └──▶ meta ──▶ PostgreSQL（目录树、分块映射、包裹后的文件密钥）
```

| 目录 | 说明 |
|---|---|
| `cmd/xiaoju` | 入口：`xiaoju serve` / `xiaoju migrate [up\|down]` |
| `migrations/` | SQL 迁移脚本（内嵌进二进制，`serve` 启动时自动执行） |
| `internal/config` | 环境变量配置 |
| `internal/db` | 连接池与迁移执行器 |
| `internal/meta` | 元数据仓储（users / files / file_parts） |
| `internal/crypto` | 文件密钥包裹（AES-GCM）与可从任意偏移解密的 AES-CTR |
| `internal/service` | 上传切块加密、可 Seek 的解密读取器、删除 |
| `internal/storage` | 存储驱动接口；`telegram/` MTProto 驱动，`local/` 本地驱动 |
| `internal/api` | HTTP API |
| `web/` | 网页前端（纯静态，部署到 GitHub Pages） |

### 加密

- 每个文件随机生成 32 字节 DEK，用主密钥 `XIAOJU_MASTER_KEY` 以 AES-GCM 包裹后存进 `files.dek_wrapped`；
- 每个分块使用独立随机 IV 的 AES-256-CTR，因此可以从任意字节偏移解密，Range 请求只需下载需要的部分。
- **主密钥丢失 = 所有文件无法解密**，请单独、安全地备份。

### Telegram 驱动要点

- 上传：`upload.saveBigFilePart`（512KB 分片，4 并发）→ `messages.sendMedia` 发到频道，记录 `message_id`；
- 下载：`channels.getMessages` 拿到文档位置（缓存 30 分钟），`upload.getFile` 按 1MB 对齐读取；`FILE_REFERENCE_EXPIRED` 时自动刷新重试；
- `FLOOD_WAIT_X`：中间件自动等待并重试（最多 3 次，单次最长 1 分钟）；
- 连接方式 `TG_TRANSPORT`：`tcp` 直连数据中心 IP（不依赖 DNS，Hugging Face Spaces 上只能用这个），或 `websocket` 经 `*.web.telegram.org:443`；启动 90 秒内连不上会报错退出；
- 每条消息的说明里写入 `{"f": 文件ID, "p": 分块号, "sha256": ...}`，作为数据库丢失时的兜底线索。

## 快速开始

### 1. 准备 Telegram

1. 在 <https://my.telegram.org> 申请 `api_id` / `api_hash`；
2. 用 [@BotFather](https://t.me/BotFather) 创建 Bot，拿到 token；
3. 新建一个**私有频道**，把 Bot 加为管理员（需要“发布消息”“删除消息”权限）；
4. 获取频道 ID（如把频道消息转发给 @userinfobot，形如 `-100xxxxxxxxxx`）。

> 中国大陆网络无法直连 Telegram，服务需部署在海外或配置可用网络。

### 2. 运行

```bash
cp .env.example .env            # 填写配置；XIAOJU_MASTER_KEY 用 openssl rand -hex 32 生成
docker compose up -d --build    # 启动 PostgreSQL + xiaoju
```

没有服务器？见 [部署到 Hugging Face Spaces](docs/deploy-huggingface.md)。

或本地开发（Go 1.25+）：

```bash
make db                          # 只启动 PostgreSQL
# .env 中设置 XIAOJU_STORAGE=local 即可不依赖 Telegram
make run
```

## API

所有 `/api/*` 请求需带 `Authorization: Bearer $XIAOJU_ADMIN_TOKEN`，或 `X-Xiaoju-Token: $XIAOJU_ADMIN_TOKEN`（Authorization 头被占用时，如私有 HF Space）。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/folders` | 新建目录，body：`{"name": "..", "parent_id": null}` |
| `GET` | `/api/folders/{id\|root}/children` | 列出目录内容 |
| `POST` | `/api/files?name=..&parent_id=..` | 上传，请求体为文件原始内容（流式） |
| `GET` | `/api/files/{id}` | 文件元数据 |
| `GET` | `/api/files/{id}/content[?inline=1]` | 下载，支持 `Range` |
| `POST` | `/api/files/{id}/link` | 生成 6 小时有效的签名下载链接 `/dl/{id}?exp=..&sig=..`（无需认证头，可加 `&inline=1` 在线预览） |
| `DELETE` | `/api/files/{id}[?permanent=1]` | 移入回收站；`permanent=1` 永久删除文件（同时删除频道消息） |

开启了 Face ID / 指纹或格子口令后，上面这些文件接口需要 `X-Xiaoju-Session` 会话（见下方「刷脸登录」「格子口令」）。

```bash
T="Authorization: Bearer change-me"
curl -H "$T" --data-binary @movie.mp4 "http://localhost:8080/api/files?name=movie.mp4"
curl -H "$T" -H "Range: bytes=0-1048575" "http://localhost:8080/api/files/<id>/content" -o head.bin
```

## 网页前端

`web/` 是纯静态页面（目录浏览、拖拽上传、下载、图片/视频在线预览、删除、一键同步相册），推送到 `main` 后由
`.github/workflows/pages.yml` 发布到 GitHub Pages：`https://<用户名>.github.io/xiaoju-cloud/`。

1. 仓库 **Settings → Pages → Build and deployment → Source** 选 **GitHub Actions**；
2. 服务端设置 `XIAOJU_CORS_ORIGINS=https://<用户名>.github.io`（允许该网页跨域调用 API）；
3. 可选：仓库变量 `XIAOJU_API_URL` 设为服务端地址，网页会默认填好；
4. 打开网页，输入 `XIAOJU_ADMIN_TOKEN` 登录（服务端地址取自 `config.js`，为空时才需要填写）。令牌只保存在当前页面内存中，刷新或关闭页面后需重新输入。

部署在 HF Space 时 Space 需要是公开的，见 [部署文档](docs/deploy-huggingface.md#网页前端)。

**同步相册**：在相册选择器里一次多选照片 / 视频，自动传到根目录的“相册”文件夹；文件名和大小相同的视为已同步并跳过，同名不同内容自动改名为“名字 (2)”。上传 2 个并发，网络中断或服务端出错（多为 Telegram 限流）时自动重试 3 次。

本地预览：`python3 -m http.server -d web 8090`，服务端设置 `XIAOJU_CORS_ORIGINS=http://localhost:8090`。

## 测试

```bash
make test               # 单元测试
make test-integration   # 含 PostgreSQL 集成测试（需要 make db）
```

## 双备份

只用一个 Telegram 账号时，账号或频道出问题数据就全丢了。可以用**另一个账号**再建一个备份 Bot 和私有频道：

1. 用第二个账号找 @BotFather 创建备份 Bot，新建私有频道并把该 Bot 设为管理员（发布、删除消息权限）；
2. 设置 `TG_BACKUP_BOT_TOKEN` 与 `TG_BACKUP_CHANNEL_ID`（两项必须同时设置，且不能和主 Bot / 主频道相同）。

启用后：

- 上传时每个加密分块同时写入两个频道（备份写入失败不影响上传，后台会重试）；
- 下载时主频道读不到（消息被删、频道不可用等）自动改读备份；
- 后台任务每隔一段时间把还没有备份的分块（包括启用前上传的旧文件）从主频道复制过去，并用 sha256 校验；
- 永久删除时两边一起删；
- `GET /api/backup/status` 查看进度：`{"enabled":true,"parts":120,"backed_up":118,...}`。

备份频道连不上时服务照常运行（只用主频道），原因写在日志和状态接口的 `error` 字段里。
两个频道都在 Telegram，防不了 Telegram 整体不可用；重要文件请另外保留原件。

## 刷脸登录（Face ID / 指纹）

登录分两步：先输入访问令牌，再用设备的 Face ID / 指纹 / Windows Hello 验证（WebAuthn 通行密钥）。
人脸比对在设备上由系统完成，服务端只保存公钥、校验签名，**不接触任何人脸数据**。

- **开启**：用令牌登录后点右上角「🔐 安全」→「在本机开启 Face ID / 指纹」。开启后，**仅凭令牌不能再访问文件**，
  必须刷脸，通过后服务端签发 12 小时的会话（换了令牌，所有会话立即失效）。
- **加设备**：苹果 / 安卓的通行密钥会通过 iCloud 钥匙串 / Google 密码管理器同步到同一账号的设备。
  其他设备：在已登录的设备上「🔐 安全」→「给新设备生成添加码」（10 分钟、一次有效），新设备登录时点「这台设备还没注册？」输入。
- **丢了设备**：在「🔐 安全」里移除那台设备。
- **所有设备都丢了**：GitHub 仓库变量 `XIAOJU_PASSKEY_RESET=1` 后重新部署，会清空已注册设备（之后仅凭令牌登录）；
  重新注册后**务必删掉这个变量**再部署一次。
- 统计、备份状态、对账这些不涉及文件内容的运维接口，令牌依然可用（诊断工作流靠它）。
- 绑定的域名默认取 `XIAOJU_CORS_ORIGINS` 里第一个来源（如 `https://<用户名>.github.io`），也可以用
  `XIAOJU_WEBAUTHN_RP_ID` / `XIAOJU_WEBAUTHN_ORIGINS` 指定。网页换了域名，已注册的设备需要重新注册。

## 格子口令

开启后，**看照片、视频时随机抽查**：每看随机 3–7 张（预览和下载都算，在哪个文件夹都一样），打开下一张之前弹出一道格子题，答对才能继续看。
登录本身仍然是 **令牌 → Face ID / 指纹**，不用答题。这样即使别人拿到你已经登录的手机，也翻不了几张相册。

- 弹出一张**随机的 4×4 数字格**（每次都不一样），按你自己设定的规则心算出答案：
  `答案 = 乘数格 × 星期 + 加数格`，周一再加一个你定的数；**周日算 7**。
- 答案不用键盘输入，**依次点格子里的数字凑出来**（如答案 23：先点任意一个 2，再点任意一个 3），点错用 ⌫ 删除。
  每张格子都保证 0–9 至少各出现一次，任何答案都点得出来。
- **两次机会**：第一次限时 15 秒；答错或超时，换一张新格子、限时 20 秒（多给 5 秒）；第二次也没答对，**立即退出登录**
  （会话在服务端作废，记在数据库里，服务重启也不会恢复），重新输令牌、刷脸才能再进。
- 弹窗不能关掉跳过；题出了不答、刷新重来，都算用掉一次机会。
- 抽查由服务端执行：额度用完后服务端不再给照片、视频的下载链接，绕过网页直接调接口也一样。随机的张数只在服务端。
- 星期按**出题那一刻**计算（时区默认北京时间，可用 `XIAOJU_TIMEZONE` 修改），页面上会显示出题时间；跨零点作答不受影响。
- 规则（哪两个格子、周一加几）在「🔐 安全」→「开启格子口令」里点选设置，必须先答对一道练习题（不限时）才会开启。
  规则用主密钥加密后存进数据库，**代码和数据库明文里都没有**，仓库公开也不会泄露。
- 开启后不能再仅凭令牌访问文件接口（抽查要按会话计数）。
- 开启时生成 **10 个恢复码**（只显示一次，每个只能用一次）：刷脸设备丢了时代替刷脸登录；规则忘了时用来关闭格子口令。
  恢复码连续输错 3 次锁定 1 小时。
- 关闭格子口令要当场重新刷脸，并答对一道题（或用恢复码）；关闭后恢复码全部作废。
- 紧急重置：`XIAOJU_PASSKEY_RESET=1` 会同时清空刷脸设备和格子口令。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/files/{id}/link`、`GET /api/files/{id}/content` | 照片、视频额度用完时返回 `403 {"code":"grid_required"}` |
| `POST` | `/api/auth/grid/challenge` | 出一道抽查题 `{id, grid, weekday, issued_local, attempt, seconds, …}`（需要会话） |
| `POST` | `/api/auth/grid/verify` | `{"id","answer"}` 或 `{"id","timeout":true}`：答对 `{"ok":true}`；第一次没答对 `403 wrong_answer / timeout`；第二次没答对 `401 grid_logout`（会话已作废） |
| `POST` | `/api/auth/passkey/login/finish` | 刷脸通过后返回会话，以及 5 分钟有效的 `ticket`（“刚刚刷过脸”，关闭格子口令时要用） |
| `POST` | `/api/auth/recovery/passkey` | `{"code"}` 用恢复码代替刷脸 → 会话 |
| `GET` | `/api/auth/grid` | 是否开启、剩余恢复码（需要会话） |
| `POST` | `/api/auth/grid/setup/begin`、`/setup/finish` | 出练习题；`{"id","rule":{"mul","add","monday_bonus"},"answer"}` 答对后开启，返回恢复码 |
| `DELETE` | `/api/auth/grid` | `{"ticket","id","answer"}` 或 `{"ticket","recovery"}` 关闭（需要会话 + 当场刷脸 + 答题） |

## 对账与清理残块

服务重启时正在进行的上传、删除失败等情况，会在频道里留下不属于任何文件的“残块”。

- **自动**：每小时清理中断超过 6 小时的上传（删除它已传上去的分块和记录）。
- **手动对账**：`GET /api/maintenance/reconcile` 扫描两个频道并与数据库逐一核对，只出报告；
  `POST` 同一地址执行修复和清理。也可以在 GitHub Actions 里运行 “Diagnose Hugging Face Space”，选 `report` 或 `apply`。

执行时：

- 备份频道缺失的分块 → 清除备份记录，后台同步重新复制；
- 主频道缺失、备份完好的分块 → 从备份复制回主频道（校验 sha256）；
- 两边都缺失 → 只报告（`lost`），不做任何改动；
- 残块只在**同时满足**以下条件时删除：是本程序上传的分块（文件名 `<文件ID>.partNNNN`，手动发的消息永远不碰）；
  没有任何分块记录引用它（包括回收站和上传中的文件）；文件名里的文件 ID 在数据库中不存在；超过 24 小时；
  并且这次扫描找到了该频道登记过的全部分块（否则认为扫描不完整，这个频道一个也不删）。

## 路线图

- [x] **MVP**：单 Bot、单频道；目录 / 上传 / 下载 / Range / 回收站；分块加密；网页前端（GitHub Pages）
- [ ] V1：多 Bot 池并发与调度、分片断点续传（上传会话）、回收站清理任务、本地热点缓存
- [x] 双备份：另一个账号的备份频道双写，主频道读取失败自动切换，旧文件后台补齐
- [ ] V2：秒传去重、分享链接、WebDAV、按消息说明重建元数据
- [ ] V3：多用户与配额、缩略图预览、S3/R2 驱动

## 风险提示

把 Telegram 当作存储后端属于非常规用法：账号或 Bot 可能被限制，没有可用性保证（SLA）。
适合个人 / 小团队自用；重要数据请另行备份，**PostgreSQL 与主密钥必须备份**——数据库丢失后频道里的数据无法按文件名还原。
