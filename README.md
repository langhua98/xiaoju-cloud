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
| `GET` | `/api/folders/{id\|root}/children` | 列出目录内容；照片、视频带 `thumb`（签名的缩略图地址）和 `duration`（视频秒数，生成过缩略图后才有） |
| `POST` | `/api/files?name=..&parent_id=..` | 上传，请求体为文件原始内容（流式） |
| `GET` | `/api/files/{id}` | 文件元数据 |
| `GET` | `/api/files/{id}/content[?inline=1]` | 下载，支持 `Range` |
| `POST` | `/api/files/{id}/link` | 生成 6 小时有效的签名下载链接 `/dl/{id}?exp=..&sig=..`（无需认证头，可加 `&inline=1` 在线预览） |
| `DELETE` | `/api/files/{id}[?permanent=1]` | 移入回收站；`permanent=1` 永久删除文件（同时删除频道消息） |
| `GET` | `/thumb/{id}?exp=..&sig=..` | 320×320 的 JPEG 缩略图（地址来自列目录，无需认证头；做不了的返回 404） |

开启了 Face ID / 指纹或格子口令后，上面这些文件接口需要 `X-Xiaoju-Session` 会话（见下方「刷脸登录」「格子口令」）。

```bash
T="Authorization: Bearer change-me"
curl -H "$T" --data-binary @movie.mp4 "http://localhost:8080/api/files?name=movie.mp4"
curl -H "$T" -H "Range: bytes=0-1048575" "http://localhost:8080/api/files/<id>/content" -o head.bin
```

## 网页前端

`web/` 是纯静态页面（目录浏览、照片墙、大图查看、拖拽上传、下载、删除、一键同步相册），推送到 `main` 后由
`.github/workflows/pages.yml` 发布到 GitHub Pages：`https://<用户名>.github.io/xiaoju-cloud/`。

1. 仓库 **Settings → Pages → Build and deployment → Source** 选 **GitHub Actions**；
2. 服务端设置 `XIAOJU_CORS_ORIGINS=https://<用户名>.github.io`（允许该网页跨域调用 API）；
3. 可选：仓库变量 `XIAOJU_API_URL` 设为服务端地址，网页会默认填好；
4. 打开网页，输入 `XIAOJU_ADMIN_TOKEN` 登录（服务端地址取自 `config.js`，为空时才需要填写）。令牌只保存在当前页面内存中；
   登录后的会话（12 小时）保存在当前标签页，刷新页面不用重新登录，关闭标签页即失效。

