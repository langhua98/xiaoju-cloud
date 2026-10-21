import AppIntents
import BackgroundTasks
import Contacts
import CryptoKit
import EventKit
import Photos
import UIKit

// 自动备份：像 iCloud 那样，充电、连着网的时候把手机上能备份的都增量传到云盘。
//   照片：全部照片放进「相册」；「备份哪些照片」里打开的相册放进「相册/相册名」，隐藏相册放进「隐藏相册」。
//   通讯录（.vcf）、日历和提醒事项（.ics）：每天一份，放进「手机数据/通讯录」这类文件夹（默认私密）。
//   「文件」App 里选好的文件夹：整个同步到「手机数据/文件/文件夹名」，改过的文件再传一份。
// 「备份」页只有一个「自动备份」开关：同时管系统的后台上传扩展（新拍的照片）和下面这个充电时的后台任务（把没传的补齐）。
// 什么时候跑：
//   1. 系统的后台任务（BGProcessingTask，要求接着电源、有网络），一般是夜里充电时；
//   2. 快捷指令自动化「连接充电器时」运行「备份到小桔云盘」（会打开 App，跑完自动锁屏照常）；
//   3. App 里点「现在备份」。
// 导出的原图交给系统的后台 URLSession 上传，App 被挂起、锁屏了也会传完（大视频 8MB 一段交给后台、一段段接着传，断了下次接着传）；传过的记在本地，下次跳过，
// 服务端也按名字 + 大小跳过。苹果不让第三方 App 读的数据（短信、备忘录、健康……）这里做不了。
final class AutoBackup: NSObject, ObservableObject, URLSessionDataDelegate {
    static let shared = AutoBackup()
    static let taskID = "com.langhua98.xiaoju.backup.sync"

    @Published private(set) var running = false
    @Published private(set) var status = UserDefaults.standard.string(forKey: Keys.status) ?? ""
    @Published private(set) var transfers: [Transfer] = [] // 正在传的每个文件和进度
    @Published private(set) var failures: [Failure] = [] // 这一轮没传上去的，下次备份（或点「重试」）再传
    @Published private(set) var pausedUntil = UserDefaults.standard.object(forKey: Keys.pausedUntil) as? Date
    @Published private(set) var lastFinished = UserDefaults.standard.object(forKey: Keys.lastFinished) as? Date

    struct Failure: Codable, Identifiable, Hashable {
        var id: String { name + "\t" + reason }
        let name: String
        let reason: String
    }

    // 一个备份目标（全部照片、某个相册）里 App 确认传过的照片数
    struct Tally {
        var done: Int
        var total: Int
        var remaining: Int { max(total - done, 0) }
    }

    struct Transfer: Identifiable {
        let id: String
        let name: String
        var sent: Int64
        let total: Int64
    }
    @Published var enabled = UserDefaults.standard.object(forKey: Keys.enabled) as? Bool ?? true {
        didSet {
            UserDefaults.standard.set(enabled, forKey: Keys.enabled)
            if enabled { schedule() } else { BGTaskScheduler.shared.cancel(taskRequestWithIdentifier: Self.taskID) }
        }
    }

    var backgroundCompletion: (() -> Void)?

    private enum Keys {
        static let enabled = "auto.enabled"
        static let status = "auto.status"
        static let failures = "auto.failures"
        static let pausedUntil = "auto.pausedUntil"
        static let lastFinished = "auto.lastFinished"
    }

    // 「备份哪些照片」「手机数据」里的开关，默认都开
    static let backupHidden = "backup.hidden"
    static let backupContacts = "backup.contacts"
    static let backupCalendar = "backup.calendar"
    static let backupReminders = "backup.reminders"
    static let selectedAlbums = "selectedAlbums"

    static func flag(_ key: String) -> Bool { UserDefaults.standard.object(forKey: key) as? Bool ?? true }

