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
| `GET` | `/api/folders/{id\|root}/children` | 列出目录内容（文件夹在前按名字；文件按拍摄时间、没有时按上传时间，新的在前）；照片、视频带 `thumb`（签名的缩略图地址）、`duration`（视频秒数）和 `taken_at`（拍摄时间），后两个生成过缩略图后才有 |
| `POST` | `/api/files?name=..&parent_id=..` | 上传，请求体为文件原始内容（流式） |
| `GET` | `/api/files/{id}` | 文件元数据 |
| `GET` | `/api/files/{id}/content[?inline=1]` | 下载，支持 `Range` |
| `POST` | `/api/files/{id}/link` | 生成 6 小时有效的签名下载链接 `/dl/{id}?exp=..&sig=..`（无需认证头，可加 `&inline=1` 在线预览） |
| `DELETE` | `/api/files/{id}[?permanent=1]` | 移入回收站；`permanent=1` 永久删除文件（同时删除频道消息） |
| `GET` | `/api/ingest/exists` | 批量上传前先问：头部 `X-File-Name`、`X-File-Size`（字节数）、`X-Xiaoju-Album`，返回 `{"result":"exists"\|"missing"}`（不需要认证） |
| `GET` | `/api/ingest/names` | 头部 `X-Xiaoju-Album`：相册里每个文件 `SHA-256("名字\|字节数")`（`text/plain`，一行一个；带“ (2)”序号的另给去掉序号的一行；只给摘要，列不出文件名；不需要认证） |
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
文件按**拍摄时间**排、新的在前（文件夹排最前、按名字）：照片读 EXIF 的 DateTimeOriginal（JPEG、PNG、HEIC，自己解析，
没写时区时按 `XIAOJU_TIMEZONE` 算），视频读 iPhone 写的 `com.apple.quicktime.creationdate`，没有时读 `creation_time`；
都在生成缩略图时一起读，存在 `files.taken_at`。读不到的（截图、下载来的图）按上传时间排。

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
- **备份照片到小桔云盘**（`shortcuts/backup.cherri`）：**上传「文件」里的一个文件夹**。先在照片 App 里把要传的相簿
  （比如“已隐藏”）全选 → 分享 → 存储到“文件”的一个新文件夹，再运行它：选放进网盘的“隐藏相册”（`X-Xiaoju-Album: hidden`）
  还是“相册”，再选那个文件夹。像断点续传：开始时 `GET /api/ingest/names` 取一次相册里已有文件的
  `SHA-256("名字|字节数")`，每个文件只取名字、扩展名和大小（文件信息，不读内容；「格式化文件大小」按字节、去掉分隔符），
  算 SHA-256 在手机上比对，对上的直接跳过、不用每个都问服务端，所以中途停了再运行一次很快就接着传。带上大小是因为
  换过手机后编号会重复（`IMG_0001` 又来一遍）：对不上的照常上传，服务端按名字 + 大小判断，同名不同内容自动改名为“名字 (2)”。
  开始时先算一下 `SHA256("xiaoju")` 对不对，不对就退回每个文件 `GET /api/ingest/exists`。结束时报告“发过去才认出来的”个数，
  这个数一直很大说明手机上的比对没起作用（比如大小的格式变了）。上传必须带文件名（`X-File-Name`），没带的返回 400 `missing_name`，快捷指令停下报错；
  以前没带名字传上来、被自动起名“照片 2026-10-16 035540-982f.mp4”的文件，重新运行时按“同文件夹、大小完全一样”认出来，
  改回真名、当作传过。（Cherri 1.3.2 自带的「获取文件的详细信息」把文件放进 `WFFolder` 参数，iPhone 上取到的名字、
  大小全是空的；`backup.cherri` 自己定义了放进 `WFInput` 的版本。）为什么不直接从相册读：
  - 相册的选择器一次选几百张，iOS 要先把每张都准备好放进内存，点完「添加」就卡住或闪退；
  - 「查找照片」的过滤条件在 iPhone 上实测都不生效：编译后写进去的“创建日期晚于 …”一张也查不到，“已隐藏 是 真”被忽略
    （试过 6 种日期写法、4 种“已隐藏”写法，新版 iOS 的确切写法查不到资料）。所以没法按日期分批，也挑不出隐藏的照片；
    **「同步相册」也用了日期条件，现在同样查不到照片**，要等找到正确写法或者换成不用条件的分批方式。

**进度**：快捷指令运行时界面上看不到进度，两个快捷指令都用**不响铃的通知**报进度：开始时一条（共几个 / 从哪天接着传），
每个视频开始上传时一条（第几个、文件名、大小，大视频要等一会儿），每处理 10 个一条（第几个、新传几个、跳过几个，
同步相册还会显示已经备份到哪天拍的；备份照片是每传 10 个、每跳过 200 个一条），最后一条汇总。

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

