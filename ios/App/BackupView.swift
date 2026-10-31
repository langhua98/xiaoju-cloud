import Contacts
import EventKit
import Photos
import SwiftUI
import UniformTypeIdentifiers

// 「备份」页（备份中心）：上面一个进度环和「本地相册 / 云端备份 / 等待上传」三个数，下面是正在传的文件、没传上去的、
// 一个「自动备份」开关和几个设置。服务端地址、试传一张这类排查用的东西放在右上角的「诊断」里。
// 从「我的」点进来是推进去的一页，右滑或左上角返回；从别的页面右上角的小圆环打开时是全屏的，左上角「完成」关掉。
struct BackupView: View {
    var pushed = false // 推进来的（外面已经有 NavigationStack）：不再套一层，也不要「完成」
    @Environment(\.dismiss) private var dismiss
    @ObservedObject private var auto = AutoBackup.shared
    @State private var auth = PHPhotoLibrary.authorizationStatus(for: .readWrite)
    @State private var tally: AutoBackup.Tally?
    @State private var hiddenTally: AutoBackup.Tally? // 隐藏相册另算（不在「全部照片」里）
    @State private var albumCount = (UserDefaults.standard.stringArray(forKey: AutoBackup.selectedAlbums) ?? []).count
    @State private var switchError = ""
    @State private var pace: [(time: Date, done: Int)] = [] // 备份时每次重算记一笔，估算还要多久

    var body: some View {
        if pushed {
            page
        } else {
            NavigationStack { page }
        }
    }

    private var page: some View {
        List {
            Section { summary }
                .listRowBackground(Palette.card)
            if auth == .authorized {
                Section { SpeedView() }
                    .listRowInsets(EdgeInsets())
                    .listRowBackground(Color.clear)
            }
            if auth != .authorized {
                Section {
                    Text(authText).font(.footnote)
                    if auth == .notDetermined {
                        Button("允许访问所有照片") { Task { await requestAuth() } }
                    } else {
                        Button("去设置里打开") { openSettings() }
                    }
                }
                .listRowBackground(Palette.card)
            }
            if !auto.transfers.isEmpty {
                Section("正在上传 · \(auto.transfers.count) 个") {
                    ForEach(auto.transfers.prefix(5)) { t in TransferRow(t: t) }
                    if auto.transfers.count > 5 {
                        Text("还有 \(auto.transfers.count - 5) 个").font(.footnote).foregroundStyle(Palette.sub)
                    }
                }
                .listRowBackground(Palette.card)
            }
            if !auto.failures.isEmpty {
                Section {
                    NavigationLink {
                        FailuresView()
                    } label: {
                        Label("\(auto.failures.count) 个没传上去", systemImage: "exclamationmark.icloud")
                            .foregroundStyle(Palette.down)
                    }
                }
                .listRowBackground(Palette.card)
            }
            Section {
                Toggle("自动备份", isOn: Binding(get: { auto.enabled }, set: { setAutomatic($0) }))
                    .disabled(auth != .authorized && !auto.enabled)
                NavigationLink {
                    BackupAlbumsView()
                } label: {
                    LabeledContent("备份哪些照片", value: albumCount == 0 ? "全部照片" : "全部 + \(albumCount) 个相册")
                }
                NavigationLink("手机数据") { PhoneDataView() }
                NavigationLink {
                    SyncFoldersView()
                } label: {
                    LabeledContent("同步文件夹", value: auto.folders.isEmpty ? "没有" : "\(auto.folders.count) 个")
                }
                NavigationLink("插上电就备份") { ChargeShortcutView() }
                Button("从云盘恢复到手机…") { Restorer.shared.show = true }
            } footer: {
                Text(switchError.isEmpty
                    ? "充电、连网时自动补齐"
                    : switchError)
            }
            .listRowBackground(Palette.card)
        }
        .tint(Palette.accent)
        .scrollContentBackground(.hidden)
        .background(Palette.bg)
        .navigationTitle("备份")
        .toolbar {
            if !pushed {
                ToolbarItem(placement: .topBarLeading) { Button("完成") { dismiss() } }
            }
            ToolbarItem(placement: .topBarTrailing) {
                NavigationLink { DiagnosticsView() } label: { Image(systemName: "stethoscope") }
                    .accessibilityLabel("诊断")
            }
        }
        .refreshable {
            await Task {
                await AutoBackup.shared.reconcileAll()
                await refresh()
            }.value
        }
        .onAppear { albumCount = (UserDefaults.standard.stringArray(forKey: AutoBackup.selectedAlbums) ?? []).count }
        // 先用本地记下的马上算出三个数，再在后台和云盘比对（以前用快捷指令、网页传过的也算进「已备份」），
        // 比对时每认出一批就重算一次（以前要等核对完才显示，一万张要等一两分钟，这段时间一直是「—」）
        .task {
            await refresh()
            // 自动备份开着就再确认一次系统后台上传是开的（上次没开成的，这次打开页面时补上）
            if auth == .authorized && auto.enabled { switchError = await auto.setAutomatic(true) ?? "" }
            await Task.detached(priority: .userInitiated) { await AutoBackup.shared.reconcileIfStale() }.value
            await refresh()
        }
        .onChange(of: auto.matched) { _, _ in Task { await refresh() } }
        .onChange(of: auto.checking) { _, checking in
            if !checking { Task { await refresh() } }
        }
        // 备份的时候每 5 秒重算一次三个数，停了再算一次
        .task(id: auto.running) {
            await refresh()
            while auto.running && !Task.isCancelled {
                try? await Task.sleep(for: .seconds(5))
                await refresh()
            }
        }
    }

