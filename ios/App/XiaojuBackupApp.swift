import Photos
import SafariServices
import SwiftUI
import UniformTypeIdentifiers

// 小桔备份（测试版）：打开 PhotoKit 的后台上传扩展（iOS 26.1 起苹果开放给第三方备份 App 的接口），
// 之后系统在后台把新拍的照片、视频交给扩展（Extension/BackgroundUploadExtension.swift）传进小桔云盘的「相册」。
// 底栏按内容分：「照片」「相簿」「文件」直接看云盘（登录见 LoginView.swift），「我的」放空间、回收站这些不常用的。
// 备份不占底栏：每页右上角的进度环（BackupRing）点开就是备份中心（BackupView.swift）。
@main
struct XiaojuBackupApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @ObservedObject private var restorer = Restorer.shared
    var body: some Scene {
        WindowGroup {
            RootView()
                .sheet(isPresented: $restorer.show) { RestoreView() }
                .onOpenURL { url in
                    // 快捷指令「充电时备份到小桔云盘」：xiaojubackup://backup
                    if url.host == "backup" { AutoBackup.shared.start(reason: "快捷指令") }
                    // 网页「恢复到手机」：xiaojubackup://restore?server=…&token=…
                    if url.host == "restore" { Restorer.shared.open(url) }
                }
        }
    }
}

struct RootView: View {
    @ObservedObject private var cloud = Cloud.shared

    var body: some View {
        TabView {
            signedIn { PhotosView() }
                .tabItem { Label("照片", systemImage: "photo.on.rectangle") }
            signedIn { CloudAlbumsView() }
                .tabItem { Label("相簿", systemImage: "rectangle.stack") }
            signedIn { FilesView() }
                .tabItem { Label("文件", systemImage: "folder") }
            MeView()
                .tabItem { Label("我的", systemImage: "person.crop.circle") }
        }
        .tint(.brand)
        .sheet(item: $cloud.gridPrompt) { _ in GridGateView() }
    }

    // 没登录（或登录过期）时先显示登录页；备份照常可以从右上角进去
    @ViewBuilder
    private func signedIn<Content: View>(@ViewBuilder _ content: () -> Content) -> some View {
        if cloud.signedIn {
            content()
        } else {
            NavigationStack {
                LoginView().toolbar { BackupRing() }
            }
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
