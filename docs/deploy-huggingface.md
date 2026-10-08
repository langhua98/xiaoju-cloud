# 部署到 Hugging Face Spaces

没有服务器时，可以用 Hugging Face Spaces（Docker 类型，免费 CPU）运行 xiaoju-cloud。

```
客户端 ──HTTPS──▶ HF Space（xiaoju 服务，无持久磁盘）
                     ├──▶ 免费 PostgreSQL（Neon / Supabase）：目录树与分块映射
                     └──▶ Telegram 私有频道：加密后的文件分块
```

## 限制（先读）

| 限制 | 影响 / 对策 |
|---|---|
| Space 磁盘不持久，重启即清空 | 数据只放在外部 PostgreSQL 和 Telegram；Space 里只有临时文件和 Bot 会话（重启后自动重新登录） |
| Space 没有数据库 | 用免费的 [Neon](https://neon.tech) 或 [Supabase](https://supabase.com) PostgreSQL |
| 免费 Space 约 48 小时无访问会休眠 | 第一次访问需要等它唤醒；可接受则不用管 |
| HF 的出站网络可能拦截 Telegram | 先用默认的 `tcp`，日志提示连不上时改 `TG_TRANSPORT=websocket`；两种都不通就说明此平台不可用 |
| 上传经过 Space 中转，单个请求太久可能被网关断开 | 大文件先用较小的测试；后续版本会加分片续传 |
| 使用条款 | 把 Space 当文件中转属于灰色用法，仅建议个人自用 |

## 步骤

### 1. 准备 PostgreSQL（Neon 为例）

1. 注册 <https://neon.tech>，新建 Project（区域选离 Telegram 近的，如 Europe / Singapore）。
2. 复制连接串，形如：
   `postgres://user:pass@ep-xxx.eu-central-1.aws.neon.tech/neondb?sslmode=require`
3. 表结构不用手动建，服务启动时会自动执行迁移。

> 用 Supabase 时请使用 **Session pooler**（端口 5432）或直连地址，不要用 Transaction pooler（6543）。

### 2. 准备 GitHub 只读 token（仓库是私有的，构建时要拉代码）

GitHub → Settings → Developer settings → **Fine-grained tokens** → Generate：
- Repository access：Only select repositories → `langhua98/xiaoju-cloud`
- Permissions：**Contents: Read-only**

### 3. 创建 Space

1. <https://huggingface.co/new-space>：SDK 选 **Docker** → Blank；硬件选免费 CPU。
   可见性选 **Public** 即可（Space 里只有 Dockerfile，密钥都在 Secrets 中，API 由 `XIAOJU_ADMIN_TOKEN` 保护）。
2. 把本仓库 `deploy/huggingface/` 下的 `Dockerfile` 和 `README.md` 上传到 Space 根目录（网页 Files → Add file，或 git push）。

### 4. 配置 Secrets（Space → Settings → Variables and secrets → New secret）

| 名称 | 值 |
|---|---|
| `GITHUB_TOKEN` | 第 2 步的 token（构建时使用） |
| `XIAOJU_DATABASE_URL` | 第 1 步的连接串 |
| `XIAOJU_ADMIN_TOKEN` | 自己生成：`openssl rand -hex 16` |
| `XIAOJU_MASTER_KEY` | 自己生成：`openssl rand -hex 32`，**务必另行备份** |
| `TG_APP_ID` / `TG_APP_HASH` | my.telegram.org 获取 |
| `TG_BOT_TOKEN` | @BotFather 获取 |
| `TG_CHANNEL_ID` | 私有频道 ID，如 `-100xxxxxxxxxx`（Bot 需为管理员） |

可选 Variables：`TG_TRANSPORT`（`tcp` / `websocket`）、`XIAOJU_PART_SIZE`（默认 256MiB）。

> 没有电脑生成随机串时，可在任意 Linux / macOS 终端或 <https://www.random.org/bytes/>（选 hex）生成，长度分别为 32 和 64 个十六进制字符。

### 5. 构建并查看日志

保存 Secrets 后 Space 会自动构建。点 **Logs**：

- `connecting to telegram transport=tcp` → `telegram connected` → `listening addr=:7860 storage=telegram`：成功；
- `not connected after 1m30s ...`：把 Variable `TG_TRANSPORT` 设为 `websocket` 后 Restart；仍失败则说明 HF 拦截了 Telegram；
- `channel ... not found`：Bot 不是频道管理员，或频道 ID 不对；
- 构建阶段 `git clone` 失败：检查 `GITHUB_TOKEN` 权限。

代码更新后，在 Space Settings 里点 **Factory rebuild** 重新拉取最新代码。

### 6. 使用

Space 地址形如 `https://<用户名>-<space名>.hf.space`：

```bash
B=https://<用户名>-<space名>.hf.space
T="X-Xiaoju-Token: <XIAOJU_ADMIN_TOKEN>"
curl -H "$T" --data-binary @photo.jpg "$B/api/files?name=photo.jpg"
curl -H "$T" "$B/api/folders/root/children"
curl -H "$T" "$B/api/files/<id>/content" -o photo-back.jpg
```

如果 Space 设为 Private，请求还需要带 HF 的 token：`-H "Authorization: Bearer hf_xxx"`，此时用 `X-Xiaoju-Token` 头传本服务的令牌。
