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
- 连接方式 `TG_TRANSPORT`：`tcp` 直连，或 `websocket` 经 `*.web.telegram.org:443`；启动 90 秒内连不上会报错退出；
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

```bash
T="Authorization: Bearer change-me"
curl -H "$T" --data-binary @movie.mp4 "http://localhost:8080/api/files?name=movie.mp4"
curl -H "$T" -H "Range: bytes=0-1048575" "http://localhost:8080/api/files/<id>/content" -o head.bin
```

## 网页前端

`web/` 是纯静态页面（目录浏览、拖拽上传、下载、图片/视频在线预览、删除），推送到 `main` 后由
`.github/workflows/pages.yml` 发布到 GitHub Pages：`https://<用户名>.github.io/xiaoju-cloud/`。

1. 仓库 **Settings → Pages → Build and deployment → Source** 选 **GitHub Actions**；
2. 服务端设置 `XIAOJU_CORS_ORIGINS=https://<用户名>.github.io`（允许该网页跨域调用 API）；
3. 可选：仓库变量 `XIAOJU_API_URL` 设为服务端地址，网页会默认填好；
4. 打开网页，填写服务端地址和 `XIAOJU_ADMIN_TOKEN` 即可使用（令牌只保存在浏览器本地）。

部署在 HF Space 时 Space 需要是公开的，见 [部署文档](docs/deploy-huggingface.md#网页前端)。

本地预览：`python3 -m http.server -d web 8090`，服务端设置 `XIAOJU_CORS_ORIGINS=http://localhost:8090`。

## 测试

```bash
make test               # 单元测试
make test-integration   # 含 PostgreSQL 集成测试（需要 make db）
```

## 路线图

- [x] **MVP**：单 Bot、单频道；目录 / 上传 / 下载 / Range / 回收站；分块加密；网页前端（GitHub Pages）
- [ ] V1：多 Bot 池并发与调度、分片断点续传（上传会话）、回收站清理任务、本地热点缓存
- [ ] V2：秒传去重、分享链接、WebDAV、备份频道双写、按消息说明重建元数据
- [ ] V3：多用户与配额、缩略图预览、S3/R2 驱动

## 风险提示

把 Telegram 当作存储后端属于非常规用法：账号或 Bot 可能被限制，没有可用性保证（SLA）。
适合个人 / 小团队自用；重要数据请另行备份，**PostgreSQL 与主密钥必须备份**——数据库丢失后频道里的数据无法按文件名还原。