部署在 HF Space 时 Space 需要是公开的，见 [部署文档](docs/deploy-huggingface.md#网页前端)。

**同步相册**：在相册选择器里一次多选照片 / 视频，自动传到根目录的“相册”文件夹；文件名和大小相同的视为已同步并跳过，同名不同内容自动改名为“名字 (2)”。上传 2 个并发，网络中断或服务端出错（多为 Telegram 限流）时自动重试 3 次。
上传期间屏幕保持常亮（支持屏幕唤醒锁的浏览器），显示预计剩余时间，传完可以“继续选下一批”。

> iPhone 上一次选几百张时，点“添加”后系统要先把每张照片准备好（必要时转换格式、从 iCloud 下载原图）才交给网页，
> 网页拿不到进度，只能显示已经等了多久。一次选 100 张左右最顺；几百上千张请用下面的「快捷指令批量上传」。

**照片墙**：照片、视频占多数的文件夹（比如“相册”）自动用网格显示缩略图（手机上一行 3 张，视频右下角标时长），
其他文件夹用列表（照片也带小缩略图）；右上角可以切换，按文件夹记住。

**大图查看**：点开一张后左右滑动切换（电脑上用 ← → 或两侧箭头），双指或双击放大、放大后拖动查看，往下拖关闭，
单击隐藏顶栏；顶栏可以下载、删除。手机的返回手势也是关闭，不会退出相册。
只有停在某一张上才加载原图（计入格子口令的“看了几张”，同一张来回看只算一次），旁边几张只用缩略图垫着。

**缩略图**：服务端生成 320×320、居中裁剪的 JPEG。照片用 libvips（认得 iPhone 的 HEIC，按拍摄方向转正），视频用 ffmpeg
截第一帧，并且只从 Telegram 拉取视频的索引和第一帧，不下载整个视频。上传完在后台生成，没生成过的第一次显示时现做（最多同时 3 张）。
缩略图用主密钥加密后存在**数据库**里（每张约 10–20 KB，一万张约 150 MB，注意免费数据库的容量），不占 Telegram 的发送频率；
永久删除文件时一起删掉。缩略图很小，**不计入**格子口令的抽查；签名和下载链接分开，不能用来下载原图。
Docker 镜像里装了 `vips-tools vips-heif ffmpeg`（`xiaoju thumb 输入 输出.jpg` 可以检查）；没装时不生成，网页显示图标。

**自动更新**：发布时工作流给网页打上提交号（`version.json`、资源地址的 `?v=`）。已经打开的旧页面在启动和切回前台时检查版本：
没登录时自动换成新版；已登录时顶部提示“网页有新版本”（令牌和会话只在内存里，刷新后要重新登录）。

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
- 答案**直接点格子里的数字**（如答案 23：先点任意一个 2，再点任意一个 3），点满位数自动提交，没有确认按钮；
  格子下方的小横线表示答案有几位。每张格子都保证 0–9 至少各出现一次，任何答案都点得出来。
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
| `POST` | `/api/auth/grid/challenge` | 出一道抽查题 `{id, grid, weekday, attempt, seconds, digits, …}`（`digits` 是答案位数；需要会话） |
| `POST` | `/api/auth/grid/verify` | `{"id","answer"}` 或 `{"id","timeout":true}`：答对 `{"ok":true}`；第一次没答对 `403 wrong_answer / timeout`；第二次没答对 `401 grid_logout`（会话已作废） |
| `POST` | `/api/auth/passkey/login/finish` | 刷脸通过后返回会话，以及 5 分钟有效的 `ticket`（“刚刚刷过脸”，关闭格子口令时要用） |
| `POST` | `/api/auth/recovery/passkey` | `{"code"}` 用恢复码代替刷脸 → 会话 |
| `GET` | `/api/auth/grid` | 是否开启、剩余恢复码（需要会话） |
| `POST` | `/api/auth/grid/setup/begin`、`/setup/finish` | `{"rule":{"mul","add","monday_bonus"}}` 按新规则出练习题；`{"id","answer"}` 答对后开启，返回恢复码 |
| `DELETE` | `/api/auth/grid` | `{"ticket","id","answer"}` 或 `{"ticket","recovery"}` 关闭（需要会话 + 当场刷脸 + 答题） |

## 快捷指令批量上传

不经过 Safari 的照片选择器：iPhone「快捷指令」从相册一张张读出原图，直接传进根目录的“相册”文件夹，几百上千张也不会卡住，
还可以设成每天自动备份新照片。

两个快捷指令，网页「🔐 安全」里各有一个安装按钮，在快捷指令 App 里「添加快捷指令」即可，不用填任何东西：

- **同步相册到小桔云盘**（`shortcuts/sync.cherri`）：不用选照片，把整个相册按拍摄时间从旧到新传上去。
  快捷指令一次查整个相册会超出内存上限（报“运行快捷指令时出现问题”），所以每批只查“进度之后”最旧的 50 张，
  一次运行最多 10 批（500 张）。进度存在 iCloud 云盘「快捷指令」文件夹的 `xiaoju-sync.json`
  （`since`：给「查找照片」用的日期文字；`key`：精确到毫秒的拍摄时间 `yyyyMMddHHmmssSSS`，不晚于它的照片都处理过了），
  每传好一张存一次，中途停了、出错了，再运行一次从那张接着传。结束时弹窗报告新上传、以前传过的张数，备份到了哪天，后面还有没有。
  满满一批都没有新进度时（日期条件没生效，或 50 张拍摄时间完全相同）提示“同步卡住了”，不会误报传完。删掉 `xiaoju-sync.json` 就从头检查一遍
  （已传过的同名同大小文件服务端会跳过）。
- **备份照片到小桔云盘**（`shortcuts/backup.cherri`）：**不用相册的选择器**——一次选几百张时，iOS 要先把每张照片都准备好
  放进内存才交给快捷指令，点完「添加」就卡住或闪退。改成先选传什么：隐藏相册、整个相册、接着上次、最近 7 天、最近 30 天、某一天起，
  再按拍摄时间从旧到新一批 30 张地查出来传，一次运行最多 200 批（6000 个）。
  - **隐藏相册**：「查找照片」加上“已隐藏 是 真”，上传时带 `X-Xiaoju-Album: hidden`，放进根目录的“隐藏相册”文件夹；
    进度单独存在 `xiaoju-hidden.json`，每次运行都接着上次传。
  - 其他几项加上“已隐藏 是 假”，放进“相册”；进度存在 `xiaoju-backup.json`（格式同上），中途停了再运行选「接着上次」接着传；
    整个相册传完以后选「接着上次」就只传新拍的。

**进度**：快捷指令运行时界面上看不到进度，两个快捷指令都用**不响铃的通知**报进度：开始时一条（共几个 / 从哪天接着传），
每个视频开始上传时一条（第几个、文件名、大小，大视频要等一会儿），每处理 10 个一条（第几个、新传几个、跳过几个，
同步相册还会显示已经备份到哪天拍的），最后一条汇总。

两个文件都在发布网页时由 `shortcuts/build.py` 自动编译（[Cherri](https://github.com/electrikmilk/cherri)），上传地址取自 `web/config.js`，
并交给 RoutineHub 的 HubSign 服务签名（iOS 15 起快捷指令文件必须签名才能导入；文件里只有上传地址，没有任何密钥）。
签名服务不可用时网页照常发布，只是不显示对应的安装按钮，可以按面板里的步骤自己搭建。构建时：

- Cherri 没有带过滤条件的「查找照片」：`sync.cherri` 里先用「获取最新的照片」占位，编译后换成
  「查找照片：创建日期 晚于 进度，按创建日期最旧的排最前，限制 50 张」；
- Cherri 1.3.2 编译 `else if` 会把“结束”放错位置（后面的操作被关进别的分支、循环的“结束”丢失），所以源码里不用 `else if`，
  `build.py` 也会检查“如果 / 重复”的开始和结束一一对应，不对就不发布。
- **只进不出**：上传接口不需要认证，但只能往“相册”（带 `X-Xiaoju-Album: hidden` 时是“隐藏相册”，别的值一律拒绝）里**添加**文件（同名不同内容自动改名，不会覆盖），不能列目录、下载或删除，
  看照片仍然要刷脸和格子口令。代价是知道地址的人都能往“相册”里塞文件（日志里记了上传者的 IP）。
- 文件名（去掉“ (2)”这类序号、不分大小写）和大小都相同的直接跳过，不会重复写入 Telegram，所以中途出错再运行一次即可。
- 照片直接传；**视频先用「编码视频」（Passthrough：原尺寸、不重新压缩）导出成文件再传**——直接拿相册里的视频项上传，
  某些视频（录屏、慢动作、原片在 iCloud）会报 “produced no file representation for type public.mpeg-4” 并停下。
- 大文件收完后写 Telegram 可能要几分钟，而快捷指令等回复超过约 60 秒就算超时：服务端先把整个文件收完、加密存进临时文件，
  写了 10 秒还没写完就先回 200，每 10 秒发一个空格保持连接，写完再发 JSON；文件收完后即使手机断开也会写完（下次运行跳过）。
  临时目录（`XIAOJU_TMP_DIR`）要能放下最大那个视频的密文。
- 每天自动备份：「自动化」→“特定时间”或“接入充电器”，运行「同步相册到小桔云盘」。

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/ingest` | 不需要认证。头部 `X-File-Name`（或 `?name=`，没有扩展名时按内容补上），请求体是文件原始内容；新文件 `201 {"result":"uploaded"}`，已存在 `200 {"result":"skipped"}` |

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
- [x] 照片墙：缩略图网格、大图左右滑动、双指缩放
- [ ] V3：多用户与配额、S3/R2 驱动

## 风险提示

把 Telegram 当作存储后端属于非常规用法：账号或 Bot 可能被限制，没有可用性保证（SLA）。
适合个人 / 小团队自用；重要数据请另行备份，**PostgreSQL 与主密钥必须备份**——数据库丢失后频道里的数据无法按文件名还原。
