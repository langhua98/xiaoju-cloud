import QuickLook
import SwiftUI
import UniformTypeIdentifiers

// 「文件」页：像「文件」App 一样一层层浏览云盘。顶上搜索当前文件夹、按类型筛选；
// 右上角「＋」上传文件、新建文件夹（都放进当前文件夹）；左滑删除，长按重命名、分享链接。
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
    }

    private func row(_ item: CloudItem) -> some View {
        Button {
            Task { await open(item) }
        } label: {
            HStack(spacing: 12) {
                icon(item)
                VStack(alignment: .leading, spacing: 2) {
                    Text(item.name).lineLimit(1).truncationMode(.middle)
                    Text(detail(item)).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if item.isPrivate ?? false {
                    Image(systemName: "lock.fill").font(.caption).foregroundStyle(.secondary)
                }
                if item.isFolder {
                    Image(systemName: "chevron.right").font(.caption).foregroundStyle(.tertiary)
                }
            }
        }
        .buttonStyle(.plain)
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
        return "\(sizeText(item.size)) · \(item.date.formatted(date: .abbreviated, time: .shortened))"
    }

    // MARK: 动作

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
            let link = try await Cloud.shared.link(for: item)
            guard let url = Cloud.shared.absolute(link.url) else { return }
            let (temp, _) = try await URLSession.shared.download(from: url)
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let file = dir.appendingPathComponent(item.name)
            try FileManager.default.moveItem(at: temp, to: file)
            preview = file
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
