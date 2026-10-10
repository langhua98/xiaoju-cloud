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
| HF 屏蔽了 Telegram 域名（DNS 解析失败） | 保持默认的 `TG_TRANSPORT=tcp`：MTProto 直连 Telegram 数据中心 IP，不经过 DNS（2026-10 实测可用）；**不要**用 `websocket`，它依赖 `*.web.telegram.org` 域名 |
| 自 2026-07 起免费账号不能新建 Docker Space（402） | 订阅 PRO；或沿用账号里规则变更前就有、仍占着免费额度的 Docker Space：把它改名为 `xiaoju-cloud`（或用 `HF_SPACE_ID` 指向它），部署会覆盖其内容。已暂停的旧 Space 通常因额度为 0 无法再启动 |
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
- `telegram: not connected after 1m30s` → 确认 Variables 里没有把 `TG_TRANSPORT` 设成 `websocket`（HF 屏蔽了 Telegram 域名，只有默认的 `tcp` 能连上）；
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

## 网页前端

网页前端部署在 GitHub Pages（见 README“网页前端”），浏览器直接跨域调用 Space 上的 API。
私有 Space 的网关会拦截浏览器的跨域预检请求，所以使用网页前端时 **Space 必须公开**，访问控制由 `XIAOJU_ADMIN_TOKEN` 负责
（请使用足够长的随机令牌；文件内容在 Telegram 中是加密的，Space 公开不会暴露数据，只是任何人都能看到 Space 的源码页面）。

在 GitHub 仓库 **Settings → Secrets and variables → Actions → Variables** 添加：

| Variable | 值 |
|---|---|
| `HF_SPACE_PUBLIC` | `true`（下次部署时把 Space 设为公开；删除该变量后再部署会改回私有） |
| `XIAOJU_CORS_ORIGINS` | `https://<GitHub 用户名>.github.io` |
| `XIAOJU_API_URL` | `https://<HF用户名>-xiaoju-cloud.hf.space`（网页默认服务端地址，可选） |

然后重新运行 “Deploy to Hugging Face Space” 和 “Deploy web to GitHub Pages” 两个工作流。
