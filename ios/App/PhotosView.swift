import AVKit
import Photos
import SwiftUI

// 「照片」页：云盘「相册」里的照片、视频按拍摄日期排（新的在上），学 iCloud 照片、Google 相册。
// 右上角的进度环是备份状态，点开是备份中心。双指捏合换每行张数，点「选择」多选后可以下载到手机、删除。
struct PhotosView: View {
    @ObservedObject private var cloud = Cloud.shared
    @State private var items: [CloudItem] = []
    @State private var loading = false
    @State private var error = ""

    var body: some View {
        NavigationStack {
            MediaGallery(items: items, title: "照片", emptyText: loading ? "正在加载…" : "云盘「相册」里还没有照片。打开「自动备份」后，手机上的照片会自己传上来。",
                         reload: load)
                .overlay(alignment: .top) {
                    if !error.isEmpty {
                        Text(error).font(.footnote).padding(8).background(.thinMaterial, in: Capsule()).padding(.top, 4)
                    }
                }
                .task { if items.isEmpty { await load() } }
        }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            guard let album = try await cloud.rootFolder(named: "相册") else {
                items = []
                return
            }
            items = try await cloud.children(of: album.id).filter(\.isMedia)
            error = ""
        } catch {
            if !error.isCancellation { self.error = error.localizedDescription }
        }
    }
}

// 照片网格：按天分组、捏合换列数、多选。照片页、相簿里共用。
struct MediaGallery: View {
    let items: [CloudItem]
    let title: String
    let emptyText: String
    let reload: () async -> Void

    @State private var columns = UserDefaults.standard.object(forKey: "gallery.columns") as? Int ?? 4
    @State private var selecting = false
    @State private var selected = Set<String>()
    @State private var viewing: CloudItem?
    @State private var working = ""
    @State private var confirmDelete = false
    @State private var removed = Set<String>() // 刚删掉的，等下次加载前先藏起来
    @State private var sharing: CloudItem?

    private static let steps = [3, 4, 5, 7]

    private var visible: [CloudItem] { items.filter { !removed.contains($0.id) } }

    private var days: [(day: Date, items: [CloudItem])] {
        let groups = Dictionary(grouping: visible) { Calendar.current.startOfDay(for: $0.date) }
        return groups.keys.sorted(by: >).map { day in
            (day, groups[day]!.sorted { $0.date > $1.date })
        }
    }