## 开发交接（给接手的人）

> 2026-10-16 写，记录到这一刻为止的状态、正在进行的事和踩过的坑。用户用中文沟通，在 iPhone 上实测，
> 偏好：界面简洁、提示少、点击操作、有“科技感”；改完直接合并到 `main` 部署（开发分支 `claude/grid-passcode`）。

### 线上部署

- 网页：GitHub Pages `https://langhua98.github.io/xiaoju-cloud/`（`.github/workflows/pages.yml`：发布 `web/`，
  给 `version.json` / `?v=` 打提交号，并编译、签名两个快捷指令 `xiaoju-backup.shortcut`、`xiaoju-sync.shortcut`）。
- 服务端：Hugging Face Space `https://langhua1998-xiaoju-cloud.hf.space`（`.github/workflows/deploy-hf.yml`：
  `test`（带 PostgreSQL 的 `go test`）→ `image`（`image.yml`：构建镜像，在镜像里实际生成 JPEG / HEIC / 视频缩略图）→ `deploy`）。
  只改 `web/**`、`shortcuts/**`、文档时不部署服务端。
- 推到 `main` 两条流水线同时跑。快捷指令签名用 RoutineHub 的 HubSign，**有频率限制**（常见 429，`build.py` 会等 30/60/90 秒重试），
  不要短时间内反复推送；`shortcut.yml` 只在开发分支上跑。
- 查线上状态：手动运行 `diagnose-hf.yml`（`reconcile` 选 `none` 就只读）：备份状态、网盘统计、Space 最近的日志
  （只保留 backup / ingest / WARN / ERROR 行，能看到每个批量上传的文件名和放进的文件夹）。
  **仓库是公开的，Actions 日志也公开**——日志里会出现上传的文件名，别往日志里打更敏感的东西。
- 这个沙箱里访问不了 hf.space、github.io、iCloud、Azure blob；能访问 api.github.com、npm、PyPI、Go 代理。

### 这次会话做了什么（按时间）

1. **格子口令**（`internal/auth/grid.go`、`internal/api/grid.go`、网页 `mountGridQuiz`）：规则 `乘数格 × 星期 + 加数格`（周一另加），
   看照片 / 视频随机 3–7 张抽查一次，两次机会（15 秒 + 20 秒），都错就作废会话（`revoked_sessions`）。缩略图不计数。
2. **网页自动更新**：旧页面发现 `version.json` 变了就自动换新（登录状态存在 `sessionStorage`）。
3. **缩略图 + 照片墙**：`internal/thumb`（vips 处理照片含 HEIC，ffmpeg 通过本机 HTTP + Range 截视频首帧），
   加密存在 `thumbnails` 表；`/thumb/{id}` 签名链接；网页网格视图、大图左右滑、双指 / 双击缩放、下滑关闭（`web/app.js` 的
   “网格与缩略图”“大图查看”两节）。浏览器端用 Playwright 测过（沙箱里的 Chromium 不能解 H.264，MOV 显示“加载失败”是正常的）。
4. **批量上传接口** `POST /api/ingest`（只进不出、不需要认证）：写 Telegram 期间发空格心跳防止快捷指令 60 秒超时；
   `X-Xiaoju-Album: hidden` 放进“隐藏相册”；`GET /api/ingest/exists` 先问传过没有；没带文件名直接 400 `missing_name`；
   以前被自动起名“照片 日期 时分秒-xxxx.ext”的文件，重新上传时按同文件夹同大小认出来改回真名（`uploadedBefore`）。
5. **快捷指令**（`shortcuts/*.cherri`，`shortcuts/build.py` 编译、补丁、检查嵌套、签名）。来回改了很多版，结论见下面“快捷指令的坑”。
6. **按拍摄时间排序**（迁移 `0009_taken_at`，`internal/thumb/taken.go`）：拍摄时间在生成缩略图时读出来。`thumbnails.version`
   记录缩略图是哪一版程序生成的（`meta.ThumbnailVersion`），服务启动时 `BackfillThumbnails` 把旧版本的、还没有缩略图的照片视频
   在后台一个个重新生成一遍（日志 `thumbnail backfill started / finished`）；以后再要给缩略图加东西，把版本号加一就会自动补。

### 正在进行（交接时的状态）

- 用户在把 iPhone 的**隐藏相册（1561 个文件，约 910 张照片、650 个视频）** 传进网盘根目录的“隐藏相册”文件夹：
  照片 App 全选 → 存储到“文件”（我的 iPhone 里的一个文件夹）→ 运行「备份照片到小桔云盘」选“隐藏相册”和那个文件夹。