    // MARK: 进度环和三个数

    private var progress: Double {
        guard let tally, tally.total > 0 else { return 0 }
        return Double(tally.done) / Double(tally.total)
    }

    private var finished: Bool {
        guard let tally, tally.total > 0 else { return false }
        return tally.remaining == 0 && !auto.running
    }

    private var tint: Color {
        if !auto.failures.isEmpty && !auto.running { return Palette.down }
        return finished ? Palette.up : Palette.accent
    }

    // 标题只说状态，两三个字
    private var headline: String {
        if auth != .authorized { return "需要照片权限" }
        if auto.running { return "备份中" }
        if auto.isPaused { return "已暂停" }
        if !auto.failures.isEmpty { return "\(auto.failures.count) 个没传上" }
        if finished { return "已完成" }
        return "待备份"
    }

    // 下面一行只放一个短信息：还要多久、几点继续、上次完成是什么时候
    private var subline: String {
        if auth != .authorized { return "去设置里允许" }
        if auto.running { return eta }
        if auto.isPaused, let until = auto.pausedUntil {
            return "\(until.formatted(date: .omitted, time: .shortened)) 继续"
        }
        if finished, let last = auto.lastFinished { return last.formatted(.relative(presentation: .named)) }
        if !auto.failures.isEmpty { return "下一轮重传" }
        return auto.enabled ? "充电时自动" : "自动备份已关"
    }

    // 按最近两分钟传了多少张估算；刚开始还算不出来时只写「正在传」
    private var eta: String {
        guard let tally, let first = pace.first, let last = pace.last, last.done > first.done,
              last.time.timeIntervalSince(first.time) >= 20 else { return "正在传" }
        let perSecond = Double(last.done - first.done) / last.time.timeIntervalSince(first.time)
        let seconds = Double(tally.remaining) / perSecond
        if seconds < 60 { return "不到 1 分钟" }
        if seconds < 3600 { return "约 \(Int((seconds / 60).rounded())) 分钟" }
        return "约 \(Int((seconds / 3600).rounded())) 小时"
    }