    var body: some View {
        ScrollView {
            if visible.isEmpty {
                Text(emptyText).font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center).padding(40)
            }
            LazyVStack(alignment: .leading, spacing: 10, pinnedViews: [.sectionHeaders]) {
                ForEach(days, id: \.day) { group in
                    Section {
                        LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 2), count: columns), spacing: 2) {
                            ForEach(group.items) { item in
                                cell(item)
                            }
                        }
                    } header: {
                        Text(dayTitle(group.day))
                            .font(.subheadline.bold())
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.horizontal, 12).padding(.vertical, 6)
                            .background(.bar)
                    }
                }
            }
            .padding(.bottom, selecting ? 80 : 0)
        }
        // 下拉刷新的任务会在界面刷新时被系统取消，正在进行的请求跟着报「cancelled」；放到单独的任务里跑完
        .refreshable {
            removed = []
            await Task { await reload() }.value
        }
        .simultaneousGesture(
            MagnifyGesture()
                .onEnded { value in zoom(value.magnification) }
        )
        .navigationTitle(selecting ? (selected.isEmpty ? "选择项目" : "已选 \(selected.count) 项") : title)
        .toolbar {
            ToolbarItem(placement: .topBarLeading) {
                if selecting {
                    Button(selected.count == visible.count ? "全不选" : "全选") {
                        selected = selected.count == visible.count ? [] : Set(visible.map(\.id))
                    }
                }
            }
            ToolbarItemGroup(placement: .topBarTrailing) {
                Button(selecting ? "完成" : "选择") {
                    selecting.toggle()
                    selected = []
                }
                if !selecting { BackupRing() }
            }
        }
        .safeAreaInset(edge: .bottom) {
            if selecting { selectionBar }
        }
        .fullScreenCover(item: $viewing) { item in
            MediaViewer(items: visible, start: item) { gone in removed.insert(gone.id) }
        }
        .sheet(item: $sharing) { ShareLinkSheet(item: $0) }
        .confirmationDialog("把 \(selected.count) 项移进回收站？", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("移进回收站", role: .destructive) { Task { await deleteSelected() } }
        } message: {
            Text("30 天内可以在「我的 → 回收站」里恢复。")
        }
    }

    private func cell(_ item: CloudItem) -> some View {
        Button {
            if selecting {
                if selected.contains(item.id) { selected.remove(item.id) } else { selected.insert(item.id) }
            } else {
                viewing = item
            }
        } label: {
            Thumb(item: item)
                .aspectRatio(1, contentMode: .fit)
                .overlay(alignment: .topTrailing) {
                    if selecting {
                        Image(systemName: selected.contains(item.id) ? "checkmark.circle.fill" : "circle")
                            .font(.title3)
                            .foregroundStyle(Color.white, selected.contains(item.id) ? Color.brand : Color.black.opacity(0.3))
                            .padding(4)
                    }
                }
                .overlay {
                    if selected.contains(item.id) { Rectangle().stroke(Color.brand, lineWidth: 3) }
                }
        }
        .buttonStyle(.plain)
        .contextMenu {
            Button("分享链接", systemImage: "link") { sharing = item }
        }
    }

    private var selectionBar: some View {
        VStack(spacing: 4) {
            if !working.isEmpty { Text(working).font(.caption).foregroundStyle(.secondary) }
            HStack {
                Button { Task { await saveSelected() } } label: { Label("存到手机", systemImage: "square.and.arrow.down") }
                Spacer()
                Button(role: .destructive) { confirmDelete = true } label: { Label("删除", systemImage: "trash") }
            }
            .disabled(selected.isEmpty || !working.isEmpty)
        }
        .padding(.horizontal, 24).padding(.vertical, 12)
        .background(.bar)
    }

    private func zoom(_ scale: CGFloat) {
        guard let i = Self.steps.firstIndex(of: columns) else { columns = 4; return }
        if scale > 1.2, i > 0 { columns = Self.steps[i - 1] }
        if scale < 0.8, i < Self.steps.count - 1 { columns = Self.steps[i + 1] }
        UserDefaults.standard.set(columns, forKey: "gallery.columns")
    }

    private func dayTitle(_ day: Date) -> String {
        let cal = Calendar.current
        if cal.isDateInToday(day) { return "今天" }
        if cal.isDateInYesterday(day) { return "昨天" }
        let zh = Locale(identifier: "zh_CN")
        if cal.component(.year, from: day) == cal.component(.year, from: Date()) {
            return day.formatted(.dateTime.locale(zh).month().day().weekday(.wide))
        }
        return day.formatted(.dateTime.locale(zh).year().month().day())
    }

    private func deleteSelected() async {
        let picked = visible.filter { selected.contains($0.id) }
        var done = 0
        for item in picked {
            working = "正在删除 \(done + 1)/\(picked.count)"
            if (try? await Cloud.shared.trash(item)) != nil {
                removed.insert(item.id)
                done += 1
            }
        }
        working = ""
        selected = []
        selecting = false
    }

    // 下载原图 / 原视频存进手机的「照片」
    private func saveSelected() async {
        let picked = visible.filter { selected.contains($0.id) }
        var saved = 0
        for (i, item) in picked.enumerated() {
            working = "正在存 \(i + 1)/\(picked.count)"
            do {
                try await MediaSaver.save(item)
                saved += 1
            } catch {
                working = "\(item.name)：\(error.localizedDescription)"
                try? await Task.sleep(for: .seconds(2))
            }
        }
        working = ""
        selected = []
        selecting = false
    }
}

// 缩略图：服务端给的签名地址（/thumb/…，6–12 小时有效），系统的网络缓存会存着，来回滑不用重下
struct Thumb: View {
    let item: CloudItem

    var body: some View {
        Color.secondary.opacity(0.15)
            .overlay {
                AsyncImage(url: Cloud.shared.absolute(item.thumb ?? item.cover)) { phase in
                    if let image = phase.image {
                        image.resizable().scaledToFill()
                    } else if item.isFolder {
                        Image(systemName: "folder.fill").font(.largeTitle).foregroundStyle(Color.brand)
                    } else if phase.error != nil || item.thumb == nil {
                        Image(systemName: item.isVideo ? "video" : "photo").foregroundStyle(.secondary)
                    }
                }
            }
            .clipped()
            .overlay(alignment: .bottomLeading) {
                if item.isVideo, let d = item.duration, d > 0 {
                    Text(Duration.seconds(d).formatted(.time(pattern: .minuteSecond)))
                        .font(.caption2.bold()).foregroundStyle(.white).shadow(radius: 2).padding(4)
                }
            }
    }
}

