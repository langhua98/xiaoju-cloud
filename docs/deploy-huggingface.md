# 部署到 Hugging Face Spaces

没有服务器时，可以用 Hugging Face Spaces（Docker 类型，免费 CPU）运行 xiaoju-cloud。
部署由 GitHub Actions 自动完成（`.github/workflows/deploy-hf.yml`），本地不需要任何工具。

```
GitHub Actions ──上传源码 + 写入 Secrets──▶ HF 私有 Space（xiaoju 服务，无持久磁盘）
                                             ├──▶ 免费 PostgreSQL（Neon / Supabase）：目录树与分块映射
                                             └──▶ Telegram 私有频道：加密后的文件分块
```

## 限制（先读）

| 限制 | 影响 / 对策 |
|---|---|
| Space 磁盘不持久，重启即清空 | 数据只放在外部 PostgreSQL 和 Telegram；Space 里只有临时文件和 Bot 会话（重启后自动重新登录） |
| Space 没有数据库 | 用免费的 [Neon](https://neon.tech) 或 [Supabase](https://supabase.com) PostgreSQL |
| 免费 Space 约 48 小时无访问会休眠 | 第一次访问需要等它唤醒 |
| HF 的出站网络可能拦截 Telegram | 先跑诊断模式确认；不通时设 `TG_TRANSPORT=websocket` 再试 |
| 上传经过 Space 中转，单个请求太久可能被网关断开 | 大文件先用较小的测试；后续版本会加分片续传 |
| 使用条款 | 把 Space 当文件中转属于灰色用法，仅建议个人自用 |

## 第一步：诊断（不需要数据库）

先确认 HF Space 能否连上 Telegram。没有设置 `XIAOJU_DATABASE_URL` 时，服务以**诊断模式**启动：
只做 Bot 登录和频道访问检查，结果写在日志里，并显示在 Space 首页。

在 GitHub 仓库 **Settings → Secrets and variables → Actions → New repository secret** 添加：

| Secret | 值 |
|---|---|
| `HF_TOKEN` | <https://huggingface.co/settings/tokens> 创建的 **Write** 权限 token |
| `TG_APP_ID` / `TG_APP_HASH` | my.telegram.org 获取 |
| `TG_BOT_TOKEN` | @BotFather 获取 |
| `TG_CHANNEL_ID` | 私有频道 ID，如 `-100xxxxxxxxxx`（Bot 需为管理员） |

然后 **Actions → Deploy to Hugging Face Space → Run workflow**。工作流会：

1. 跑单元测试和集成测试；
2. 创建私有 Space `<HF 用户名>/xiaoju-cloud`（可用 Variable `HF_SPACE_ID` 改名）；
3. 把 Secrets 写进 Space，上传源码，等待构建；
4. 打印 Space 运行日志。

在日志中查看结果：

- `telegram check passed` → HF 能连上 Telegram，进入第二步；
- `telegram: not connected after 1m30s` → 在 **Variables** 里添加 `TG_TRANSPORT=websocket` 后重跑；仍不通说明 HF 拦截了 Telegram；
- `channel ... not found` → Bot 不是频道管理员，或频道 ID 不对；
- `ACCESS_TOKEN_INVALID` / `API_ID_INVALID` → Bot token 或 api_id/api_hash 有误。

## 第二步：正式运行

1. 准备 PostgreSQL（Neon 为例）：注册 <https://neon.tech> → Create Project（区域选 Europe 或 Singapore）→ 复制连接串，形如
   `postgres://user:pass@ep-xxx.eu-central-1.aws.neon.tech/neondb?sslmode=require`。
   表结构会在服务启动时自动创建。
   > 用 Supabase 时请使用 **Session pooler**（端口 5432）或直连地址，不要用 Transaction pooler（6543）。
2. 在 GitHub Secrets 中再添加：

   | Secret | 值 |
   |---|---|
   | `XIAOJU_DATABASE_URL` | 上面的连接串 |
   | `XIAOJU_ADMIN_TOKEN` | API 访问令牌，随机字符串（如 `openssl rand -hex 16`） |
   | `XIAOJU_MASTER_KEY` | 64 位十六进制（如 `openssl rand -hex 32`），**务必另行备份，丢失后文件无法解密** |

3. 重新运行工作流。日志出现 `listening addr=:7860 storage=telegram` 即部署成功。

之后每次推送到 `main`（文档改动除外）都会自动重新部署。没有设置 `HF_TOKEN` 时部署步骤会直接跳过。

## 使用

Space 是私有的，请求需同时带 HF token（给 HF 网关）和本服务的令牌：

```bash
B=https://<HF用户名>-xiaoju-cloud.hf.space
HF="Authorization: Bearer hf_xxx"
T="X-Xiaoju-Token: <XIAOJU_ADMIN_TOKEN>"
curl -H "$HF" -H "$T" --data-binary @photo.jpg "$B/api/files?name=photo.jpg"
curl -H "$HF" -H "$T" "$B/api/folders/root/children"
curl -H "$HF" -H "$T" "$B/api/files/<id>/content" -o photo-back.jpg
```

如果把 Space 改成 Public，就不需要 HF token，只用 `X-Xiaoju-Token`（或 `Authorization: Bearer <XIAOJU_ADMIN_TOKEN>`）。