    // 圆环在左、状态在右，三个数在下面一条底板里：整块是规整的长方形
    private var summary: some View {
        VStack(spacing: 14) {
            HStack(spacing: 16) {
                ring
                VStack(alignment: .leading, spacing: 4) {
                    Text(headline).font(.title3.bold())
                        .foregroundStyle(finished ? Palette.up : .primary)
                    Text(subline).font(.footnote).foregroundStyle(Palette.sub).monospacedDigit()
                }
                .lineLimit(1)
                Spacer(minLength: 0)
            }
            HStack(spacing: 0) {
                stat("本地相册", tally?.total)
                Divider().frame(height: 26)
                stat("云端备份", tally?.done)
                Divider().frame(height: 26)
                stat("等待上传", tally?.remaining)
            }
            .padding(.vertical, 10)
            .background(Palette.well, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            if let hiddenTally, hiddenTally.total > 0 {
                Text("隐藏相册 \(hiddenTally.done.formatted()) / \(hiddenTally.total.formatted())")
                    .font(.caption).foregroundStyle(Palette.sub)
            }
        }
        .padding(.vertical, 6)
    }

    // 圆环就是开始 / 停止按钮：中间是百分比和一个小字（停止 / 开始 / 继续）；长按选暂停多久
    private var ring: some View {
        Button(action: toggle) {
            ZStack {
                Circle().stroke(Palette.track, lineWidth: 8)
                Circle().trim(from: 0, to: progress)
                    .stroke(tint, style: StrokeStyle(lineWidth: 8, lineCap: .round))
                    .rotationEffect(.degrees(-90))
                    .animation(.easeInOut, value: progress)
                VStack(spacing: 1) {
                    if finished {
                        Image(systemName: "checkmark").font(.title.bold()).foregroundStyle(Palette.up)
                    } else {
                        Text(progress, format: .percent.precision(.fractionLength(0)))
                            .font(.title2.bold()).monospacedDigit()
                        Label(auto.running ? "停止" : auto.isPaused ? "继续" : "开始",
                              systemImage: auto.running ? "stop.fill" : "play.fill")
                            .labelStyle(RingAction())
                    }
                }
            }
            .frame(width: 104, height: 104)
            .contentShape(Circle())
        }
        .buttonStyle(RingPress())
        .disabled(auth != .authorized)
        .contextMenu {
            if auto.isPaused {
                Button("继续自动备份", systemImage: "play") { auto.resume() }
            } else {
                Button("暂停 1 小时", systemImage: "pause") { auto.pause(hours: 1) }
                Button("暂停到明天", systemImage: "moon") { auto.pause(hours: 24) }
            }
        }
        .accessibilityLabel(auto.running ? "停止备份" : "开始备份")
        .accessibilityHint("长按可以选暂停多久")
        .sensoryFeedback(.impact, trigger: auto.running)
    }

    private func toggle() {
        if auto.running {
            auto.stop()
        } else {
            auto.resume()
            auto.start(reason: "手动")
        }
    }

    private func stat(_ label: String, _ value: Int?) -> some View {
        VStack(spacing: 2) {
            Text(value.map { $0.formatted() } ?? "—").font(.headline).monospacedDigit()
                .contentTransition(.numericText())
                .animation(.default, value: value)
            Text(label).font(.caption2).foregroundStyle(Palette.sub)
        }
        .frame(maxWidth: .infinity)
    }

    // MARK: 动作

    private func refresh() async {
        auth = PHPhotoLibrary.authorizationStatus(for: .readWrite)
        guard auth == .authorized else {
            tally = nil
            return
        }
        // 不等核对完：手机上的张数马上就有；已备份先按本地记下的算（传过的、上次核对认出来的），核对时认出一批再加上
        let counted = await Task.detached(priority: .userInitiated) { () -> (AutoBackup.Tally, AutoBackup.Tally?) in
            let all = AutoBackup.shared.tally(target: "all", assets: AutoBackup.allPhotos())
            guard AutoBackup.flag(AutoBackup.backupHidden), let hidden = PhoneAlbum.hiddenAlbum() else { return (all, nil) }
            return (all, AutoBackup.shared.tally(target: "hidden", assets: hidden.assets()))
        }.value
        (tally, hiddenTally) = counted
        if auto.running {
            pace = (pace + [(time: Date(), done: counted.0.done)]).filter { Date().timeIntervalSince($0.time) < 120 }
        } else {
            pace = []
        }
    }

    private func setAutomatic(_ on: Bool) {
        Task { switchError = await auto.setAutomatic(on) ?? "" }
    }

    private func requestAuth() async {
        auth = await PHPhotoLibrary.requestAuthorization(for: .readWrite)
        // 第一次允许后把系统后台上传也打开（开关默认是开的）
        if auth == .authorized && auto.enabled { setAutomatic(true) }
        // 页面打开时还没有权限，那次核对没做：现在补上，不然三个数一直是「—」
        if auth == .authorized {
            await Task.detached(priority: .userInitiated) { await AutoBackup.shared.reconcileAll() }.value
        }
        await refresh()
    }

    private func openSettings() {
        if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
    }

    private var authText: String {
        switch auth {
        case .limited: return "现在只允许了部分照片。自动备份要「所有照片」，请到 设置 → 小桔备份 → 照片 里改。"
        case .denied, .restricted: return "没有照片权限，请到 设置 → 小桔备份 → 照片 里选「所有照片」。"
        default: return "备份照片需要访问「所有照片」。"
        }
    }
}

// 圆环里「■ 停止」「▶ 开始」这样的一行小灰字
private struct RingAction: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: 3) {
            configuration.icon.font(.system(size: 7, weight: .bold))
            configuration.title.font(.caption2)
        }
        .foregroundStyle(Palette.sub)
    }
}

