import Photos
import SwiftUI

// 「我的」页：网盘用了多少、备份状态，以及不常用的功能：分享的链接、回收站、重复文件、释放手机空间、
// 恢复到手机、在网页里打开、退出登录。
struct MeView: View {
    @ObservedObject private var cloud = Cloud.shared
    @ObservedObject private var auto = AutoBackup.shared
    @State private var stats: DriveStats?
    @State private var showWeb = false

    var body: some View {
        NavigationStack {
            List {
                if cloud.signedIn { Section { storage } }
                Section {
                    // 推进去（不是全屏盖住），右滑就回到「我的」
                    NavigationLink { BackupView(pushed: true) } label: {
                        HStack {
                            BackupRing().allowsHitTesting(false)
                            VStack(alignment: .leading) {
                                Text(auto.running ? "备份中" : auto.failures.isEmpty ? "备份" : "\(auto.failures.count) 个没传上")
                                Text(auto.running ? "查看进度" : auto.lastFinished.map { "上次 \($0.formatted(.relative(presentation: .named)))" } ?? "还没备份过")
                                    .font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    }
                }
                Section {
                    if cloud.signedIn {
                        NavigationLink { SharesView() } label: { TileLabel("分享的链接", symbol: "link", color: .indigo) }
                        NavigationLink { TrashView() } label: { TileLabel("回收站", symbol: "trash.fill", color: .gray) }
                        NavigationLink { DuplicatesView() } label: { TileLabel("重复文件", symbol: "square.on.square.fill", color: .purple) }
                    }
                    NavigationLink { FreeSpaceView() } label: { TileLabel("释放手机空间", symbol: "iphone", color: .green) }
                    Button { Restorer.shared.show = true } label: { TileLabel("恢复到这台手机", symbol: "clock.arrow.circlepath", color: .teal) }
                }
                Section {
                    Button { showWeb = true } label: { TileLabel("在网页里打开", symbol: "safari", color: .blue) }
                    if cloud.signedIn {
                        Button(role: .destructive) { cloud.signOut() } label: { TileLabel("退出登录", symbol: "rectangle.portrait.and.arrow.right", color: .red, titleColor: .red) }
                    }
                } footer: {
                    Text("退出登录只影响在 App 里看照片、文件，自动备份照常跑。")
                }
                Section {
                    LabeledContent("版本", value: Self.version)
                }
            }
            .navigationTitle("我的")
            .refreshable { await Task { await load() }.value }
            .task { await load() }
            .sheet(isPresented: $showWeb) {
                if let url = cloud.webURL { SafariView(url: url) { showWeb = false }.ignoresSafeArea() }
            }
        }
    }

    // 「0.1（构建 32）」：构建号就是打包时工作流的运行次数，和 Release「ios-test」、安装包文件名里的对得上
    static let version: String = {
        let info = Bundle.main.infoDictionary ?? [:]
        let short = info["CFBundleShortVersionString"] as? String ?? "?"
        let build = info["CFBundleVersion"] as? String ?? "?"
        return "\(short)（构建 \(build)）"
    }()

    private var storage: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline) {
                Text(stats.map { "已用 \(sizeText($0.bytes))" } ?? "已用 —").font(.title3.bold())
                Spacer()
                Text("Telegram 频道 · 不限容量").font(.caption).foregroundStyle(.secondary)
            }
            if let s = stats, s.bytes > 0 {
                GeometryReader { geo in
                    HStack(spacing: 0) {
                        Rectangle().fill(Color.orange).frame(width: geo.size.width * CGFloat(s.photoBytes) / CGFloat(s.bytes))
                        Rectangle().fill(Color.blue).frame(width: geo.size.width * CGFloat(s.videoBytes) / CGFloat(s.bytes))
                        Rectangle().fill(Color.green)
                    }
                }
                .frame(height: 8)
                .clipShape(Capsule())
                Text("照片 \(s.photos.formatted()) 张 · \(sizeText(s.photoBytes))　视频 \(s.videos.formatted()) 个 · \(sizeText(s.videoBytes))　其他 \(s.others.formatted()) 个")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 4)
    }

    private func load() async {
        guard cloud.signedIn else { return }
        stats = try? await cloud.get("/api/stats", as: DriveStats.self)
    }
}

// 分享的链接：看谁分享了什么、看过几次，可以复制链接或取消（取消后立刻失效）。提取码只在生成时显示。
struct SharesView: View {
    @State private var shares: [CloudShare] = []
    @State private var error = ""

