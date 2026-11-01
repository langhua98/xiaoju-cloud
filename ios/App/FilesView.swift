import QuickLook
import SwiftUI
import UniformTypeIdentifiers

// 「文件」页：像「文件」App 一样一层层浏览云盘。顶上搜索当前文件夹、按类型筛选；
// 右上角「＋」上传文件、新建文件夹（都放进当前文件夹）；文件右边有分享（直链）、下载（存到「文件」）两个按钮；
// 左滑删除，长按重命名、分享、存到「照片」「文件」。
struct FilesView: View {
    var body: some View {
        NavigationStack {
            FolderView(folder: nil)
        }
    }
}

struct FolderView: View {
    let folder: CloudItem?

    @State private var items: [CloudItem] = []
    @State private var loading = true
    @State private var error = ""
    @State private var query = ""
    @State private var kind = FileKind.all
    @State private var importing = false
    @State private var naming: Naming?
    @State private var newName = ""
    @State private var uploading = ""
    @State private var viewing: CloudItem?
    @State private var sharing: CloudItem?
    @State private var preview: URL?
    @State private var opened: CloudItem?
    @State private var progress: Double? // 下载进度，nil 时不显示进度条
    @State private var exporting: URL?   // 下载好、等用户在「存储到」里选位置的文件
    @State private var downloading: Task<Void, Never>?
    @State private var downloadingID: String? // 正在下载的文件，它的下载按钮变成进度环，点一下取消
    @State private var status = ""            // 下载进度、完成提示

    // 新建文件夹或重命名时弹出的输入框
    enum Naming: Identifiable {
        case newFolder
        case rename(CloudItem)
        var id: String {
            switch self {
            case .newFolder: return "new"
            case .rename(let item): return item.id
            }
        }
    }

    private var namingTitle: String {
        guard let naming else { return "" }
        if case .newFolder = naming { return "新建文件夹" }
        return "重命名"
    }

    private var shown: [CloudItem] {
        items.filter { item in
            (query.isEmpty || item.name.localizedCaseInsensitiveContains(query))
                && (kind == .all || item.isFolder || item.kind == kind)
        }
    }

    var body: some View {
        List {
            Section {
                Picker("类型", selection: $kind) {
                    ForEach(FileKind.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .listRowSeparator(.hidden)
            }
            if !uploading.isEmpty {
                Label(uploading, systemImage: "arrow.up.circle").font(.footnote)
            }
            if let progress, !status.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    HStack {
                        Label(status, systemImage: "arrow.down.circle").font(.footnote).lineLimit(1)
                        Spacer()
                        Button("取消") { downloading?.cancel() }.font(.footnote).buttonStyle(.borderless)
                    }
                    ProgressView(value: progress).tint(.brand)
                }
            } else if !status.isEmpty {
                Label(status, systemImage: "checkmark.circle").font(.footnote)
            }
            if !error.isEmpty {
                Text(error).font(.footnote).foregroundStyle(.red)
            }
            ForEach(shown) { item in
                row(item)
                    .swipeActions {
                        Button(role: .destructive) { Task { await remove(item) } } label: { Label("删除", systemImage: "trash") }
                        Button { sharing = item } label: { Label("分享", systemImage: "link") }.tint(.brand)
                    }
                    .contextMenu {
                        Button("重命名", systemImage: "pencil") {
                            newName = item.name
                            naming = .rename(item)
                        }
                        Button("分享链接", systemImage: "link") { sharing = item }
                        if !item.isFolder {
                            Section {
                                if item.isMedia {
                                    Button("存到「照片」", systemImage: "photo.badge.arrow.down") { startDownload(item, toPhotos: true) }
                                }
                                Button("存到「文件」", systemImage: "folder.badge.plus") { startDownload(item, toPhotos: false) }
                            }
                            .disabled(downloading != nil)
                        }
                        Button("移进回收站", systemImage: "trash", role: .destructive) { Task { await remove(item) } }
                    }
            }
            if shown.isEmpty && !loading {
                Text(query.isEmpty ? "这里是空的。点右上角 ＋ 上传。" : "没有找到「\(query)」")
                    .font(.callout).foregroundStyle(.secondary)
            }
        }
        .listStyle(.plain)
        .overlay { if loading && items.isEmpty { ProgressView() } }
        .searchable(text: $query, prompt: "搜索这个文件夹")
        .navigationTitle(folder?.name ?? "文件")
        .toolbar {
            Menu {
                Button("上传文件", systemImage: "doc.badge.plus") { importing = true }
                Button("新建文件夹", systemImage: "folder.badge.plus") {
                    newName = ""
                    naming = .newFolder
                }
            } label: {
                Image(systemName: "plus")
            }
            if folder == nil { BackupRing() }
        }
        .refreshable { await Task { await load() }.value }
        .task { await load() }
        .navigationDestination(item: $opened) { FolderView(folder: $0) }
        .fileImporter(isPresented: $importing, allowedContentTypes: [.item], allowsMultipleSelection: true) { result in
            if case .success(let urls) = result { Task { await upload(urls) } }
        }
        .alert(namingTitle, isPresented: Binding(get: { naming != nil }, set: { if !$0 { naming = nil } })) {
            TextField("名字", text: $newName)
            Button("取消", role: .cancel) { naming = nil }
            Button("好") { Task { await applyName() } }
        }
        .fullScreenCover(item: $viewing) { item in
            MediaViewer(items: shown.filter(\.isMedia), start: item) { gone in items.removeAll { $0.id == gone.id } }
        }
        .sheet(item: $sharing) { ShareLinkSheet(item: $0) }
        .quickLookPreview($preview)
        .fileMover(isPresented: Binding(get: { exporting != nil }, set: { if !$0 { cleanExport() } }), file: exporting) { result in
            switch result {
            case .success(let dest): flash("已存到「文件」：\(dest.deletingLastPathComponent().lastPathComponent)")
            case .failure(let error): self.error = error.localizedDescription
            }
            cleanExport()
        }
    }

