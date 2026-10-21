import Contacts
import EventKit
import Photos
import SwiftUI

// 「备份」页（备份中心）：上面一个进度环和「手机上 / 已备份 / 还剩」三个数，下面是正在传的文件、没传上去的、
// 一个「自动备份」开关和几个设置。服务端地址、试传一张这类排查用的东西放在右上角的「诊断」里。
struct BackupView: View {
    @ObservedObject private var auto = AutoBackup.shared
    @State private var auth = PHPhotoLibrary.authorizationStatus(for: .readWrite)
    @State private var tally: AutoBackup.Tally?
    @State private var albumCount = (UserDefaults.standard.stringArray(forKey: AutoBackup.selectedAlbums) ?? []).count
    @State private var switchError = ""

    var body: some View {
        NavigationStack {
            List {
                Section { summary }
                if auth != .authorized {
                    Section {
                        Text(authText).font(.footnote)
                        if auth == .notDetermined {
                            Button("允许访问所有照片") { Task { await requestAuth() } }
                        } else {
                            Button("去设置里打开") { openSettings() }
                        }
                    }
                }
                if !auto.transfers.isEmpty {
                    Section("正在上传") {
                        ForEach(auto.transfers.prefix(5)) { t in TransferRow(t: t) }
                        if auto.transfers.count > 5 {
                            Text("还有 \(auto.transfers.count - 5) 个").font(.footnote).foregroundStyle(.secondary)
                        }
                    }
                }
                if !auto.failures.isEmpty {
                    Section {
                        NavigationLink {
                            FailuresView()
                        } label: {
                            Label("\(auto.failures.count) 个没传上去", systemImage: "exclamationmark.icloud")
                                .foregroundStyle(.red)
                        }
                    }
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
                    NavigationLink("插上电就备份") { ChargeShortcutView() }
                } footer: {
                    Text(switchError.isEmpty
                        ? "打开后，新拍的照片由系统在后台传；插着电、连着网的时候（一般是夜里）再把没传的补齐。"
                        : switchError)
                }
                Section {
                    if auto.running {
                        Button("停止这一轮", role: .destructive) { auto.stop() }
                    } else {
                        Button("现在备份") {
                            auto.resume()
                            auto.start(reason: "手动")
                        }
                        .disabled(auth != .authorized)
                    }
                    if auto.isPaused {
                        Button("继续自动备份") { auto.resume() }
                    } else {
                        Menu("暂停") {
                            Button("暂停 1 小时") { auto.pause(hours: 1) }
                            Button("暂停到明天") { auto.pause(hours: 24) }
                        }
                    }
                }
            }
            .navigationTitle("备份")
            .toolbar {
                NavigationLink { DiagnosticsView() } label: { Image(systemName: "stethoscope") }
                    .accessibilityLabel("诊断")
            }
            .refreshable {
                await AutoBackup.shared.reconcileAll()
                await refresh()
            }
            .onAppear { albumCount = (UserDefaults.standard.stringArray(forKey: AutoBackup.selectedAlbums) ?? []).count }
            // 先和云盘比对一遍（以前用快捷指令、网页传过的也算进「已备份」），再算三个数
            .task {
                await Task.detached(priority: .userInitiated) { await AutoBackup.shared.reconcileAll() }.value
                await refresh()
            }
            // 备份的时候每 10 秒重算一次三个数，停了再算一次
            .task(id: auto.running) {
                await refresh()
                while auto.running && !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(10))
                    await refresh()
                }
            }
        }
    }

    // MARK: 进度环和三个数

    private var progress: Double {
        guard let tally, tally.total > 0 else { return 0 }
        return Double(tally.done) / Double(tally.total)
    }

    private var tint: Color {
        if !auto.failures.isEmpty { return .red }
        if let tally, tally.total > 0, tally.remaining == 0 { return .green }
        return .orange
    }

    private var headline: String {
        if auth != .authorized { return "需要照片权限" }
        if auto.running { return "正在备份" }
        if auto.isPaused { return "已暂停" }
        if !auto.failures.isEmpty { return "有 \(auto.failures.count) 个没传上去" }
        if let tally, tally.total > 0, tally.remaining == 0 { return "已全部备份" }
        return auto.enabled ? "等插上电时自动备份" : "自动备份已关闭"
    }

    private var subline: String {
        if auto.isPaused, let until = auto.pausedUntil {
            return "\(until.formatted(date: .omitted, time: .shortened)) 自动恢复"
        }
        if let last = auto.lastFinished {
            return "上次备份完成：\(last.formatted(.relative(presentation: .named)))"
        }
        return "还没完整备份过"
    }

    private var summary: some View {
        VStack(spacing: 14) {
            ZStack {
                Circle().stroke(Color.secondary.opacity(0.2), lineWidth: 10)
                Circle().trim(from: 0, to: progress)
                    .stroke(tint, style: StrokeStyle(lineWidth: 10, lineCap: .round))
                    .rotationEffect(.degrees(-90))
                    .animation(.easeInOut, value: progress)
                if tint == .green {
                    Image(systemName: "checkmark").font(.system(size: 34, weight: .bold)).foregroundStyle(.green)
                } else {
                    Text(progress, format: .percent.precision(.fractionLength(0))).font(.title2.bold()).monospacedDigit()
                }
            }
            .frame(width: 112, height: 112)
            VStack(spacing: 2) {
                Text(headline).font(.headline)
                Text(subline).font(.footnote).foregroundStyle(.secondary)
            }
            HStack {
                stat("手机上", tally?.total)
                stat("已备份", tally?.done)
                stat("还剩", tally?.remaining)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 8)
    }

    private func stat(_ label: String, _ value: Int?) -> some View {
        VStack(spacing: 2) {
            Text(value.map { $0.formatted() } ?? "—").font(.headline).monospacedDigit()
            Text(label).font(.caption).foregroundStyle(.secondary)
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
        tally = await Task.detached(priority: .userInitiated) {
            AutoBackup.shared.tally(target: "all", assets: AutoBackup.allPhotos())
        }.value
    }

    private func setAutomatic(_ on: Bool) {
        switchError = auto.setAutomatic(on) ?? ""
    }

    private func requestAuth() async {
        auth = await PHPhotoLibrary.requestAuthorization(for: .readWrite)
        // 第一次允许后把系统后台上传也打开（开关默认是开的）
        if auth == .authorized && auto.enabled { setAutomatic(true) }
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

struct TransferRow: View {
    let t: AutoBackup.Transfer

    // 「12/490 MB」；小于 1MB 的显示百分比
    static func progressText(_ t: AutoBackup.Transfer) -> String {
        if t.total < 1 << 20 { return "\(t.sent * 100 / max(t.total, 1))%" }
        return "\(t.sent >> 20)/\(t.total >> 20) MB"
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(t.name).font(.footnote).lineLimit(1).truncationMode(.middle)
                Spacer()
                Text(Self.progressText(t)).font(.caption).monospacedDigit().foregroundStyle(.secondary)
            }
            ProgressView(value: Double(t.sent), total: Double(max(t.total, 1)))
        }
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
