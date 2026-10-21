import Photos
import SwiftUI
import SafariServices

// 小桔备份（测试版）：打开 PhotoKit 的后台上传扩展（iOS 26.1 起苹果开放给第三方备份 App 的接口），
// 之后系统在后台把新拍的照片、视频交给扩展（Extension/BackgroundUploadExtension.swift）传进小桔云盘的「相册」。
// 「云盘」页直接打开网盘网页（浏览、下载、分享都用网页的功能），「相册」页按手机相册原样上传，「备份」页管后台自动备份。
@main
struct XiaojuBackupApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    var body: some Scene {
        WindowGroup {
            TabView {
                DriveView().tabItem { Label("云盘", systemImage: "externaldrive") }
                AlbumsView().tabItem { Label("相册", systemImage: "photo.on.rectangle") }
                ContentView().tabItem { Label("备份", systemImage: "arrow.up.circle") }
            }
            .onOpenURL { url in
                // 快捷指令「充电时备份到小桔云盘」：xiaojubackup://backup
                if url.host == "backup" { AutoBackup.shared.start(reason: "快捷指令") }
            }
        }
    }
}

struct DriveView: View {
    @State private var generation = 0 // 点了 Safari 的「完成」就重新打开

    private var url: URL? {
        URL(string: Bundle.main.object(forInfoDictionaryKey: "XiaojuWebURL") as? String ?? "")
    }

    var body: some View {
        if let url {
            SafariView(url: url) { generation += 1 }
                .id(generation)
                .ignoresSafeArea(edges: .top)
        } else {
            Text("网页地址没配置")
        }
    }
}

// 用 Safari 的网页视图打开网盘：App 自己的 WKWebView 用不了 Face ID 通行密钥（要绑定域名，自签名的 App 做不到），
// Safari 视图里可以，用的就是 Safari 里存的通行密钥。下载、分享也交给 Safari。
struct SafariView: UIViewControllerRepresentable {
    let url: URL
    let onDone: () -> Void

    func makeCoordinator() -> Coordinator { Coordinator(onDone: onDone) }

    func makeUIViewController(context: Context) -> SFSafariViewController {
        let config = SFSafariViewController.Configuration()
        config.barCollapsingEnabled = true
        let view = SFSafariViewController(url: url, configuration: config)
        view.dismissButtonStyle = .close
        view.delegate = context.coordinator
        return view
    }

    func updateUIViewController(_ view: SFSafariViewController, context: Context) {}

    final class Coordinator: NSObject, SFSafariViewControllerDelegate {
        let onDone: () -> Void
        init(onDone: @escaping () -> Void) { self.onDone = onDone }
        func safariViewControllerDidFinish(_ controller: SFSafariViewController) { onDone() }
    }
}