// 圆环按下时缩一下，松手弹回（不用列表那种整行变灰、要等一下的按下效果）
private struct RingPress: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .scaleEffect(configuration.isPressed ? 0.94 : 1)
            .animation(.spring(duration: 0.2), value: configuration.isPressed)
    }
}

struct TransferRow: View {
    let t: AutoBackup.Transfer

    // 「12/490 MB」；小于 1MB 的显示百分比
    static func progressText(_ t: AutoBackup.Transfer) -> String {
        // 手机这边发完了，服务端还要把最后一段存进 Telegram，大文件要等一两分钟才算传完
        if t.total > 0 && t.sent >= t.total { return "保存中" }
        if t.total < 1 << 20 { return "\(t.sent * 100 / max(t.total, 1))%" }
        return "\(t.sent >> 20)/\(t.total >> 20) MB"
    }

    private var saving: Bool { t.total > 0 && t.sent >= t.total }

    // 照片、视频各一个图标（看扩展名）
    private var icon: String {
        let ext = (t.name as NSString).pathExtension.lowercased()
        return ["mov", "mp4", "m4v", "avi", "hevc"].contains(ext) ? "video.fill" : "photo.fill"
    }

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: icon).font(.footnote).foregroundStyle(Palette.accent)
                .frame(width: 32, height: 32)
                .background(Palette.accent.opacity(0.15), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text(t.name).font(.footnote).lineLimit(1).truncationMode(.middle)
                    Spacer(minLength: 8)
                    Text(Self.progressText(t)).font(.caption).monospacedDigit().lineLimit(1)
                        .foregroundStyle(Palette.sub)
                }
                ProgressView(value: Double(min(t.sent, max(t.total, 1))), total: Double(max(t.total, 1)))
                    .tint(saving ? Palette.sub : Palette.accent)
            }
        }
        .padding(.vertical, 2)
    }
}

// 没传上去的：是哪几个、为什么；「全部重试」就是马上再跑一轮（传过的会跳过，很快）。
struct FailuresView: View {
    @ObservedObject private var auto = AutoBackup.shared
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        List {
            Section {
                ForEach(auto.failures) { f in
                    VStack(alignment: .leading, spacing: 2) {
                        Text(f.name).lineLimit(1).truncationMode(.middle)
                        Text(f.reason).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                    }
                }
            } footer: {
                Text("照片原图在 iCloud 上时要先下载，网络不好会失败；视频断了下次会接着传。")
            }
        }
        .navigationTitle("没传上去的")
        .toolbar {
            Button("全部重试") {
                auto.resume()
                auto.start(reason: "重试")
                dismiss()
            }
            .disabled(auto.running)
        }
    }
}

// 通讯录、日历、提醒事项：打开时才问权限，每天一份放进云盘「手机数据」。
struct PhoneDataView: View {
    @AppStorage(AutoBackup.backupContacts) private var contacts = true
    @AppStorage(AutoBackup.backupCalendar) private var calendar = true
    @AppStorage(AutoBackup.backupReminders) private var reminders = true

