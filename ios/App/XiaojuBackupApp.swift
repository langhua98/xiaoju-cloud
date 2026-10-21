import Photos
import SwiftUI
import SafariServices

// 小桔备份（测试版）：打开 PhotoKit 的后台上传扩展（iOS 26.1 起苹果开放给第三方备份 App 的接口），
// 之后系统在后台把新拍的照片、视频交给扩展（Extension/BackgroundUploadExtension.swift）传进小桔云盘的「相册」。
// 「云盘」页直接打开网盘网页（浏览、下载、分享都用网页的功能），「备份」页（BackupView.swift）管备份：一个「自动备份」开关，
// 进度、没传上去的、备份哪些相册都在这一页。
@main
struct XiaojuBackupApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    var body: some Scene {
        WindowGroup {
            TabView {
                DriveView().tabItem { Label("云盘", systemImage: "externaldrive") }
                BackupView().tabItem { Label("备份", systemImage: "arrow.triangle.2.circlepath.icloud") }
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
