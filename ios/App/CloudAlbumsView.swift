import LocalAuthentication
import SwiftUI

// 「相簿」页：云盘「相册」下面的每个相册（手机上按相册备份的、网页上建的），加上「隐藏相册」和其他私密文件夹。
// 私密的封面打码，点开要先过 Face ID。
struct CloudAlbumsView: View {
    @ObservedObject private var cloud = Cloud.shared
    @State private var albums: [CloudItem] = []
    @State private var hidden: [CloudItem] = []
    @State private var error = ""
    @State private var unlocked = Set<String>()
    @State private var opened: CloudItem?

    private let columns = [GridItem(.flexible(), spacing: 14), GridItem(.flexible(), spacing: 14)]

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    if !error.isEmpty { Text(error).font(.footnote).foregroundStyle(.red) }
                    if albums.isEmpty && hidden.isEmpty && error.isEmpty {
                        Text("还没有相簿。在「备份 → 备份哪些照片」里打开手机上的相册，就会在这里出现。")
                            .font(.callout).foregroundStyle(.secondary)
                    }
                    LazyVGrid(columns: columns, spacing: 18) {
                        ForEach(albums) { card($0) }
                    }
                    if !hidden.isEmpty {
                        Text("私密").font(.title3.bold())
                        LazyVGrid(columns: columns, spacing: 18) {
                            ForEach(hidden) { card($0) }
                        }
                    }
                }
                .padding(16)
            }
            .navigationTitle("相簿")
            .toolbar { BackupRing() }
            .refreshable { await Task { await load() }.value }
            .task { if albums.isEmpty { await load() } }
            .navigationDestination(item: $opened) { AlbumDetailView(album: $0) }
        }
    }

    private func isLocked(_ album: CloudItem) -> Bool {
        (album.isPrivate ?? false || album.name == "隐藏相册") && !unlocked.contains(album.id)
    }

    private func card(_ album: CloudItem) -> some View {
        Button {
            Task {
                if isLocked(album) {
                    guard await PrivateLock.unlock(reason: "查看「\(album.name)」") else { return }
                    unlocked.insert(album.id)
                }
                opened = album
            }
        } label: {
            VStack(alignment: .leading, spacing: 6) {
                Thumb(item: album)
                    .aspectRatio(1, contentMode: .fit)
                    .blur(radius: isLocked(album) ? 18 : 0)
                    .overlay {
                        if isLocked(album) { Image(systemName: "lock.fill").font(.title).foregroundStyle(.white) }
                    }
                    .clipShape(RoundedRectangle(cornerRadius: 10))
                Text(album.name).font(.subheadline.bold()).lineLimit(1)
                if let c = album.counts {
                    Text(countText(c)).font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .buttonStyle(.plain)
    }

    private func countText(_ c: CloudItem.Counts) -> String {
        var parts: [String] = []
        if c.photos > 0 { parts.append("\(c.photos) 张照片") }
        if c.videos > 0 { parts.append("\(c.videos) 个视频") }
        if c.others > 0 { parts.append("\(c.others) 个文件") }
        return parts.isEmpty ? "空" : parts.joined(separator: "，")
    }

    private func load() async {
        do {
            let root = try await cloud.children(of: nil)
            if let album = root.first(where: { $0.isFolder && $0.name == "相册" }) {
                albums = try await cloud.children(of: album.id).filter(\.isFolder)
            } else {
                albums = []
            }
            hidden = root.filter { $0.isFolder && ($0.name == "隐藏相册" || ($0.isPrivate ?? false)) }
            error = ""
        } catch {
            if !error.isCancellation { self.error = error.localizedDescription }
        }
    }
}

struct AlbumDetailView: View {
    let album: CloudItem
    @State private var items: [CloudItem] = []
    @State private var loading = true

    var body: some View {
        MediaGallery(items: items, title: album.name, emptyText: loading ? "正在加载…" : "这个相簿是空的", reload: load)
            .task { await load() }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        items = ((try? await Cloud.shared.children(of: album.id)) ?? []).filter(\.isMedia)
    }
}

// 私密内容：用这台手机的 Face ID / 密码确认是本人（没设密码的手机直接放行）
enum PrivateLock {
    @MainActor
    static func unlock(reason: String) async -> Bool {
        let context = LAContext()
        var error: NSError?
        guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else { return true }
        return (try? await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)) ?? false
    }
}
