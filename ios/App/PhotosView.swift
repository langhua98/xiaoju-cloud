import AVKit
import Combine
import Photos
import SwiftUI
import UIKit

// 「照片」页：云盘「相册」里的照片、视频按拍摄日期排（新的在上），学 iCloud 照片、Google 相册。
// 右上角的进度环是备份状态，点开是备份中心。双指捏合换每行张数，点「选择」多选后可以下载到手机、删除。
// 选得快：在照片上横着一划直接进多选，划过的都选上，手指再上下移动接着选一整片，贴着屏幕上下边自动滚动；
// 从已选的开始划是一起取消。日期右边「选择」选一整天。删除后底部可以撤销。
struct PhotosView: View {
    @ObservedObject private var cloud = Cloud.shared
    @State private var items: [CloudItem] = []
    @State private var loading = false
    @State private var error = ""
    @State private var album: String?
    @State private var next: Int? // 下一批从第几个开始；nil = 取完了

    var body: some View {
        NavigationStack {
            MediaGallery(items: items, title: "照片", emptyText: loading ? "正在加载…" : "云盘「相册」里还没有照片。打开「自动备份」后，手机上的照片会自己传上来。",
                         reload: load, more: next == nil ? nil : loadMore)
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
                next = nil
                return
            }
            // 先取最新的一批显示，滑到底部再取下一批（见 loadMore）
            let page = try await cloud.childrenPage(of: album.id, offset: 0)
            self.album = album.id
            items = page.items.filter(\.isMedia)
            next = page.next
            error = ""
        } catch {
            if !error.isCancellation { self.error = error.localizedDescription }
        }
    }

    private func loadMore() async {
        guard let album, let at = next else { return }
        do {
            let page = try await cloud.childrenPage(of: album, offset: at)
            guard next == at else { return } // 这期间刷新过了
            items = Cloud.merge(items, page.items.filter(\.isMedia))
            next = page.next
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
    var more: (() async -> Void)? = nil // 还有下一批时：滑到底部取下一批

    @State private var columns = UserDefaults.standard.object(forKey: "gallery.columns") as? Int ?? 4
    @State private var selecting = false
    @State private var selected = Set<String>()
    @State private var viewing: CloudItem?
    @State private var working = ""
    @State private var confirmDelete = false
    @State private var removed = Set<String>() // 刚删掉的，等下次加载前先藏起来
    @State private var sharing: CloudItem?
    @State private var undo: [CloudItem] = [] // 刚移进回收站的，底部「撤销」能放回去
    @State private var frames = CellFrames()
    @State private var swipe: SwipeState?

    private static let steps = [3, 4, 5, 7]

    // 划选：从 anchor 划到手指下那张，中间的都设成 mode；划回来时没划到的恢复成开始时的样子（和 iPhone 照片一样）
    struct SwipeState {
        var order: [String]
        var anchor: Int
        var base: Set<String>
        var mode: Bool
        var last: Int
    }

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
                                    .onGeometryChange(for: CGRect.self) { $0.frame(in: .named(Self.space)) } action: { frames.rects[item.id] = $0 }
                                    .onDisappear { frames.rects[item.id] = nil } // 只记屏幕上的，滚走的位置可能已经变了
                            }
                        }
                    } header: {
                        HStack {
                            Text(dayTitle(group.day)).font(.subheadline.bold())
                            Spacer()
                            if selecting {
                                let all = group.items.allSatisfy { selected.contains($0.id) }
                                Button(all ? "取消选择" : "选择") {
                                    let ids = group.items.map(\.id)
                                    if all { selected.subtract(ids) } else { selected.formUnion(ids) }
                                }
                                .font(.subheadline)
                            }
                        }
                        .padding(.horizontal, 12).padding(.vertical, 6)
                        .background(.bar)
                    }
                }
            }
            .coordinateSpace(.named(Self.space))
            .gesture(SwipeSelect(space: Self.space, onChange: swipeMoved, onEnd: { swipe = nil }))
            // 滑到底部：取下一批（加进来以后还在屏幕上就接着取）
            if let more {
                ProgressView().frame(maxWidth: .infinity).padding(20)
                    .task(id: items.count) { await more() }
            }
            Color.clear.frame(height: selecting ? 80 : 0)
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
            if selecting {
                selectionBar
            } else if !working.isEmpty {
                Text(working).font(.footnote).padding(.horizontal, 14).padding(.vertical, 8)
                    .background(.thinMaterial, in: Capsule()).padding(.bottom, 8)
            } else if !undo.isEmpty {
                HStack(spacing: 14) {
                    Text("已移进回收站 \(undo.count) 项").font(.footnote)
                    Button("撤销") { Task { await restoreUndo() } }.font(.footnote.bold())
                }
                .padding(.horizontal, 16).padding(.vertical, 8)
                .background(.thinMaterial, in: Capsule()).padding(.bottom, 8)
                .task(id: undo.map(\.id)) {
                    try? await Task.sleep(for: .seconds(8))
                    if !Task.isCancelled { undo = [] }
                }
            }
        }
        .fullScreenCover(item: $viewing) { item in
            MediaViewer(items: visible, start: item) { gone in removed.insert(gone.id) }
        }
        .sheet(item: $sharing) { ShareLinkSheet(item: $0) }
        .confirmationDialog("把 \(selected.count) 项移进回收站？", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("移进回收站", role: .destructive) { Task { await deleteSelected() } }
        } message: {
            Text("删除后马上可以撤销；30 天内也可以在「我的 → 回收站」里恢复。")
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
            Button("存到手机", systemImage: "square.and.arrow.down") { Task { await saveOne(item) } }
        }
    }

    private var selectionBar: some View {
        VStack(spacing: 4) {
            if !working.isEmpty { Text(working).font(.caption).foregroundStyle(.secondary) }
            HStack {
                Button { Task { await saveSelected() } } label: { Label("存到手机", systemImage: "square.and.arrow.down") }
                Spacer()
                Button(role: .destructive) { confirmDelete = true } label: {
                    Label(selected.count > 1 ? "删除 \(selected.count) 项" : "删除", systemImage: "trash")
                }
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

    // 几张一起删（每次最多 4 个请求），几百张也很快
    private func deleteSelected() async {
        let picked = visible.filter { selected.contains($0.id) }
        working = "正在删除 0/\(picked.count)"
        var done: [CloudItem] = []
        await Cloud.eachLimit(picked) { item in
            (try? await Cloud.shared.trash(item)) != nil
        } progress: { item, ok in
            if ok {
                removed.insert(item.id)
                done.append(item)
            }
            working = "正在删除 \(done.count)/\(picked.count)"
        }
        working = done.count == picked.count ? "" : "有 \(picked.count - done.count) 项没删掉，稍后再试"
        selected = []
        selecting = false
        undo = done
        if !working.isEmpty {
            try? await Task.sleep(for: .seconds(3))
            working = ""
        }
    }

    // 撤销：刚删的放回原处
    private func restoreUndo() async {
        let list = undo
        undo = []
        working = "正在放回…"
        await Cloud.eachLimit(list) { item in
            (try? await Cloud.shared.send("POST", "/api/files/\(item.id)/restore")) != nil
        } progress: { item, ok in
            if ok { removed.remove(item.id) }
        }
        working = ""
        await reload()
    }

    // MARK: 划选

    private static let space = "gallery"

    private func swipeMoved(_ point: CGPoint, _ start: CGPoint) {
        if swipe == nil {
            // 刚开始划：从手指按下的那张开始
            guard let first = frames.id(at: start) else { return }
            let order = days.flatMap { $0.items.map(\.id) }
            guard let anchor = order.firstIndex(of: first) else { return }
            if !selecting { selecting = true; selected = [] }
            swipe = SwipeState(order: order, anchor: anchor, base: selected, mode: !selected.contains(first), last: -1)
            apply(anchor)
        }
        guard let id = frames.id(at: point), let i = swipe?.order.firstIndex(of: id), i != swipe?.last else { return }
        apply(i)
    }

    private func apply(_ i: Int) {
        guard var s = swipe else { return }
        s.last = i
        swipe = s
        let range = min(s.anchor, i)...max(s.anchor, i)
        var next = s.base
        for k in range { if s.mode { next.insert(s.order[k]) } else { next.remove(s.order[k]) } }
        if next != selected {
            selected = next
            UISelectionFeedbackGenerator().selectionChanged()
        }
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

    // 长按一张「存到手机」
    private func saveOne(_ item: CloudItem) async {
        working = "正在存到手机…"
        do {
            try await MediaSaver.save(item) { working = "正在存到手机 \(Int($0 * 100))%" }
            working = "已存到手机的「照片」"
        } catch {
            working = "\(item.name)：\(error.localizedDescription)"
        }
        try? await Task.sleep(for: .seconds(2))
        working = ""
    }
}

// 每个格子在网格里的位置（划选时看手指下面是哪张）。只是查表用，改了不用刷新界面，所以放在类里
final class CellFrames {
    var rects: [String: CGRect] = [:]

    func id(at point: CGPoint) -> String? {
        rects.first { $0.value.contains(point) }?.key
    }
}

// 网格上横着一划：开始划选，竖着划照样滚动（滚动要等它不成立才开始，只差一点点距离）。
// 从屏幕左边缘划是返回上一页，不算。划选中手指贴近上下边时自动滚动，越靠边越快。
// onChange(手指位置, 开始划的位置)，都是 space 这个坐标系里的
struct SwipeSelect: UIGestureRecognizerRepresentable {
    let space: String
    let onChange: (CGPoint, CGPoint) -> Void
    let onEnd: () -> Void

    func makeCoordinator(converter: CoordinateSpaceConverter) -> Coordinator { Coordinator() }

    func makeUIGestureRecognizer(context: Context) -> UIPanGestureRecognizer {
        let pan = UIPanGestureRecognizer()
        pan.maximumNumberOfTouches = 1
        pan.delegate = context.coordinator
        return pan
    }

    func handleUIGestureRecognizerAction(_ pan: UIPanGestureRecognizer, context: Context) {
        let c = context.coordinator
        switch pan.state {
        case .began, .changed:
            let here = context.converter.location(in: .named(space))
            if pan.state == .began {
                let t = pan.translation(in: pan.view)
                c.start = CGPoint(x: here.x - t.x, y: here.y - t.y)
                c.pan = pan
                c.scroll = Coordinator.scrollView(around: pan)
            }
            if let sv = c.scroll {
                let p = pan.location(in: sv) // 滚动内容里的位置；和 space 都跟着内容走，差是固定的
                c.delta = CGPoint(x: here.x - p.x, y: here.y - p.y)
            }
            let start = c.start
            c.report = { onChange($0, start) }
            onChange(here, start)
            c.follow()
        default:
            c.stop()
            onEnd()
        }
    }

    final class Coordinator: NSObject, UIGestureRecognizerDelegate {
        var start = CGPoint.zero
        var delta = CGPoint.zero
        weak var scroll: UIScrollView?
        weak var pan: UIPanGestureRecognizer?
        var report: ((CGPoint) -> Void)?
        private var link: CADisplayLink?

        func gestureRecognizerShouldBegin(_ g: UIGestureRecognizer) -> Bool {
            guard let pan = g as? UIPanGestureRecognizer else { return true }
            let v = pan.velocity(in: pan.view)
            let at = pan.location(in: nil).x - pan.translation(in: nil).x
            return abs(v.x) > abs(v.y) * 1.2 && at > 30
        }

        // 滚动（UIScrollView 的拖动）要等划选不成立才开始
        func gestureRecognizer(_ g: UIGestureRecognizer, shouldBeRequiredToFailBy other: UIGestureRecognizer) -> Bool {
            other.view is UIScrollView && other is UIPanGestureRecognizer
        }

        // 手势所在的滚动视图：先往上找，找不到就在下面找包含手指的那个
        static func scrollView(around pan: UIPanGestureRecognizer) -> UIScrollView? {
            var v = pan.view
            while let cur = v {
                if let sv = cur as? UIScrollView { return sv }
                v = cur.superview
            }
            var queue = pan.view.map { [$0] } ?? []
            var found: UIScrollView?
            while !queue.isEmpty {
                let cur = queue.removeFirst()
                if let sv = cur as? UIScrollView, sv.bounds.contains(pan.location(in: sv)) { found = sv }
                queue += cur.subviews
            }
            return found
        }

        func follow() {
            guard link == nil, scroll != nil else { return }
            let l = CADisplayLink(target: self, selector: #selector(tick))
            l.add(to: .main, forMode: .common)
            link = l
        }

        func stop() {
            link?.invalidate()
            link = nil
            report = nil
        }

        @objc private func tick() {
            guard let sv = scroll, let pan, pan.state == .began || pan.state == .changed else { stop(); return }
            let y = pan.location(in: sv).y - sv.contentOffset.y // 手指在屏幕上（滚动视图可见部分里）的高度
            let top = sv.adjustedContentInset.top + 60
            let bottom = sv.bounds.height - sv.adjustedContentInset.bottom - 60
            var speed: CGFloat = 0
            if y < top { speed = -min(18, (top - y) / 4 + 3) } else if y > bottom { speed = min(18, (y - bottom) / 4 + 3) }
            guard speed != 0 else { return }
            let minY = -sv.adjustedContentInset.top
            let maxY = max(minY, sv.contentSize.height + sv.adjustedContentInset.bottom - sv.bounds.height)
            let to = min(maxY, max(minY, sv.contentOffset.y + speed))
            guard to != sv.contentOffset.y else { return }
            sv.contentOffset.y = to
            let p = pan.location(in: sv)
            report?(CGPoint(x: p.x + delta.x, y: p.y + delta.y))
        }
    }
}

// 缩略图格子 Thumb 在 RateGate.swift（限流时排队、等一会儿再下）

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
            try await MediaSaver.save(item) { note = "正在存到手机 \(Int($0 * 100))%" }
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
                if item.isVideo, let direct = Cloud.shared.absolute(l.url) {
                    // 有中转先走中转；播不了（中转出问题）再换回直接从家里电脑播
                    let viaRelay = Cloud.shared.absolute(l.stream)
                    let p = AVPlayer(url: viaRelay ?? direct)
                    player = p
                    p.play()
                    if viaRelay != nil, let first = p.currentItem {
                        for await status in first.publisher(for: \.status).values where status == .failed {
                            let at = p.currentTime()
                            p.replaceCurrentItem(with: AVPlayerItem(url: direct))
                            await p.seek(to: at)
                            p.play()
                            break
                        }
                    }
                }
            } catch {
                if !error.isCancellation { self.error = error.localizedDescription }
            }
        }
    }
}