// 右上角的备份进度环：备份中转圈，完成变绿，有没传上去的变红；点开是备份中心
struct BackupRing: View {
    @ObservedObject private var auto = AutoBackup.shared
    @State private var showing = false

    var body: some View {
        Button { showing = true } label: {
            ZStack {
                Circle().stroke(Color.secondary.opacity(0.25), lineWidth: 2.5)
                if auto.running {
                    Circle().trim(from: 0, to: 0.7)
                        .stroke(Color.brand, style: StrokeStyle(lineWidth: 2.5, lineCap: .round))
                        .rotationEffect(.degrees(-90))
                } else {
                    Circle().stroke(tint, lineWidth: 2.5)
                }
                Image(systemName: icon).font(.system(size: 11, weight: .bold)).foregroundStyle(tint)
            }
            .frame(width: 26, height: 26)
        }
        .accessibilityLabel("备份状态")
        .fullScreenCover(isPresented: $showing) { BackupView() }
    }

    private var tint: Color {
        if !auto.failures.isEmpty { return .red }
        if auto.running { return .brand }
        return auto.enabled ? .green : .secondary
    }

    private var icon: String {
        if !auto.failures.isEmpty { return "exclamationmark" }
        if auto.running { return "arrow.up" }
        return auto.enabled ? "checkmark" : "pause"
    }
}

// 看大图、放视频：左右滑换下一张，下滑或点「完成」关闭；底部可以分享链接、存到手机、删除。
struct MediaViewer: View {
    let items: [CloudItem]
    let start: CloudItem
    let onDelete: (CloudItem) -> Void

    @Environment(\.dismiss) private var dismiss
    @State private var current: String = ""
    @State private var sharing: CloudItem?
    @State private var note = ""
    @State private var confirmDelete = false

    private var item: CloudItem? { items.first { $0.id == current } }

    var body: some View {
        NavigationStack {
            TabView(selection: $current) {
                ForEach(items) { item in
                    MediaPage(item: item, active: item.id == current).tag(item.id)
                }
            }
            .tabViewStyle(.page(indexDisplayMode: .never))
            .background(Color.black)
            .ignoresSafeArea(edges: .bottom)
            .gesture(DragGesture().onEnded { v in if v.translation.height > 120 { dismiss() } })
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .principal) {
                    if let item {
                        VStack(spacing: 0) {
                            Text(item.date.formatted(date: .abbreviated, time: .shortened)).font(.subheadline.bold())
                            Text(note.isEmpty ? "\(item.name) · \(sizeText(item.size))" : note).font(.caption2).foregroundStyle(.secondary)
                        }
                    }
                }
                ToolbarItem(placement: .topBarTrailing) { Button("完成") { dismiss() } }
                ToolbarItemGroup(placement: .bottomBar) {
                    Button { sharing = item } label: { Image(systemName: "link") }
                    Spacer()
                    Button { Task { await save() } } label: { Image(systemName: "square.and.arrow.down") }
                    Spacer()
                    Button(role: .destructive) { confirmDelete = true } label: { Image(systemName: "trash") }
                }
            }
            .toolbarBackground(.visible, for: .navigationBar, .bottomBar)
            .sheet(item: $sharing) { ShareLinkSheet(item: $0) }
            .confirmationDialog("移进回收站？", isPresented: $confirmDelete) {
                Button("移进回收站", role: .destructive) { Task { await remove() } }
            }
        }
        .onAppear { current = start.id }
    }

    private func save() async {
        guard let item else { return }
        note = "正在存到手机…"
        do {
            try await MediaSaver.save(item)
            note = "已存到手机的「照片」"
        } catch {
            note = error.localizedDescription
        }
    }

    private func remove() async {
        guard let item else { return }
        do {
            try await Cloud.shared.trash(item)
            onDelete(item)
            if items.count <= 1 { dismiss() } else if let next = items.first(where: { $0.id != item.id }) { current = next.id }
        } catch {
            note = error.localizedDescription
        }
    }
}

// 一张：照片先显示大图预览，视频用系统播放器（服务端支持 Range，可以拖动）
struct MediaPage: View {
    let item: CloudItem
    let active: Bool