    var body: some View {
        List {
            if !error.isEmpty { Text(error).foregroundStyle(.red) }
            ForEach(shares) { share in
                VStack(alignment: .leading, spacing: 4) {
                    Text(share.file?.name ?? "已删除的文件").lineLimit(1)
                    Text(detail(share)).font(.caption).foregroundStyle(share.expired ? .red : .secondary)
                }
                .swipeActions {
                    Button("取消分享", role: .destructive) { Task { await remove(share) } }
                    Button("复制") { UIPasteboard.general.string = Cloud.shared.shareURL(share) }.tint(.orange)
                }
                .contextMenu {
                    Button("复制链接", systemImage: "doc.on.doc") { UIPasteboard.general.string = Cloud.shared.shareURL(share) }
                    Button("取消分享", systemImage: "xmark", role: .destructive) { Task { await remove(share) } }
                }
            }
            if shares.isEmpty && error.isEmpty {
                Text("还没有分享过。在照片或文件上点「分享链接」。").foregroundStyle(.secondary)
            }
        }
        .navigationTitle("分享的链接")
        .refreshable { await Task { await load() }.value }
        .task { await load() }
    }

    private func detail(_ share: CloudShare) -> String {
        var parts: [String] = []
        if share.expired {
            parts.append("已过期")
        } else {
            parts.append(share.expiresAt.map { "\($0.formatted(date: .abbreviated, time: .omitted)) 失效" } ?? "永久有效")
        }
        if share.hasPassword { parts.append("有提取码") }
        parts.append("看过 \(share.views) 次")
        return parts.joined(separator: " · ")
    }