- 修复前传上去约 20 个没有名字的文件（名字是“照片 2026-10-16 03xxxx-xxxx.*”），重新运行时会被自动改回真名，不会重复。
  传完可以在网页里确认“隐藏相册”里没有剩下这种名字的文件。
- 修复（提交 `da072e0`）刚推上去，等部署完让用户删掉旧快捷指令、重装、在快捷指令 App 里重新运行。
- 用户的下一步计划：隐藏相册传完 → 传**整个相册** → 之后**按时间只传新照片**。这两项现在都**没有可用的实现**（见下）。

### 快捷指令的坑（都在 iPhone 上实测过）

- **「查找照片」的过滤条件在新版 iOS 上不生效**：`build.py` 编译后写进去的条件，`backup` 的「🔧 诊断」（已删，见提交
  `45ecbf6`）结果是：6 种日期写法（文字 / 日期变量、放在 Values / 最外层 / VariableOverrides、操作符 0/2/3）**一张都查不到**；
  4 种“已隐藏”写法（`Bool` 放最外层或 Values 里）**都被忽略**（和不加条件结果一样）。不加条件、按创建日期排序、限制张数是好的
  （查 300 张没问题）。新版 iOS 的确切写法查不到资料；最靠谱的办法是让用户在快捷指令 App 里手动做一个带条件的「查找照片」，
  导出 / 发来看真实的 plist（签名后的 `.shortcut` 是 AEA 归档，需要自己解）。
  - 所以 **`sync.cherri`（同步相册）现在查不到照片**，网页上已经标了“正在修”。
  - 不靠过滤条件的分批办法：不加条件的「查找照片」按创建日期最旧的排最前、限制 `已处理数 + 30` 张，再用「获取列表中的项目」
    取最后 30 张；进度存“已处理到第几张 + 最后一张的拍摄时间（毫秒）”，取到的照片先按时间跳过处理过的（防止中间删了照片错位）。
    隐患：越往后一次查的张数越多，几千张时的内存没验证过。
  - 只传新照片可以用「获取最新的照片」（不加条件，按时间倒序）再按拍摄时间跳过已处理的。
- **相册选择器一次选几百张会卡死或闪退**（iOS 先把每张都准备好放进内存），网页的相册选择器也一样，所以别用选择器传大量照片。
- **Cherri 1.3.2 的 bug**：`else if` 编译后“结束”放错位置（只用 `if/else` 嵌套，`build.py` 会检查嵌套）；
  `getFileDetail` 把文件放进 `WFFolder`（应该是 `WFInput`，取到的全是空的，`backup.cherri` 自己定义了 `fileDetail`）；
  `resp['key']` 会编译成读取文件属性（要先「从输入获取词典」再「获取词典值」）；`==` 比较要先把值转成文字变量 `"{x}"`。
- **「此操作无法在当前用户界面中运行」**：快捷指令运行时 App 进了后台、熄屏或锁屏，「获取 URL 的内容」就会报这个错；
  新装的快捷指令第一次联网要弹窗授权，新版 iOS 的简化运行界面弹不出来也报这个错——让用户在编辑界面点 ▶ 运行一次、选“始终允许”。
  授权过（始终允许）以后，用户实测切到别的 App 也会在后台继续传（灵动岛上有进度圈），锁屏后会不会停还没验证过。
  长时间上传还是让用户把自动锁定设成“永不”、插着电。
- 编码视频（「编码视频」Passthrough）在从相册直接读视频时需要；从“文件”传就不用了。
- 发通知用不响铃的（`showNotification(..., false)`），每 10 个一条，是唯一能在运行中看到进度的办法。

### 其他注意

- Telegram 写入有频率限制（每个频道大约每分钟 20 条，日志里会看到 `telegram flood wait`），上传上千个文件要一两个小时以上。
- 缩略图存在数据库里，每张 10–20 KB；用户用的是免费 PostgreSQL，照片多了注意容量。
- 测试：`XIAOJU_TEST_DATABASE_URL=... go test -race ./...`；`internal/thumb` 的测试需要 ffmpeg（HEIC 测试需要 vips，没有就跳过）。
  `TestIngest` 里改过 `ingestHeartbeat` 的地方要及时改回来（曾经因此在 CI 上偶发失败、挡住部署）。
- 本地看网页：起服务端（`XIAOJU_STORAGE=local`）+ `python3 -m http.server -d web`，`web/config.js` 指向本地服务端；
  Playwright 用 `/opt/pw-browsers/chromium`。