    // 一行：点名字打开；文件右边两个按钮，分享（生成直链）、下载（存到「文件」）
    private func row(_ item: CloudItem) -> some View {
        HStack(spacing: 12) {
            Button {
                Task { await open(item) }
            } label: {
                HStack(spacing: 12) {
                    icon(item)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(item.name).lineLimit(1).truncationMode(.middle)
                        Text(detail(item)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer(minLength: 0)
                    if item.isPrivate ?? false {
                        Image(systemName: "lock.fill").font(.caption).foregroundStyle(.secondary)
                    }
                    if item.isFolder {
                        Image(systemName: "chevron.right").font(.caption).foregroundStyle(.tertiary)
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            if !item.isFolder {
                HStack(spacing: 8) {
                    Button { sharing = item } label: { RowAction(symbol: "link") }
                        .accessibilityLabel("分享直链")
                    Button {
                        if downloadingID == item.id { downloading?.cancel() } else { startDownload(item, toPhotos: false) }
                    } label: {
                        RowAction(symbol: downloadingID == item.id ? "xmark" : "arrow.down",
                                  progress: downloadingID == item.id ? progress : nil)
                    }
                    .accessibilityLabel(downloadingID == item.id ? "取消下载" : "下载到「文件」")
                    .disabled(downloading != nil && downloadingID != item.id)
                }
                .buttonStyle(RowActionPress())
            }
        }
    }

    @ViewBuilder
    private func icon(_ item: CloudItem) -> some View {
        if item.thumb != nil && !(item.isPrivate ?? false) {
            Thumb(item: item).frame(width: 40, height: 40).clipShape(RoundedRectangle(cornerRadius: 8))
        } else {
            IconTile(symbol: FileTile.symbol(item), color: FileTile.color(item), size: 40, glyph: FileTile.glyph(item))
        }
    }

    private func detail(_ item: CloudItem) -> String {
        if item.isFolder, let c = item.counts {
            return "\(c.photos + c.videos + c.others) 项"
        }
        if item.isFolder { return item.date.formatted(date: .abbreviated, time: .omitted) }
        // 右边有两个按钮，日期写短一点（今年的不写年份），一行放得下
        let sameYear = Calendar.current.isDate(item.date, equalTo: Date(), toGranularity: .year)
        let date = sameYear
            ? item.date.formatted(.dateTime.month(.defaultDigits).day().hour().minute())
            : item.date.formatted(.dateTime.year().month(.defaultDigits).day())
        return "\(sizeText(item.size)) · \(date)"
    }

    // MARK: 动作

    // 长按「存到「照片」」「存到「文件」」：先下载（显示进度、能取消），照片直接存进相册，
    // 其他交给系统的「存储到」让用户选文件夹（我的 iPhone、iCloud 云盘……）
    private func startDownload(_ item: CloudItem, toPhotos: Bool) {
        guard downloading == nil else { return }
        let total = sizeText(item.size)
        status = "正在下载 \(item.name)"
        progress = 0
        downloadingID = item.id
        downloading = Task {
            defer { downloading = nil; downloadingID = nil; progress = nil }
            do {
                if toPhotos {
                    try await MediaSaver.save(item) { p in
                        progress = p
                        status = "正在下载 \(item.name) · \(Int(p * 100))% / \(total)"
                    }
                    flash("已存到手机的「照片」")
                } else {
                    let file = try await Cloud.shared.download(item) { p in
                        progress = p
                        status = "正在下载 \(item.name) · \(Int(p * 100))% / \(total)"
                    }
                    status = ""
                    exporting = file
                }
            } catch {
                status = ""
                if !error.isCancellation { self.error = "\(item.name)：\(error.localizedDescription)" }
            }
        }
    }

    // 完成提示显示几秒后消失
    private func flash(_ text: String) {
        status = text
        Task {
            try? await Task.sleep(for: .seconds(3))
            if status == text { status = "" }
        }
    }

    // 「存储到」关掉后删掉临时目录（移走成功时已经不在了）
    private func cleanExport() {
        if let exporting { try? FileManager.default.removeItem(at: exporting.deletingLastPathComponent()) }
        exporting = nil
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            items = try await Cloud.shared.children(of: folder?.id)
            error = ""
        } catch {
            if !error.isCancellation { self.error = error.localizedDescription }
        }
    }

    private func open(_ item: CloudItem) async {
        if item.isFolder {
            if item.isPrivate ?? false {
                guard await PrivateLock.unlock(reason: "打开「\(item.name)」") else { return }
            }
            opened = item
            return
        }
        if item.isMedia {
            viewing = item
            return
        }
        // 其他文件：下载到临时目录，用系统的快速查看打开（可以再分享、存到「文件」）
        do {
            uploading = "正在打开 \(item.name)…"
            defer { uploading = "" }
            preview = try await Cloud.shared.download(item) { uploading = "正在打开 \(item.name) · \(Int($0 * 100))%" }
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func upload(_ urls: [URL]) async {
        for (i, url) in urls.enumerated() {
            uploading = "正在上传 \(i + 1)/\(urls.count)：\(url.lastPathComponent)"
            let access = url.startAccessingSecurityScopedResource()
            defer { if access { url.stopAccessingSecurityScopedResource() } }
            do {
                try await Cloud.shared.upload(file: url, name: url.lastPathComponent, parent: folder?.id)
            } catch {
                self.error = "\(url.lastPathComponent)：\(error.localizedDescription)"
            }
        }
        uploading = ""
        await load()
    }

    private func applyName() async {
        let name = newName.trimmingCharacters(in: .whitespaces)
        let action = naming
        naming = nil
        guard !name.isEmpty, let action else { return }
        do {
            switch action {
            case .newFolder:
                var body: [String: Any] = ["name": name]
                if let folder { body["parent_id"] = folder.id }
                try await Cloud.shared.send("POST", "/api/folders", json: body)
            case .rename(let item):
                try await Cloud.shared.send("PATCH", "/api/files/\(item.id)", json: ["name": name])
            }
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func remove(_ item: CloudItem) async {
        do {
            try await Cloud.shared.trash(item)
            items.removeAll { $0.id == item.id }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// 文件行右边的小圆按钮：和顶上的返回、＋按钮一个样子，浅橙底（深色模式是深橙棕）配品牌橙图标；
// 下载中外圈画进度环
private struct RowAction: View {
    let symbol: String
    var progress: Double?

    @Environment(\.isEnabled) private var enabled

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: 13, weight: .bold))
            .foregroundStyle(Color.brand)
            .frame(width: 32, height: 32)
            .background(Circle().fill(Color.folderTile))
            .overlay {
                if let progress {
                    Circle().trim(from: 0, to: max(0.02, progress))
                        .stroke(Color.brand, style: StrokeStyle(lineWidth: 2.5, lineCap: .round))
                        .rotationEffect(.degrees(-90))
                        .padding(1.25)
                        .animation(.linear(duration: 0.2), value: progress)
                }
            }
            .opacity(enabled ? 1 : 0.4)
            .contentShape(Circle())
    }
}

private struct RowActionPress: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .scaleEffect(configuration.isPressed ? 0.88 : 1)
            .animation(.spring(duration: 0.2), value: configuration.isPressed)
    }
}