struct ContentView: View {
    @State private var auth = PHPhotoLibrary.authorizationStatus(for: .readWrite)
    @State private var enabled = PHPhotoLibrary.shared().uploadJobExtensionEnabled
    @State private var message = ""
    @State private var busy = false
    @ObservedObject private var auto = AutoBackup.shared
    @ObservedObject private var albumUploader = AlbumUploader.shared

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    var body: some View {
        NavigationStack {
            Form {
                Section("服务端") {
                    Text(server).font(.footnote).textSelection(.enabled)
                }
                Section("相册权限") {
                    Text(authText)
                    if auth != .authorized {
                        Button("允许访问所有照片") { Task { await requestAuth() } }
                    }
                }
                Section {
                    Toggle("充电时自动备份", isOn: $auto.enabled)
                    if auto.running {
                        Button("停止", role: .destructive) { auto.stop() }
                    } else {
                        Button("现在备份") { auto.start(reason: "手动") }
                            .disabled(auth != .authorized || albumUploader.running)
                        if albumUploader.running {
                            Text("「相册」页正在上传，等它传完或停掉再备份。").font(.footnote).foregroundStyle(.secondary)
                        }
                    }
                    if !auto.status.isEmpty {
                        Text(auto.status).font(.footnote).foregroundStyle(.secondary).textSelection(.enabled)
                    }
                } header: {
                    Text("自动备份")
                } footer: {
                    Text("先传隐藏相册（放进云盘「隐藏相册」），再传全部照片（放进云盘「相册」）、「相册」页勾选的相册、通讯录、日历、提醒事项，只传新的。打开开关后，系统会在充电、连着网的时候（一般是夜里）自动跑。想每次一插电就跑：快捷指令 → 自动化 → 新建 → 充电器 → 已连接 → 立即运行 → 添加操作「备份到小桔云盘」。第一次请先点「现在备份」，允许通讯录、日历、提醒事项的权限。")
                }
                Section {
                    Toggle("后台自动备份", isOn: Binding(get: { enabled }, set: { setEnabled($0) }))
                        .disabled(auth != .authorized)
                } header: {
                    Text("系统后台上传（建议关掉 iCloud 照片，开着时系统可能不调用）")
                } footer: {
                    Text("打开后，之后新拍的照片、视频由系统在后台传进小桔云盘的「相册」（最近 24 小时拍的也会补传，更早的不传）。系统会挑网络、电量合适的时候传，可能要等几分钟到几小时。")
                }
                Section {
                    Button(busy ? "正在上传…" : "现在上传最新的一张") { Task { await uploadLatest() } }
                        .disabled(auth != .authorized || busy)
                } footer: {
                    Text("不经过后台扩展，直接从 App 里传，用来确认手机到服务端是通的。")
                }
                if !message.isEmpty {
                    Section("结果") { Text(message).font(.footnote).textSelection(.enabled) }
                }
            }
            .navigationTitle("小桔备份")
        }
    }

    private var authText: String {
        switch auth {
        case .authorized: return "已允许访问所有照片"
        case .limited: return "只允许了部分照片：后台上传需要「所有照片」，请到 设置 → 小桔备份 → 照片 里改"
        case .denied, .restricted: return "没有权限：请到 设置 → 小桔备份 → 照片 里打开"
        default: return "还没有询问"
        }
    }

    private func requestAuth() async {
        auth = await PHPhotoLibrary.requestAuthorization(for: .readWrite)
    }

    private func setEnabled(_ on: Bool) {
        do {
            try PHPhotoLibrary.shared().setUploadJobExtensionEnabled(on)
            message = on ? "已打开后台上传。现在拍一张照片，锁屏等一会儿，再去网页上看「相册」。" : "已关闭后台上传。"
        } catch {
            message = "开关失败：\(error)"
        }
        enabled = PHPhotoLibrary.shared().uploadJobExtensionEnabled
    }

    // 直接传最新的一张（原图），和后台扩展用同一个接口 POST /api/ingest。
    private func uploadLatest() async {
        busy = true
        defer { busy = false }
        let options = PHFetchOptions()
        options.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: false)]
        options.fetchLimit = 1
        guard let asset = PHAsset.fetchAssets(with: options).firstObject,
              let resource = PHAssetResource.assetResources(for: asset).first(where: { $0.type == .photo || $0.type == .video })
        else {
            message = "相册里没有照片"
            return
        }
        let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: file) }
        do {
            let opts = PHAssetResourceRequestOptions()
            opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
            try await PHAssetResourceManager.default().writeData(for: resource, toFile: file, options: opts)
            guard let url = URL(string: server + "/api/ingest") else {
                message = "服务端地址不对：\(server)"
                return
            }
            var req = URLRequest(url: url)
            req.httpMethod = "POST"
            req.setValue(encodedName(resource.originalFilename), forHTTPHeaderField: "X-File-Name")
            req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
            let (data, resp) = try await URLSession.shared.upload(for: req, fromFile: file)
            let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
            message = "\(resource.originalFilename)：HTTP \(status) \(String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespaces))"
        } catch {
            message = "上传失败：\(error.localizedDescription)"
        }
    }
}

// 文件名 URL 编码后放进 X-File-Name（请求头放不了中文，服务端会解码）。
func encodedName(_ name: String) -> String {
    name.addingPercentEncoding(withAllowedCharacters: .alphanumerics.union(CharacterSet(charactersIn: "-._~"))) ?? name
}