// 分享链接：文件默认生成直链（别人点开就直接下载，不用打开分享页、不要提取码），
// 也可以改成分享页链接（可设提取码，文件夹只能用这种）。有效期可选，生成后复制或发出去。
struct ShareLinkSheet: View {
    let item: CloudItem

    enum Kind: Hashable { case direct, page }

    @Environment(\.dismiss) private var dismiss
    @State private var kind: Kind
    @State private var days = 7
    @State private var usePassword = true
    @State private var password = String((0..<4).map { _ in "abcdefghjkmnpqrstuvwxyz23456789".randomElement()! })
    @State private var share: CloudShare?
    @State private var error = ""
    @State private var busy = false
    @State private var copied = false

    init(item: CloudItem) {
        self.item = item
        _kind = State(initialValue: item.isFolder ? .page : .direct)
    }

    private var text: String {
        guard let share else { return "" }
        if kind == .direct { return Cloud.shared.directURL(share, name: item.name) }
        let url = Cloud.shared.shareURL(share)
        return usePassword ? "\(url) 提取码：\(password)" : url
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    LabeledContent("分享", value: item.name)
                    if !item.isFolder {
                        Picker("链接", selection: $kind) {
                            Text("直链").tag(Kind.direct)
                            Text("分享页").tag(Kind.page)
                        }
                        .pickerStyle(.segmented)
                    }
                    Picker("有效期", selection: $days) {
                        Text("1 天").tag(1)
                        Text("7 天").tag(7)
                        Text("30 天").tag(30)
                        Text("永久").tag(0)
                    }
                    .pickerStyle(.segmented)
                    if kind == .page {
                        Toggle("需要提取码", isOn: $usePassword)
                        if usePassword { TextField("提取码", text: $password).textInputAutocapitalization(.never) }
                    }
                } footer: {
                    if share == nil {
                        Text(kind == .direct ? "直链：别人点开就直接下载，浏览器、下载工具都能用，不需要提取码。"
                                             : "分享页：别人打开网页看、下载，可以设提取码。")
                    }
                }
                .disabled(share != nil)
                if let share {
                    Section {
                        Text(text).font(.footnote.monospaced()).textSelection(.enabled)
                        Button(copied ? "已复制" : (kind == .direct || !usePassword ? "复制链接" : "复制链接和提取码"),
                               systemImage: copied ? "checkmark" : "doc.on.doc") {
                            UIPasteboard.general.string = text
                            copied = true
                        }
                        ShareLink(item: text) { Label("发给…", systemImage: "square.and.arrow.up") }
                    } footer: {
                        Text(share.expiresAt.map { "\($0.formatted(date: .abbreviated, time: .omitted)) 失效。" } ?? "永久有效。")
                            + Text("可以在「我的 → 分享的链接」里随时取消。")
                    }
                } else {
                    Section {
                        Button(busy ? "正在生成…" : (kind == .direct ? "生成直链" : "生成链接")) { Task { await create() } }.disabled(busy)
                    } footer: {
                        if !error.isEmpty { Text(error).foregroundStyle(.red) }
                    }
                }
            }
            .navigationTitle(kind == .direct ? "分享直链" : "分享链接")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { Button("完成") { dismiss() } }
        }
        .presentationDetents([.medium, .large])
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            let pw = kind == .page && usePassword ? password : ""
            share = try await Cloud.shared.createShare(item, days: days, password: pw)
            if kind == .direct { // 生成好就复制，少点一下
                UIPasteboard.general.string = text
                copied = true
            }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// 下载原文件存进手机的「照片」
enum MediaSaver {
    @MainActor
    static func save(_ item: CloudItem, progress: @escaping @MainActor (Double) -> Void = { _ in }) async throws {
        let file = try await Cloud.shared.download(item, progress: progress)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        try await PHPhotoLibrary.shared().performChanges {
            let req = PHAssetCreationRequest.forAsset()
            req.addResource(with: item.isVideo ? .video : .photo, fileURL: file, options: nil)
        }
    }
}