    private func load() async {
        do {
            shares = try await Cloud.shared.get("/api/shares", as: ShareList.self).items
            error = ""
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func remove(_ share: CloudShare) async {
        do {
            try await Cloud.shared.send("DELETE", "/api/shares/\(share.id)")
            shares.removeAll { $0.id == share.id }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct ShareList: Decodable {
    let items: [CloudShare]
}

// 回收站：恢复，或者永久删除（同时删掉 Telegram 里的数据，找不回来）
struct TrashView: View {
    @State private var items: [CloudItem] = []
    @State private var error = ""
    @State private var purging: CloudItem?

    var body: some View {
        List {
            if !error.isEmpty { Text(error).foregroundStyle(.red) }
            ForEach(items) { item in
                HStack(spacing: 12) {
                    IconTile(symbol: FileTile.symbol(item), color: FileTile.color(item), size: 32)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(item.name).lineLimit(1).truncationMode(.middle)
                        Text(item.trashedAt.map { "\($0.formatted(.relative(presentation: .named)))删除" } ?? "")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
                .swipeActions {
                    Button("永久删除", role: .destructive) { purging = item }
                    Button("恢复") { Task { await restore(item) } }.tint(.green)
                }
            }
            if items.isEmpty && error.isEmpty {
                Text("回收站是空的").foregroundStyle(.secondary)
            }
        }
        .navigationTitle("回收站")
        .refreshable { await Task { await load() }.value }
        .task { await load() }
        .confirmationDialog("永久删除「\(purging?.name ?? "")」？", isPresented: Binding(get: { purging != nil }, set: { if !$0 { purging = nil } }),
                            titleVisibility: .visible) {
            Button("永久删除", role: .destructive) {
                if let item = purging { Task { await purge(item) } }
            }
        } message: {
            Text("Telegram 里的数据也会删掉，删了就找不回来了。")
        }
    }

    private func load() async {
        do {
            items = try await Cloud.shared.get("/api/trash", as: ItemList.self).items
            error = ""
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func restore(_ item: CloudItem) async {
        do {
            try await Cloud.shared.send("POST", "/api/files/\(item.id)/restore")
            items.removeAll { $0.id == item.id }
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func purge(_ item: CloudItem) async {
        purging = nil
        do {
            try await Cloud.shared.send("DELETE", "/api/files/\(item.id)?permanent=1")
            items.removeAll { $0.id == item.id }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// 重复文件：内容完全一样的文件放一组，每组留一份，其他的可以左滑移进回收站
struct DuplicatesView: View {
    @State private var groups: [DuplicateGroup] = []
    @State private var extra: Int64 = 0
    @State private var loading = true
    @State private var error = ""
    @State private var removed = Set<String>()

    var body: some View {
        List {
            if !error.isEmpty { Text(error).foregroundStyle(.red) }
            if !loading {
                Text(groups.isEmpty ? "没有重复的文件" : "\(groups.count) 组重复，删掉多余的可以省 \(sizeText(extra))")
                    .font(.footnote).foregroundStyle(.secondary)
            }
            ForEach(groups) { group in
                Section("\(group.files.count) 份 · 每份 \(sizeText(group.size))") {
                    ForEach(group.files.filter { !removed.contains($0.id) }) { file in
                        HStack(spacing: 12) {
                            Thumb(item: file.item).frame(width: 40, height: 40).clipShape(RoundedRectangle(cornerRadius: 6))
                            VStack(alignment: .leading, spacing: 2) {
                                Text(file.item.name).lineLimit(1).truncationMode(.middle)
                                Text(file.path).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.head)
                            }
                        }
                        .swipeActions {
                            Button("移进回收站", role: .destructive) { Task { await remove(file) } }
                        }
                    }
                }
            }
        }
        .navigationTitle("重复文件")
        .overlay { if loading { ProgressView() } }
        .refreshable { await Task { await load() }.value }
        .task { await load() }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            let list = try await Cloud.shared.get("/api/duplicates", as: DuplicateList.self)
            groups = list.groups
            extra = list.extraBytes
            removed = []
            error = ""
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func remove(_ file: DuplicateFile) async {
        do {
            try await Cloud.shared.trash(file.item)
            removed.insert(file.id)
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct DuplicateList: Decodable {
    let groups: [DuplicateGroup]
    let extraBytes: Int64

    enum CodingKeys: String, CodingKey {
        case groups
        case extraBytes = "extra_bytes"
    }
}

struct DuplicateGroup: Decodable, Identifiable {
    let sha256: String
    let size: Int64
    let files: [DuplicateFile]
    var id: String { sha256 }
}

// 服务端把文件的字段和 path 放在同一层
struct DuplicateFile: Decodable, Identifiable {
    let item: CloudItem
    let path: String
    var id: String { item.id }

    enum CodingKeys: String, CodingKey { case path }

    init(from decoder: Decoder) throws {
        item = try CloudItem(from: decoder)
        path = try decoder.container(keyedBy: CodingKeys.self).decodeIfPresent(String.self, forKey: .path) ?? ""
    }
}

// 释放手机空间：已经确认在云盘里的照片、视频，可以从手机上删掉（先进手机的「最近删除」，30 天内能找回）。
struct FreeSpaceView: View {
    @State private var olderThan = 30
    @State private var assets: [PHAsset] = []
    @State private var bytes: Int64 = 0
    @State private var checking = false
    @State private var message = ""

    var body: some View {
        List {
            Section {
                Picker("删多久以前的", selection: $olderThan) {
                    Text("30 天").tag(30)
                    Text("90 天").tag(90)
                    Text("1 年").tag(365)
                    Text("全部").tag(0)
                }
                .pickerStyle(.segmented)
            } footer: {
                Text("只删已经核对过、确实在云盘里的；收藏的照片不删。删掉的先进手机「照片」的「最近删除」，30 天内能找回。云盘里的不受影响。")
            }
            Section {
                if checking {
                    HStack { ProgressView(); Text("正在和云盘核对…") }
                } else {
                    LabeledContent("可以删", value: "\(assets.count) 项")
                    LabeledContent("大约能省", value: sizeText(bytes))
                    Button("从手机删除 \(assets.count) 项", role: .destructive) { Task { await delete() } }
                        .disabled(assets.isEmpty)
                }
                if !message.isEmpty { Text(message).font(.footnote) }
            }
        }
        .navigationTitle("释放手机空间")
        .task(id: olderThan) { await check() }
    }

    private func check() async {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else {
            message = "需要「所有照片」权限，在「备份」页里打开。"
            return
        }
        checking = true
        defer { checking = false }
        let days = olderThan
        let found = await Task.detached(priority: .userInitiated) { () -> ([PHAsset], Int64) in
            await AutoBackup.shared.reconcileAll() // 先和云盘再核对一次
            let before = days == 0 ? Date.distantFuture : Date().addingTimeInterval(-Double(days) * 86400)
            let list = AutoBackup.shared.fullyBackedUp(before: before)
            var total: Int64 = 0
            for asset in list {
                for r in PHAssetResource.assetResources(for: asset) {
                    total += (r.value(forKey: "fileSize") as? Int64) ?? 0
                }
            }
            return (list, total)
        }.value
        assets = found.0
        bytes = found.1
    }

    // 系统会再弹一次确认
    private func delete() async {
        let list = assets
        do {
            try await PHPhotoLibrary.shared().performChanges {
                PHAssetChangeRequest.deleteAssets(list as NSArray)
            }
            message = "已删除 \(list.count) 项，在「照片 → 最近删除」里能找回。"
            await check()
        } catch {
            message = "没有删除：\(error.localizedDescription)"
        }
    }
}
