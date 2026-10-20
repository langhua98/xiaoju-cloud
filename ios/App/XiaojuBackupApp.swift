import Photos
import SwiftUI
import WebKit

// 小桔备份（测试版）：打开 PhotoKit 的后台上传扩展（iOS 26.1 起苹果开放给第三方备份 App 的接口），
// 之后系统在后台把新拍的照片、视频交给扩展（Extension/BackgroundUploadExtension.swift）传进小桔云盘的「相册」。
// 「云盘」页直接打开网盘网页（浏览、下载、分享都用网页的功能），「相册」页按手机相册原样上传，「备份」页管后台自动备份。
@main
struct XiaojuBackupApp: App {
    var body: some Scene {
        WindowGroup {
            TabView {
                DriveView().tabItem { Label("云盘", systemImage: "externaldrive") }
                AlbumsView().tabItem { Label("相册", systemImage: "photo.on.rectangle") }
                ContentView().tabItem { Label("备份", systemImage: "arrow.up.circle") }
            }
        }
    }
}

struct DriveView: View {
    private var url: URL? {
        URL(string: Bundle.main.object(forInfoDictionaryKey: "XiaojuWebURL") as? String ?? "")
    }

    var body: some View {
        if let url {
            WebView(url: url).ignoresSafeArea(edges: .bottom)
        } else {
            Text("网页地址没配置")
        }
    }
}

// 网页登录信息存在 WKWebView 默认的持久存储里，关掉 App 再开不用重新登录。
// 网页里的下载链接交给系统下载，下载完弹出分享面板（存到「文件」或相册）。
struct WebView: UIViewRepresentable {
    let url: URL

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.allowsInlineMediaPlayback = true
        let view = WKWebView(frame: .zero, configuration: config)
        view.navigationDelegate = context.coordinator
        view.uiDelegate = context.coordinator
        view.allowsBackForwardNavigationGestures = true
        let refresh = UIRefreshControl()
        refresh.addTarget(context.coordinator, action: #selector(Coordinator.reload(_:)), for: .valueChanged)
        view.scrollView.refreshControl = refresh
        context.coordinator.webView = view
        view.load(URLRequest(url: url))
        return view
    }

    func updateUIView(_ view: WKWebView, context: Context) {}

    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate {
        weak var webView: WKWebView?
        private var files: [ObjectIdentifier: URL] = [:]

        @objc func reload(_ sender: UIRefreshControl) {
            webView?.reload()
            sender.endRefreshing()
        }

        func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction) async -> WKNavigationActionPolicy {
            action.shouldPerformDownload ? .download : .allow
        }

        func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse) async -> WKNavigationResponsePolicy {
            response.canShowMIMEType ? .allow : .download
        }

        func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
            download.delegate = self
        }

        func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
            download.delegate = self
        }

        // 网页里 target=_blank 的链接（比如分享页）在当前页打开
        func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                     for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
            webView.load(action.request)
            return nil
        }

        func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String) async -> URL? {
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let file = dir.appendingPathComponent(suggestedFilename)
            files[ObjectIdentifier(download)] = file
            return file
        }

        func downloadDidFinish(_ download: WKDownload) {
            guard let file = files.removeValue(forKey: ObjectIdentifier(download)) else { return }
            Task { @MainActor in self.share(file) }
        }

        func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
            files.removeValue(forKey: ObjectIdentifier(download))
        }

        @MainActor private func share(_ file: URL) {
            guard let view = webView, let root = view.window?.rootViewController else { return }
            var top = root
            while let next = top.presentedViewController { top = next }
            let sheet = UIActivityViewController(activityItems: [file], applicationActivities: nil)
            sheet.popoverPresentationController?.sourceView = view
            top.present(sheet, animated: true)
        }
    }
}

struct ContentView: View {
    @State private var auth = PHPhotoLibrary.authorizationStatus(for: .readWrite)
    @State private var enabled = PHPhotoLibrary.shared().uploadJobExtensionEnabled
    @State private var message = ""
    @State private var busy = false

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
                    Toggle("后台自动备份", isOn: Binding(get: { enabled }, set: { setEnabled($0) }))
                        .disabled(auth != .authorized)
                } header: {
                    Text("后台上传")
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
