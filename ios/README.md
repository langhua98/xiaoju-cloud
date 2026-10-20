# 小桔备份（iPhone App，测试版）

目标是像 iCloud 照片一样：新拍的照片、视频由系统在后台自动传进小桔云盘的「相册」，锁屏、切到别的 App 也照样传。
用的是苹果 iOS 26.1 起开放给第三方备份 App 的 PhotoKit「后台上传扩展」
（[Uploading asset resources in the background](https://developer.apple.com/documentation/photokit/uploading-asset-resources-in-the-background)），
这里用 iOS 26.1 的 `PHBackgroundResourceUploadExtension`（iOS 27 起有新的 async 版本 `PHBackgroundResourceUploadJobExtension`，但 GitHub 的 Mac 机器上还只有 iOS 26.5 的 SDK；旧接口在 iOS 27 上照样能用），**需要 iOS 26.1 以上**。

这一版只验证流程：

- `App/`：主 App。允许访问所有照片、打开「后台自动备份」开关；还有一个「现在上传最新的一张」按钮，直接从 App 里传，确认手机到服务端是通的。
- `Extension/`：后台上传扩展。第一次被调用时记下当时的相册变更令牌，之后只传新加进相册的照片（照片原图、视频原片、实况照片的视频部分），
  每个资源一个上传任务，`POST /api/ingest`（和快捷指令同一个接口，不需要认证，名字 + 大小相同的跳过）。
- 上传前系统会先发 `OPTIONS /api/ingest` 问支不支持断点续传，服务端现在回 501（不支持），日志里记 `ingest preflight`。
  之后会实现 IETF 断点续传协议，大视频断网后能接着传。

## 编译

仓库里不放 Xcode 工程。推送改动后 `.github/workflows/ios.yml` 在 GitHub 的 Mac 机器上 `xcodegen generate` 再编译，
得到**没签名**的 `XiaojuBackup.ipa`，发布到 Release「ios-test」（每次覆盖）。服务端地址默认是 `web/config.js` 里那个，
设了仓库变量 `XIAOJU_API_URL` 时用它。

## 安装（不用电脑）

1. 手机上装 [Feather](https://github.com/khcrysalis/feather)（开源的手机端签名安装工具），导入一张证书。
2. 在 Safari 打开 Release「ios-test」，下载 `XiaojuBackup.ipa`，用 Feather 打开、签名、安装。
3. 打开「小桔备份」，允许访问**所有**照片，点「现在上传最新的一张」，看到 HTTP 200 / 201 说明通了。
4. 打开「后台自动备份」，拍一张新照片，锁屏等一会儿（系统挑时机，可能几分钟到几小时），再去网页上看「相册」。

免费证书经常被苹果吊销，吊销后 App 打不开，换一张证书重签即可（照片在服务端，不受影响）。