    private let defaults = UserDefaults.standard
    private let lock = NSLock()
    private var job: Task<Void, Never>?
    private var done: Set<String> = []
    private var inCloud: Set<String> = [] // "target|assetLocalID"：按名字 + 拍摄日期认出云盘里已经有的（见 planMatch）
    private var doneDirty = 0
    private var counts = (uploaded: 0, skipped: 0, failed: 0)
    private var lastError = ""

    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.background(withIdentifier: "com.langhua98.xiaoju.backup.auto")
        config.isDiscretionary = false
        config.httpMaximumConnectionsPerHost = 8 // 默认只有 4 个；服务端有多个 Telegram 机器人轮流存，可以多传几个
        config.sessionSendsLaunchEvents = true
        return URLSession(configuration: config, delegate: self, delegateQueue: nil)
    }()

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }
    private var support: URL {
        let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }
    private var doneFile: URL { support.appendingPathComponent("uploaded.json") }
    private var inCloudFile: URL { support.appendingPathComponent("incloud.json") }
    private var spool: URL {
        let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("upload")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }

    override private init() {
        super.init()
        if let data = try? Data(contentsOf: doneFile), let keys = try? JSONDecoder().decode([String].self, from: data) {
            done = Set(keys)
        }
        if let data = try? Data(contentsOf: inCloudFile), let keys = try? JSONDecoder().decode([String].self, from: data) {
            inCloud = Set(keys)
        }
        if let data = defaults.data(forKey: Keys.failures), let list = try? JSONDecoder().decode([Failure].self, from: data) {
            failed = list
            failures = list
        }
    }

    // MARK: 触发

    // App 启动时（包括被系统在后台叫起来）调用，要在 didFinishLaunching 里注册后台任务。
    func register() {
        _ = session // 接上之前交给系统的上传
        BGTaskScheduler.shared.register(forTaskWithIdentifier: Self.taskID, using: nil) { task in
            self.handle(task as! BGProcessingTask)
        }
    }

    func schedule() {
        guard enabled else { return }
        let req = BGProcessingTaskRequest(identifier: Self.taskID)
        req.requiresExternalPower = true
        req.requiresNetworkConnectivity = true
        req.earliestBeginDate = Date().addingTimeInterval(3600)
        try? BGTaskScheduler.shared.submit(req)
    }

    private func handle(_ task: BGProcessingTask) {
        schedule() // 先排好下一次
        task.expirationHandler = { self.stop() }
        start(reason: "后台任务") { ok in task.setTaskCompleted(success: ok) }
    }

    func stop() { job?.cancel() }

    var isPaused: Bool { (pausedUntil ?? .distantPast) > Date() }

    // 暂停：停掉正在跑的这一轮，到时间之前后台任务、快捷指令都不跑（点「现在备份」会先恢复）。
    func pause(hours: Double) {
        let until = Date().addingTimeInterval(hours * 3600)
        defaults.set(until, forKey: Keys.pausedUntil)
        pausedUntil = until
        stop()
    }

    func resume() {
        defaults.removeObject(forKey: Keys.pausedUntil)
        pausedUntil = nil
    }

    // 「自动备份」开关：充电时的后台任务 + 系统的后台上传扩展一起开关。扩展没开成返回原因。
    func setAutomatic(_ on: Bool) -> String? {
        enabled = on
        if on && PHPhotoLibrary.authorizationStatus(for: .readWrite) != .authorized { return nil }
        do {
            try PHPhotoLibrary.shared().setUploadJobExtensionEnabled(on)
            return nil
        } catch {
            return "系统后台上传没打开：\(error.localizedDescription)"
        }
    }

    func start(reason: String, completion: ((Bool) -> Void)? = nil) {
        lock.lock()
        if job != nil {
            lock.unlock()
            completion?(true)
            return
        }
        if isPaused {
            lock.unlock()
            setStatus("\(Self.time()) 暂停中，没有备份（\(reason)）")
            completion?(true)
            return
        }
        counts = (0, 0, 0)
        lastError = ""
        failed = [] // 这一轮会把上次没传上去的再传一遍
        job = Task.detached(priority: .utility) { [self] in
            await run(reason: reason)
            lock.withLock { job = nil }
            completion?(!Task.isCancelled)
        }
        lock.unlock()
        publishFailures()
    }

    // MARK: 统计：「手机上 / 已备份 / 还剩」

    // 全部照片（不含隐藏的），新的在前；备份和统计用同一个
    static func allPhotos() -> PHFetchResult<PHAsset> {
        let all = PHFetchOptions()
        all.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: false)]
        return PHAsset.fetchAssets(with: all)
    }

    // target 和备份时记录的一样："all"、"hidden"、"album:相册名"。照片或视频有一份传过就算。
    // 只算核对过的：系统后台扩展刚传的，下次备份核对过后才算进来。快捷指令、网页传的按名字 + 拍摄日期认出来的也算。
    func tally(target: String, assets: PHFetchResult<PHAsset>) -> Tally {
        let prefix = target + "|"
        let ids: Set<String> = lock.withLock {
            Set(done.compactMap { key -> String? in
                guard key.hasPrefix(prefix) else { return nil }
                let rest = key.dropFirst(prefix.count)
                return rest.lastIndex(of: "|").map { String(rest[..<$0]) }
            }).union(inCloud.compactMap { $0.hasPrefix(prefix) ? String($0.dropFirst(prefix.count)) : nil })
        }
        var n = 0
        assets.enumerateObjects { asset, _, _ in
            if ids.contains(asset.localIdentifier) { n += 1 }
        }
        return Tally(done: n, total: assets.count)
    }

    // 「释放手机空间」能删的：全部照片里比 before 早、照片和视频（实况照片的视频部分也算）都确认在云盘里的。
    // 个人收藏的不删。
    func fullyBackedUp(before: Date) -> [PHAsset] {
        let keys = lock.withLock { done }
        var out: [PHAsset] = []
        Self.allPhotos().enumerateObjects { asset, _, _ in
            guard !asset.isFavorite, (asset.creationDate ?? .distantFuture) < before else { return }
            let resources = PHAssetResource.assetResources(for: asset).filter { [.photo, .video, .pairedVideo].contains($0.type) }
            if !resources.isEmpty && resources.allSatisfy({ keys.contains("all|\(asset.localIdentifier)|\($0.type.rawValue)") }) {
                out.append(asset)
            }
        }
        return out
    }

    // MARK: 失败

    private var failed: [Failure] = []

    private func fail(_ name: String, _ reason: String) {
        lastError = "\(name)：\(reason)"
        lock.withLock {
            failed.removeAll { $0.name == name }
            failed.append(Failure(name: name, reason: reason))
        }
        publishFailures()
    }

    private func publishFailures() {
        let list = lock.withLock { failed }
        if let data = try? JSONEncoder().encode(list) { defaults.set(data, forKey: Keys.failures) }
        DispatchQueue.main.async { self.failures = list }
    }

    // MARK: 备份

    private func run(reason: String) async {
        await MainActor.run {
            running = true
            UIApplication.shared.isIdleTimerDisabled = true // 开着 App 跑的时候不自动锁屏（充电时）
        }
        setStatus("正在备份（\(reason)）…")
        await resumeBigJobs()
        await backupData()
        await backupPhotos()
        await backupFolders()
        saveDone(force: true)
        if !Task.isCancelled {
            let now = Date()
            defaults.set(now, forKey: Keys.lastFinished)
            await MainActor.run { lastFinished = now }
        }
        let head = Task.isCancelled ? "备份中断" : "备份完成"
        setStatus("\(Self.time()) \(head)：新传 \(counts.uploaded)，已有跳过 \(counts.skipped)，失败 \(counts.failed)"
            + (lastError.isEmpty ? "" : "\n最后一个错误：\(lastError)")
            + (counts.uploaded > 0 ? "\n大文件会在后台继续传完" : ""))
        await MainActor.run {
            running = false
            UIApplication.shared.isIdleTimerDisabled = false
        }
        schedule()
    }

    // 照片：隐藏相册 → 隐藏相册（总是最先传）；全部照片 → 相册；勾选的相册 → 相册/名字。
    private func backupPhotos() async {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else {
            fail("照片", "没有照片权限")
            return
        }
        for target in photoTargets() {
            let assets = target.assets
            await reconcile(target: target.key, assets: assets, album: target.album)
            for i in 0..<assets.count {
                if Task.isCancelled { return }
                if i % 20 == 0 {
                    setStatus("正在备份「\(target.label)」\(i)/\(assets.count)：新传 \(counts.uploaded)，已有 \(counts.skipped)，失败 \(counts.failed)")
                }
                let asset = assets.object(at: i)
                let known = lock.withLock { inCloud.contains("\(target.key)|\(asset.localIdentifier)") }
                for resource in PHAssetResource.assetResources(for: asset) where [.photo, .video, .pairedVideo].contains(resource.type) {
                    let key = "\(target.key)|\(asset.localIdentifier)|\(resource.type.rawValue)"
                    if lock.withLock({ done.contains(key) }) { continue }
                    if known && resource.type != .pairedVideo { continue } // 快捷指令、网页传过这张：不重传（实况照片的视频照常问）
                    do {
                        try await upload(resource, key: key, album: target.album)
                    } catch {
                        counts.failed += 1
                        fail(resource.originalFilename, error.localizedDescription)
                    }
                }
            }
        }
    }

    typealias PhotoTarget = (key: String, label: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?)

    // 要备份的：隐藏相册（总是最先传）、全部照片（新的先传）、「备份哪些照片」里打开的相册
    private func photoTargets() -> [PhotoTarget] {
        var targets: [PhotoTarget] = []
        let albums = PhoneAlbum.all()
        for album in albums where album.hidden && Self.flag(Self.backupHidden) {
            targets.append(("hidden", album.title, album.assets(), album))
        }
        targets.append(("all", "全部照片", Self.allPhotos(), nil))
        let selected = Set(defaults.stringArray(forKey: Self.selectedAlbums) ?? [])
        for album in albums where !album.hidden && selected.contains(album.id) {
            targets.append(("album:\(album.title)", album.title, album.assets(), album))
        }
        return targets
    }

    // 云盘里已经有的（快捷指令、网页、之前装的 App 传过的）：取一次这个相册里每个文件“名字|大小”的摘要
    // （GET /api/ingest/names），在手机上比对，对上的记成传过。这样「已备份」一打开就是对的，备份时也不用每张都去问。
    func reconcile(target: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?) async {
        await matchNames(target: target, assets: assets, album: album)
        await planMatch(target: target, assets: assets, album: album)
    }

    private func matchNames(target: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?) async {
        guard let url = URL(string: server + "/api/ingest/names") else { return }
        var req = URLRequest(url: url)
        if let album {
            if album.hidden {
                req.setValue("hidden", forHTTPHeaderField: "X-Xiaoju-Album")
            } else {
                req.setValue(encodedName(album.title), forHTTPHeaderField: "X-Xiaoju-Album-Name")
            }
        }
        guard let reply = try? await URLSession.shared.data(for: req),
              (reply.1 as? HTTPURLResponse)?.statusCode == 200 else { return }
        let known = Set(String(decoding: reply.0, as: UTF8.self).split(separator: "\n").map(String.init))
        guard !known.isEmpty else { return }
        var found = 0
        assets.enumerateObjects { asset, _, _ in
            for resource in PHAssetResource.assetResources(for: asset) where [.photo, .video, .pairedVideo].contains(resource.type) {
                let key = "\(target)|\(asset.localIdentifier)|\(resource.type.rawValue)"
                guard let size = resource.value(forKey: "fileSize") as? Int64, size > 0,
                      known.contains(Self.sha("\(resource.originalFilename)|\(size)")),
                      !self.lock.withLock({ self.done.contains(key) }) else { continue }
                self.markDone(key)
                found += 1
            }
        }
        if found > 0 { saveDone(force: true) }
    }

    // 快捷指令、网页传的照片常常和手机上的原件大小不一样（导出时重新编码），按“名字|大小”认不出来，
    // 「已备份」就只剩 App 自己传的几十张。所以再把每张的名字和拍摄日期一次发给服务端（POST /api/ingest/plan，
    // 和快捷指令同一套比法：名字不分大小写、不比扩展名，拍摄日期差一天以内），认出来的记进 inCloud：
    // 算进「已备份」，备份时不再重传；但「释放手机空间」只认按原件核对过的（done），不会因为这个删照片。
    // 服务端只认顶层的「相册」「隐藏相册」，勾选的相册（相册/名字）还是只按“名字|大小”认。
    private func planMatch(target: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?) async {
        guard album?.hidden ?? true, let url = URL(string: server + "/api/ingest/plan") else { return }
        let day = DateFormatter()
        day.locale = Locale(identifier: "en_US_POSIX")
        day.dateFormat = "yyyy-MM-dd"
        var ids: [String] = [], names: [String] = [], dates: [String] = []
        assets.enumerateObjects { asset, _, _ in
            let resources = PHAssetResource.assetResources(for: asset)
            guard let main = resources.first(where: { $0.type == .photo || $0.type == .video }) else { return }
            ids.append(asset.localIdentifier)
            names.append(main.originalFilename.replacingOccurrences(of: "\n", with: " "))
            dates.append(asset.creationDate.map(day.string(from:)) ?? "")
        }
        guard !ids.isEmpty else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let body: [String: String] = ["album": album == nil ? "" : "hidden",
                                      "names": names.joined(separator: "\n"), "dates": dates.joined(separator: "\n")]
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        struct Plan: Decodable {
            let todo: String
            let files: Int
        }
        guard let reply = try? await URLSession.shared.data(for: req),
              (reply.1 as? HTTPURLResponse)?.statusCode == 200,
              let plan = try? JSONDecoder().decode(Plan.self, from: reply.0), plan.files == ids.count else { return }
        let todo = Set(plan.todo.split(separator: "\n").compactMap { Int($0) })
        let prefix = target + "|"
        let keys = lock.withLock { () -> [String] in
            inCloud = inCloud.filter { !$0.hasPrefix(prefix) }
            for (i, id) in ids.enumerated() where !todo.contains(i + 1) { inCloud.insert(prefix + id) }
            return Array(inCloud)
        }
        if let data = try? JSONEncoder().encode(keys) { try? data.write(to: inCloudFile, options: .atomic) }
    }

    // 「备份」页打开时：把每个要备份的相册都和云盘比对一遍（同一时间只跑一个）
    func reconcileAll() async {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized,
              lock.withLock({ () -> Bool in
                  if reconciling { return false }
                  reconciling = true
                  return true
              }) else { return }
        defer { lock.withLock { reconciling = false } }
        for target in photoTargets() {
            await reconcile(target: target.key, assets: target.assets, album: target.album)
        }
    }

    private var reconciling = false

    // 放进哪个相册的头部：隐藏相册 X-Xiaoju-Album: hidden，其他相册 X-Xiaoju-Album-Name: 相册名
    private func albumHeaders(_ album: PhoneAlbum?) -> [String: String] {
        guard let album else { return [:] }
        return album.hidden ? ["X-Xiaoju-Album": "hidden"] : ["X-Xiaoju-Album-Name": encodedName(album.title)]
    }

    private func upload(_ resource: PHAssetResource, key: String, album: PhoneAlbum?) async throws {
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
        let size = resource.value(forKey: "fileSize") as? Int64 ?? 0
        try await send(name: resource.originalFilename, size: size, key: key, extra: albumHeaders(album)) { dest in
            try await PHAssetResourceManager.default().writeData(for: resource, toFile: dest, options: opts)
        }
    }

    // 先问服务端有没有；没有就用 export 把内容写到本地，交给后台上传（同时最多 10 个排队，不占满手机空间）。
    // extra 是放进哪个文件夹的头部（X-Xiaoju-Album、X-Xiaoju-Album-Name）。
    private func send(name: String, size: Int64, key: String, extra: [String: String],
                      export: (URL) async throws -> Void) async throws
    {
        guard let base = URL(string: server) else { throw URLError(.badURL) }
        if size > 0 {
            var ask = URLRequest(url: base.appendingPathComponent("api/ingest/exists"))
            ask.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
            for (k, v) in extra { ask.setValue(v, forHTTPHeaderField: k) }
            ask.setValue(String(size), forHTTPHeaderField: "X-File-Size")
            let (data, _) = try await URLSession.shared.data(for: ask)
            if (try? JSONDecoder().decode([String: String].self, from: data))?["result"] == "exists" {
                markDone(key)
                counts.skipped += 1
                return
            }
        }
        // 大文件（视频）：分段断点续传，断了下次从断的地方接着传。导出的文件留着，下次不用再导出
        if size > 16 << 20 {
            let file = spool.appendingPathComponent(Self.sha(key))
            if !FileManager.default.fileExists(atPath: file.path) {
                let part = file.appendingPathExtension("part")
                try? FileManager.default.removeItem(at: part)
                try await export(part)
                try FileManager.default.moveItem(at: part, to: file)
            }
            // 同时最多 3 个大文件在后台分段传
            while await chunkTasks() >= 3 {
                if Task.isCancelled { return }
                try await Task.sleep(nanoseconds: 2_000_000_000)
            }
            let job = BigJob(key: key, file: file.path, name: name, headers: extra, size: size)
            if try await startBig(job) { counts.uploaded += 1 } else { counts.skipped += 1 }
            return
        }
        while await session.allTasks.count >= 10 {
            if Task.isCancelled { return }
            try await Task.sleep(nanoseconds: 1_000_000_000)
        }
        let file = spool.appendingPathComponent(UUID().uuidString)
        try await export(file)
        var req = URLRequest(url: base.appendingPathComponent("api/ingest"))
        req.httpMethod = "POST"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        req.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
        for (k, v) in extra { req.setValue(v, forHTTPHeaderField: k) }
        let task = session.uploadTask(with: req, fromFile: file)
        task.taskDescription = key + "\t" + file.path
        lock.withLock { lastNames[key] = name }
        track(key, name: name, sent: 0, total: size)
        task.resume()
        counts.uploaded += 1
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.withLock { replies[dataTask.taskIdentifier, default: Data()].append(data) }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        let reply = lock.withLock { replies.removeValue(forKey: task.taskIdentifier) } ?? Data()
        if task.taskDescription?.hasPrefix("chunk\t") == true {
            chunkDone(task, reply: reply, error: error)
            return
        }
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        untrack(parts[0])
        defer { lock.withLock { lastNames[parts[0]] = nil } }
        if parts.count == 2 { try? FileManager.default.removeItem(atPath: parts[1]) }
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        if error == nil, (200..<300).contains(code), let key = parts.first {
            markDone(key)
            saveDone(force: false)
        } else {
            let name = lock.withLock { lastNames[parts[0]] } ?? "照片"
            fail(name, error?.localizedDescription ?? "服务器返回 \(code)")
        }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didSendBodyData bytesSent: Int64,
                    totalBytesSent: Int64, totalBytesExpectedToSend: Int64)
    {
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        if parts.first == "chunk", parts.count == 4, let end = Int64(parts[3]) {
            // 大文件：这一段的起点 + 这一段已发出的
            let sent = end - task.countOfBytesExpectedToSend + totalBytesSent
            if lock.withLock({ meta[parts[1]] }) == nil, let job = loadJobs()[parts[1]] {
                lock.withLock { meta[job.key] = (job.name, job.size) }
            }
            updateProgress(parts[1], sent: sent)
        } else if let key = parts.first {
            lock.withLock {
                if (meta[key]?.total ?? 0) <= 0 { meta[key] = (meta[key]?.name ?? "照片", totalBytesExpectedToSend) }
            }
            updateProgress(key, sent: totalBytesSent)
        }
    }

    // MARK: 进度：每个正在传的文件一条，界面上每半秒刷新一次

    private var meta: [String: (name: String, total: Int64)] = [:]
    private var lastNames: [String: String] = [:] // 交给后台的文件名，失败时显示
    private var sentBytes: [String: Int64] = [:]
    private var lastPublish = Date.distantPast

    private func track(_ key: String, name: String, sent: Int64, total: Int64) {
        lock.withLock {
            meta[key] = (name, total)
            sentBytes[key] = sent
        }
        publishProgress(force: true)
    }

    private func untrack(_ key: String) {
        lock.withLock {
            meta[key] = nil
            sentBytes[key] = nil
        }
        publishProgress(force: true)
    }

    private func updateProgress(_ key: String, sent: Int64) {
        lock.withLock { sentBytes[key] = sent }
        publishProgress(force: false)
    }

    private func publishProgress(force: Bool) {
        let list: [Transfer]? = lock.withLock {
            guard force || Date().timeIntervalSince(lastPublish) > 0.5 else { return nil }
            lastPublish = Date()
            return sentBytes.compactMap { key, sent in
                meta[key].map { Transfer(id: key, name: $0.name, sent: min(sent, $0.total), total: $0.total) }
            }.sorted { $0.name < $1.name }
        }
        guard let list else { return }
        DispatchQueue.main.async { self.transfers = list }
    }

    func urlSessionDidFinishEvents(forBackgroundURLSession session: URLSession) {
        saveDone(force: true)
        DispatchQueue.main.async {
            self.backgroundCompletion?()
            self.backgroundCompletion = nil
        }
    }

    // MARK: 大文件：交给系统后台一段段传（8MB 一段，传完一段在回调里接着交下一段，锁屏、App 被挂起也不停）

    struct BigJob: Codable {
        var key: String
        var file: String // 导出的原文件，传完才删
        var name: String
        var headers: [String: String]
        var size: Int64
        var id = ""
        var chunk: Int64 = 8 << 20
    }

    private var replies: [Int: Data] = [:]
    private var jobsFile: URL { support.appendingPathComponent("bigjobs.json") }

    private func loadJobs() -> [String: BigJob] {
        guard let data = try? Data(contentsOf: jobsFile) else { return [:] }
        return (try? JSONDecoder().decode([String: BigJob].self, from: data)) ?? [:]
    }

    private func saveJob(_ job: BigJob?, key: String) {
        lock.withLock {
            var jobs = loadJobs()
            jobs[key] = job
            if let data = try? JSONEncoder().encode(jobs) { try? data.write(to: jobsFile, options: .atomic) }
        }
    }

    private func chunkTasks() async -> Int {
        await session.allTasks.filter { $0.taskDescription?.hasPrefix("chunk\t") == true }.count
    }

    // 开始（或接着）传一个大文件：问服务端收到了多少，从那里交出下一段。服务端早就有了返回 false。
    private func startBig(_ job: BigJob) async throws -> Bool {
        var job = job
        guard let url = URL(string: server + "/api/ingest/uploads") else { throw URLError(.badURL) }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue(encodedName(job.name), forHTTPHeaderField: "X-File-Name")
        req.setValue(String(job.size), forHTTPHeaderField: "X-File-Size")
        for (k, v) in job.headers { req.setValue(v, forHTTPHeaderField: k) }
        let (data, resp) = try await URLSession.shared.data(for: req)
        let out = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
            throw NSError(domain: "xiaoju", code: 0, userInfo: [NSLocalizedDescriptionKey: String(decoding: data, as: UTF8.self)])
        }
        if out["result"] as? String == "skipped" {
            finishBig(job)
            return false
        }
        job.id = out["id"] as? String ?? ""
        job.chunk = max((out["chunk"] as? NSNumber)?.int64Value ?? job.chunk, 1 << 20)
        saveJob(job, key: job.key)
        try queueChunk(job, offset: (out["offset"] as? NSNumber)?.int64Value ?? 0)
        return true
    }

    private func queueChunk(_ job: BigJob, offset: Int64) throws {
        guard let handle = FileHandle(forReadingAtPath: job.file) else { throw URLError(.fileDoesNotExist) }
        defer { try? handle.close() }
        try handle.seek(toOffset: UInt64(offset))
        let piece = try handle.read(upToCount: Int(min(job.chunk, job.size - offset))) ?? Data()
        let chunkFile = spool.appendingPathComponent("\(job.id)-\(offset)")
        try piece.write(to: chunkFile)
        var req = URLRequest(url: URL(string: server + "/api/ingest/uploads/\(job.id)?offset=\(offset)")!)
        req.httpMethod = "PUT"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        let task = session.uploadTask(with: req, fromFile: chunkFile)
        task.taskDescription = ["chunk", job.key, chunkFile.path, String(offset + Int64(piece.count))].joined(separator: "\t")
        track(job.key, name: job.name, sent: offset, total: job.size)
        task.resume()
        setStatus("后台在传 \(job.name)：\(offset >> 20)/\(job.size >> 20) MB")
    }

    private func chunkDone(_ task: URLSessionTask, reply: Data, error: Error?) {
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        guard parts.count == 4, let job = loadJobs()[parts[1]] else { return }
        try? FileManager.default.removeItem(atPath: parts[2])
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        let out = (try? JSONSerialization.jsonObject(with: reply)) as? [String: Any] ?? [:]
        let next = (out["offset"] as? NSNumber)?.int64Value ?? Int64(parts[3]) ?? 0
        do {
            switch code {
            case 201:
                finishBig(job)
            case 200 where out["same_content"] as? Bool == true:
                finishBig(job)
            case 200, 409:
                try queueChunk(job, offset: next)
            default:
                throw NSError(domain: "xiaoju", code: code, userInfo: [NSLocalizedDescriptionKey:
                    error?.localizedDescription ?? "HTTP \(code) \(String(decoding: reply, as: UTF8.self))"])
            }
        } catch {
            // 断了：留着这个任务，下次备份时问服务端收到多少接着传
            untrack(job.key)
            fail(job.name, "\(error.localizedDescription)（下次接着传）")
            setStatus("\(Self.time()) \(lastError)")
        }
    }

    private func finishBig(_ job: BigJob) {
        untrack(job.key)
        try? FileManager.default.removeItem(atPath: job.file)
        saveJob(nil, key: job.key)
        markDone(job.key)
        saveDone(force: true)
        setStatus("\(Self.time()) 后台传完 \(job.name)")
    }

    // 上次没传完的大文件（断网、App 被杀）：接着传。
    private func resumeBigJobs() async {
        let active = Set(await session.allTasks.compactMap { $0.taskDescription?.components(separatedBy: "\t") }
            .filter { $0.first == "chunk" && $0.count > 1 }.map { $0[1] })
        for job in loadJobs().values where !active.contains(job.key) {
            if Task.isCancelled { return }
            guard FileManager.default.fileExists(atPath: job.file) else {
                saveJob(nil, key: job.key) // 文件被系统清掉了：下次从头导出
                continue
            }
            do { _ = try await startBig(job) } catch { fail(job.name, error.localizedDescription) }
        }
    }

    private func markDone(_ key: String) {
        lock.withLock {
            done.insert(key)
            doneDirty += 1
        }
    }

    private func saveDone(force: Bool) {
        let keys: [String]? = lock.withLock {
            guard force || doneDirty >= 20 else { return nil }
            doneDirty = 0
            return Array(done)
        }
        if let keys, let data = try? JSONEncoder().encode(keys) {
            try? data.write(to: doneFile, options: .atomic)
        }
    }

    // MARK: 「文件」App 里的文件夹：选好的文件夹整个同步到「手机数据/文件/文件夹名/…」，子文件夹照搬

    struct SyncFolder: Codable, Identifiable {
        var id: String
        var name: String
        var bookmark: Data
    }

    @Published private(set) var folders: [SyncFolder] = {
        guard let data = UserDefaults.standard.data(forKey: "sync.folders") else { return [] }
        return (try? JSONDecoder().decode([SyncFolder].self, from: data)) ?? []
    }()

    private func saveFolders(_ list: [SyncFolder]) {
        defaults.set(try? JSONEncoder().encode(list), forKey: "sync.folders")
        DispatchQueue.main.async { self.folders = list }
    }

    // 「文件」App 选来的文件夹：存一个书签，以后（包括后台）还能打开它
    func addFolder(_ url: URL) throws {
        let ok = url.startAccessingSecurityScopedResource()
        defer { if ok { url.stopAccessingSecurityScopedResource() } }
        let bookmark = try url.bookmarkData(options: [], includingResourceValuesForKeys: nil, relativeTo: nil)
        var list = folders.filter { $0.name != url.lastPathComponent }
        list.append(SyncFolder(id: UUID().uuidString, name: url.lastPathComponent, bookmark: bookmark))
        saveFolders(list)
    }

    func removeFolder(_ id: String) {
        saveFolders(folders.filter { $0.id != id })
    }

    private func backupFolders() async {
        for folder in folders {
            if Task.isCancelled { return }
            var stale = false
            guard let root = try? URL(resolvingBookmarkData: folder.bookmark, options: [], relativeTo: nil, bookmarkDataIsStale: &stale) else {
                lastError = "打不开文件夹「\(folder.name)」，在 App 里删掉重新选一次"
                continue
            }
            let ok = root.startAccessingSecurityScopedResource()
            defer { if ok { root.stopAccessingSecurityScopedResource() } }
            if stale, let fresh = try? root.bookmarkData(options: [], includingResourceValuesForKeys: nil, relativeTo: nil) {
                saveFolders(folders.map { $0.id == folder.id ? SyncFolder(id: $0.id, name: $0.name, bookmark: fresh) : $0 })
            }
            // 「文件」里的 iCloud 云盘文件可能还没下载：用 NSFileCoordinator 读的时候系统会先下载
            let keys: [URLResourceKey] = [.isRegularFileKey, .fileSizeKey, .contentModificationDateKey]
            let files = Self.listFiles(root, keys: keys)
            for (i, file) in files.enumerated() {
                if Task.isCancelled { return }
                guard let v = try? file.resourceValues(forKeys: Set(keys)), v.isRegularFile == true else { continue }
                if i % 20 == 0 {
                    setStatus("正在同步文件夹「\(folder.name)」\(i)/\(files.count)：新传 \(counts.uploaded)，已有 \(counts.skipped)，失败 \(counts.failed)")
                }
                let rel = String(file.path.dropFirst(root.path.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
                let size = Int64(v.fileSize ?? 0)
                let mtime = Int(v.contentModificationDate?.timeIntervalSince1970 ?? 0)
                let key = "file|\(folder.id)|\(rel)|\(size)|\(mtime)"
                if lock.withLock({ done.contains(key) }) { continue }
                let dir = ([ "文件", folder.name ] + rel.split(separator: "/").dropLast().map(String.init)).joined(separator: "/")
                do {
                    try await send(name: file.lastPathComponent, size: size, key: key,
                                   extra: ["X-Xiaoju-Album": "data", "X-Xiaoju-Album-Name": encodedName(dir)]) { dest in
                        try Self.coordinatedCopy(file, to: dest)
                    }
                } catch {
                    counts.failed += 1
                    fail(file.lastPathComponent, error.localizedDescription)
                }
            }
        }
    }

    // 同步函数里列文件（目录枚举器不能在 async 函数里直接遍历）
    private static func listFiles(_ root: URL, keys: [URLResourceKey]) -> [URL] {
        let walker = FileManager.default.enumerator(at: root, includingPropertiesForKeys: keys,
                                                    options: [.skipsHiddenFiles, .skipsPackageDescendants])
        var out: [URL] = []
        while let url = walker?.nextObject() as? URL { out.append(url) }
        return out
    }

    private static func coordinatedCopy(_ src: URL, to dest: URL) throws {
        var coordError: NSError?
        var copyError: Error?
        NSFileCoordinator().coordinate(readingItemAt: src, options: [], error: &coordError) { url in
            do { try FileManager.default.copyItem(at: url, to: dest) } catch { copyError = error }
        }
        if let e = coordError ?? copyError { throw e }
    }

    // MARK: 通讯录、日历、提醒事项

    private func backupData() async {
        let day = Self.day()
        let contacts = Self.flag(Self.backupContacts), events = Self.flag(Self.backupCalendar), todos = Self.flag(Self.backupReminders)
        if contacts && CNContactStore.authorizationStatus(for: .contacts) == .notDetermined {
            _ = try? await CNContactStore().requestAccess(for: .contacts)
        }
        if contacts && CNContactStore.authorizationStatus(for: .contacts) == .authorized, let vcf = contactsVCard() {
            await uploadData(vcf, name: "通讯录 \(day).vcf", folder: "通讯录")
        }
        let store = EKEventStore()
        if events && EKEventStore.authorizationStatus(for: .event) == .notDetermined {
            _ = try? await store.requestFullAccessToEvents()
        }
        if events && EKEventStore.authorizationStatus(for: .event) == .fullAccess {
            await uploadData(Data(calendarICS(store).utf8), name: "日历 \(day).ics", folder: "日历")
        }
        if todos && EKEventStore.authorizationStatus(for: .reminder) == .notDetermined {
            _ = try? await store.requestFullAccessToReminders()
        }
        if todos && EKEventStore.authorizationStatus(for: .reminder) == .fullAccess {
            await uploadData(Data(await remindersICS(store).utf8), name: "提醒事项 \(day).ics", folder: "提醒事项")
        }
    }

    private func uploadData(_ data: Data, name: String, folder: String) async {
        guard let url = URL(string: server + "/api/ingest") else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
        req.setValue("data", forHTTPHeaderField: "X-Xiaoju-Album")
        req.setValue(encodedName(folder), forHTTPHeaderField: "X-Xiaoju-Album-Name")
        do {
            let (body, resp) = try await URLSession.shared.upload(for: req, from: data)
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            if code == 201 {
                counts.uploaded += 1
            } else if code == 200 {
                counts.skipped += 1
            } else {
                counts.failed += 1
                fail(name, "服务器返回 \(code) \(String(decoding: body, as: UTF8.self))")
            }
        } catch {
            counts.failed += 1
            fail(name, error.localizedDescription)
        }
    }

    private func contactsVCard() -> Data? {
        let keys = [CNContactVCardSerialization.descriptorForRequiredKeys()]
        var contacts: [CNContact] = []
        try? CNContactStore().enumerateContacts(with: CNContactFetchRequest(keysToFetch: keys)) { c, _ in contacts.append(c) }
        guard !contacts.isEmpty else { return nil }
        return try? CNContactVCardSerialization.data(with: contacts)
    }

    // 日历：过去 10 年到未来 2 年的日程（系统一次最多查 4 年，分段查）。
    private func calendarICS(_ store: EKEventStore) -> String {
        var seen = Set<String>()
        var out = ""
        let now = Date()
        var from = Calendar.current.date(byAdding: .year, value: -10, to: now)!
        let end = Calendar.current.date(byAdding: .year, value: 2, to: now)!
        while from < end {
            let to = min(Calendar.current.date(byAdding: .year, value: 3, to: from)!, end)
            for e in store.events(matching: store.predicateForEvents(withStart: from, end: to, calendars: nil)) {
                let id = (e.calendarItemExternalIdentifier ?? e.eventIdentifier ?? UUID().uuidString) + "@" + Self.icsDate(e.startDate)
                guard seen.insert(id).inserted else { continue }
                out += "BEGIN:VEVENT\r\nUID:\(Self.icsText(id))\r\n"
                if e.isAllDay {
                    out += "DTSTART;VALUE=DATE:\(Self.icsDay(e.startDate))\r\nDTEND;VALUE=DATE:\(Self.icsDay(e.endDate.addingTimeInterval(1)))\r\n"
                } else {
                    out += "DTSTART:\(Self.icsDate(e.startDate))\r\nDTEND:\(Self.icsDate(e.endDate))\r\n"
                }
                out += "SUMMARY:\(Self.icsText(e.title ?? ""))\r\n"
                if let v = e.location, !v.isEmpty { out += "LOCATION:\(Self.icsText(v))\r\n" }
                if let v = e.notes, !v.isEmpty { out += "DESCRIPTION:\(Self.icsText(v))\r\n" }
                out += "CATEGORIES:\(Self.icsText(e.calendar.title))\r\nEND:VEVENT\r\n"
            }
            from = to
        }
        return Self.calendar(out)
    }

    private func remindersICS(_ store: EKEventStore) async -> String {
        let reminders: [EKReminder] = await withCheckedContinuation { cont in
            store.fetchReminders(matching: store.predicateForReminders(in: nil)) { cont.resume(returning: $0 ?? []) }
        }
        var out = ""
        for r in reminders {
            out += "BEGIN:VTODO\r\nUID:\(Self.icsText(r.calendarItemExternalIdentifier ?? r.calendarItemIdentifier))\r\n"
            out += "SUMMARY:\(Self.icsText(r.title ?? ""))\r\n"
            if let due = r.dueDateComponents?.date { out += "DUE:\(Self.icsDate(due))\r\n" }
            out += "STATUS:\(r.isCompleted ? "COMPLETED" : "NEEDS-ACTION")\r\n"
            if let v = r.notes, !v.isEmpty { out += "DESCRIPTION:\(Self.icsText(v))\r\n" }
            out += "CATEGORIES:\(Self.icsText(r.calendar.title))\r\nEND:VTODO\r\n"
        }
        return Self.calendar(out)
    }

    private static func calendar(_ body: String) -> String {
        "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//xiaoju//backup//CN\r\n" + body + "END:VCALENDAR\r\n"
    }

    private static func sha(_ s: String) -> String {
        SHA256.hash(data: Data(s.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    private static func icsText(_ s: String) -> String {
        s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: ";", with: "\\;")
            .replacingOccurrences(of: ",", with: "\\,").replacingOccurrences(of: "\r\n", with: "\\n")
            .replacingOccurrences(of: "\n", with: "\\n")
    }

    private static func format(_ pattern: String, utc: Bool) -> DateFormatter {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = utc ? TimeZone(identifier: "UTC") : .current
        f.dateFormat = pattern
        return f
    }

    private static func icsDate(_ d: Date) -> String { format("yyyyMMdd'T'HHmmss'Z'", utc: true).string(from: d) }
    private static func icsDay(_ d: Date) -> String { format("yyyyMMdd", utc: false).string(from: d) }
    private static func day() -> String { format("yyyy-MM-dd", utc: false).string(from: Date()) }
    private static func time() -> String { format("MM-dd HH:mm", utc: false).string(from: Date()) }

    private func setStatus(_ text: String) {
        defaults.set(text, forKey: Keys.status)
        DispatchQueue.main.async { self.status = text }
    }
}

// 快捷指令里的「备份到小桔云盘」：自动化选「充电器 → 已连接」→「立即运行」，加这个操作。
struct BackupNowIntent: AppIntent {
    static var title: LocalizedStringResource = "备份到小桔云盘"
    static var description = IntentDescription("把照片、相册、通讯录、日历、提醒事项增量备份到小桔云盘。")
    static var openAppWhenRun = true // 开着 App 跑才能导出照片；跑完照常自动锁屏

    func perform() async throws -> some IntentResult {
        AutoBackup.shared.start(reason: "快捷指令")
        return .result()
    }
}

struct XiaojuShortcuts: AppShortcutsProvider {
    static var appShortcuts: [AppShortcut] {
        AppShortcut(intent: BackupNowIntent(), phrases: ["用\(.applicationName)备份", "\(.applicationName)备份"],
                    shortTitle: "备份到小桔云盘", systemImageName: "arrow.up.circle")
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        // 缩略图走系统的网络缓存：照片页来回滑不用重下
        URLCache.shared = URLCache(memoryCapacity: 64 << 20, diskCapacity: 1 << 30)
        AutoBackup.shared.register()
        AutoBackup.shared.schedule()
        return true
    }

    func applicationDidEnterBackground(_ application: UIApplication) {
        AutoBackup.shared.schedule()
    }

    func application(_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String,
                     completionHandler: @escaping () -> Void) {
        AutoBackup.shared.backgroundCompletion = completionHandler
    }
}