    var body: some View {
        List {
            Section {
                Toggle("通讯录", isOn: $contacts)
                Toggle("日历", isOn: $calendar)
                Toggle("提醒事项", isOn: $reminders)
            } footer: {
                Text("每天一份，放进云盘「手机数据」（默认私密）。短信、健康这类苹果不开放给其他 App 的数据备份不了。")
            }
        }
        .navigationTitle("手机数据")
        .onChange(of: contacts) { _, on in
            if on { Task { _ = try? await CNContactStore().requestAccess(for: .contacts) } }
        }
        .onChange(of: calendar) { _, on in
            if on { Task { _ = try? await EKEventStore().requestFullAccessToEvents() } }
        }
        .onChange(of: reminders) { _, on in
            if on { Task { _ = try? await EKEventStore().requestFullAccessToReminders() } }
        }
    }
}

// 「文件」App 里的文件夹：每次备份时把新文件、改过的文件传进云盘「手机数据/文件/文件夹名」。
struct SyncFoldersView: View {
    @ObservedObject private var auto = AutoBackup.shared
    @State private var picking = false
    @State private var error = ""

    var body: some View {
        List {
            Section {
                ForEach(auto.folders) { f in Label(f.name, systemImage: "folder") }
                    .onDelete { rows in rows.map { auto.folders[$0].id }.forEach(auto.removeFolder) }
                Button("添加文件夹") { picking = true }
            } footer: {
                Text(error.isEmpty
                    ? "「文件」App 里的文件夹（我的 iPhone、iCloud 云盘、微信 / QQ 存的文件……），每次备份时把新文件和改过的文件传进云盘「手机数据/文件/文件夹名」，子文件夹照搬。左滑移除（云盘里已经传的不删）。"
                    : error)
            }
        }
        .navigationTitle("同步文件夹")
        .fileImporter(isPresented: $picking, allowedContentTypes: [.folder]) { result in
            do { try auto.addFolder(result.get()) } catch { self.error = "添加文件夹失败：\(error.localizedDescription)" }
        }
    }
}

struct ChargeShortcutView: View {
    var body: some View {
        List {
            Section {
                Text("系统自己挑时间跑自动备份，一般在夜里。想每次一插电就开始，用快捷指令加一个自动化：")
                Label("打开「快捷指令」→ 自动化 → 新建", systemImage: "1.circle")
                Label("选「充电器」→「已连接」→「立即运行」", systemImage: "2.circle")
                Label("添加操作「备份到小桔云盘」", systemImage: "3.circle")
            } footer: {
                Text("运行时会打开 App 备份，跑完照常自动锁屏。")
            }
        }
        .navigationTitle("插上电就备份")
    }
}

// 诊断：平时用不到，备份不动的时候来这里看。
struct DiagnosticsView: View {
    @ObservedObject private var auto = AutoBackup.shared
    @State private var message = ""
    @State private var busy = false

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    var body: some View {
        List {
            Section("服务端") {
                Text(server).font(.footnote).textSelection(.enabled)
            }
            Section("状态") {
                LabeledContent("照片权限", value: authText)
                LabeledContent("系统后台上传", value: PHPhotoLibrary.shared().uploadJobExtensionEnabled ? "开" : "关")
                LabeledContent("充电时备份", value: auto.enabled ? "开" : "关")
                if !auto.status.isEmpty {
                    Text(auto.status).font(.footnote).foregroundStyle(.secondary).textSelection(.enabled)
                }
            }
            Section {
                Button(busy ? "正在上传…" : "现在上传最新的一张") { Task { await uploadLatest() } }
                    .disabled(PHPhotoLibrary.authorizationStatus(for: .readWrite) != .authorized || busy)
                if !message.isEmpty {
                    Text(message).font(.footnote).textSelection(.enabled)
                }
            } footer: {
                Text("不经过后台，直接从 App 里传一张，确认手机到服务端是通的。")
            }
        }
        .navigationTitle("诊断")
    }

    private var authText: String {
        switch PHPhotoLibrary.authorizationStatus(for: .readWrite) {
        case .authorized: return "所有照片"
        case .limited: return "部分照片"
        case .denied, .restricted: return "没有"
        default: return "还没问"
        }
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
