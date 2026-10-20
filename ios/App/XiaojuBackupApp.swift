import Photos
import SwiftUI

// 小桔备份（测试版）：打开 PhotoKit 的后台上传扩展（iOS 26.1 起苹果开放给第三方备份 App 的接口），
// 之后系统在后台把新拍的照片、视频交给扩展（Extension/BackgroundUploadExtension.swift）传进小桔云盘的「相册」。
// 这一版只验证流程：锁屏、切到别的 App 时能不能自动传。
@main
struct XiaojuBackupApp: App {
    var body: some Scene {
        WindowGroup { ContentView() }
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