    @State private var link: FileLink?
    @State private var player: AVPlayer?
    @State private var error = ""
    @State private var zoom: CGFloat = 1

    var body: some View {
        ZStack {
            Color.black
            if let player {
                VideoPlayer(player: player)
            } else if let link, let url = Cloud.shared.absolute(link.preview ?? link.url) {
                AsyncImage(url: url) { phase in
                    if let image = phase.image {
                        image.resizable().scaledToFit()
                            .scaleEffect(zoom)
                            .gesture(MagnifyGesture().onChanged { zoom = max(1, $0.magnification) }.onEnded { _ in withAnimation { zoom = 1 } })
                    } else if phase.error != nil {
                        Text("加载失败").foregroundStyle(.white)
                    } else {
                        Thumb(item: item).scaledToFit()
                    }
                }
            } else if !error.isEmpty {
                Text(error).foregroundStyle(.white).padding()
            } else {
                Thumb(item: item).scaledToFit().overlay { ProgressView() }
            }
        }
        // 只给当前这张（和左右滑到的）要地址：每要一次都算看了一张，格子口令按这个抽查
        .task(id: active) {
            guard active, link == nil else {
                if !active { player?.pause() }
                return
            }
            do {
                let l = try await Cloud.shared.link(for: item)
                link = l
                if item.isVideo, let url = Cloud.shared.absolute(l.url) {
                    let p = AVPlayer(url: url)
                    player = p
                    p.play()
                }
            } catch {
                if !error.isCancellation { self.error = error.localizedDescription }
            }
        }
    }
}

// 分享链接：有效期、提取码，生成后复制（链接和提取码一起）
struct ShareLinkSheet: View {
    let item: CloudItem

    @Environment(\.dismiss) private var dismiss
    @State private var days = 7
    @State private var usePassword = true
    @State private var password = String((0..<4).map { _ in "abcdefghjkmnpqrstuvwxyz23456789".randomElement()! })
    @State private var share: CloudShare?
    @State private var error = ""
    @State private var busy = false

    private var text: String {
        guard let share else { return "" }
        let url = Cloud.shared.shareURL(share)
        return usePassword ? "\(url) 提取码：\(password)" : url
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("分享", value: item.name)
                    Picker("有效期", selection: $days) {
                        Text("1 天").tag(1)
                        Text("7 天").tag(7)
                        Text("30 天").tag(30)
                        Text("永久").tag(0)
                    }
                    .pickerStyle(.segmented)
                    Toggle("需要提取码", isOn: $usePassword)
                    if usePassword { TextField("提取码", text: $password).textInputAutocapitalization(.never) }
                }
                .disabled(share != nil)
                if let share {
                    Section {
                        Text(text).font(.footnote.monospaced()).textSelection(.enabled)
                        Button("复制链接和提取码") { UIPasteboard.general.string = text }
                        ShareLink(item: text) { Label("发给…", systemImage: "square.and.arrow.up") }
                    } footer: {
                        Text(share.expiresAt.map { "\($0.formatted(date: .abbreviated, time: .omitted)) 失效。" } ?? "永久有效。")
                            + Text("可以在「我的 → 分享的链接」里随时取消。")
                    }
                } else {
                    Section {
                        Button(busy ? "正在生成…" : "生成链接") { Task { await create() } }.disabled(busy)
                    } footer: {
                        if !error.isEmpty { Text(error).foregroundStyle(.red) }
                    }
                }
            }
            .navigationTitle("分享链接")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { Button("完成") { dismiss() } }
        }
        .presentationDetents([.medium, .large])
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            share = try await Cloud.shared.createShare(item, days: days, password: usePassword ? password : "")
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// 下载原文件存进手机的「照片」
enum MediaSaver {
    @MainActor
    static func save(_ item: CloudItem) async throws {
        let link = try await Cloud.shared.link(for: item)
        guard let url = Cloud.shared.absolute(link.url) else { throw CloudError.server("下载地址不对") }
        let (temp, _) = try await URLSession.shared.download(from: url)
        let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + "-" + item.name)
        try FileManager.default.moveItem(at: temp, to: file)
        defer { try? FileManager.default.removeItem(at: file) }
        try await PHPhotoLibrary.shared().performChanges {
            let req = PHAssetCreationRequest.forAsset()
            req.addResource(with: item.isVideo ? .video : .photo, fileURL: file, options: nil)
        }
    }
}
