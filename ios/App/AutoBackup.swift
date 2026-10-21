import AppIntents
import BackgroundTasks
import Contacts
import CryptoKit
import EventKit
import Photos
import UIKit

// 自动备份：像 iCloud 那样，充电、连着网的时候把手机上能备份的都增量传到云盘。
//   照片：全部照片放进「相册」；在「相册」页勾选的相册放进「相册/相册名」，隐藏相册放进「隐藏相册」。
//   通讯录（.vcf）、日历和提醒事项（.ics）：每天一份，放进「手机数据/通讯录」这类文件夹（默认私密）。
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
    }

    private let defaults = UserDefaults.standard
    private let lock = NSLock()
    private var job: Task<Void, Never>?
    private var done: Set<String> = []
    private var doneDirty = 0
    private var counts = (uploaded: 0, skipped: 0, failed: 0)
    private var lastError = ""

    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.background(withIdentifier: "com.langhua98.xiaoju.backup.auto")
        config.isDiscretionary = false
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

    func start(reason: String, completion: ((Bool) -> Void)? = nil) {
        lock.lock()
        if job != nil {
            lock.unlock()
            completion?(true)
            return
        }
        if let other = UploadGate.enter("现在备份") {
            lock.unlock()
            setStatus("\(Self.time()) 没有开始备份：「\(other)」正在上传，等它传完再备份")
            completion?(true)
            return
        }
        counts = (0, 0, 0)
        lastError = ""
        job = Task.detached(priority: .utility) { [self] in
            await run(reason: reason)
            lock.withLock { job = nil }
            UploadGate.leave()
            completion?(!Task.isCancelled)
        }
        lock.unlock()
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
        saveDone(force: true)
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
            lastError = "没有相册权限"
            return
        }
        var targets: [(key: String, label: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?)] = []
        let albums = PhoneAlbum.all()
        for album in albums where album.hidden {
            targets.append(("hidden", album.title, album.assets(), album))
        }
        let all = PHFetchOptions()
        all.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: false)] // 新的先传
        targets.append(("all", "全部照片", PHAsset.fetchAssets(with: all), nil))
        let selected = Set(defaults.stringArray(forKey: "selectedAlbums") ?? [])
        for album in albums where !album.hidden && selected.contains(album.id) {
            targets.append(("album:\(album.title)", album.title, album.assets(), album))
        }
        for target in targets {
            let assets = target.assets
            for i in 0..<assets.count {
                if Task.isCancelled { return }
                if i % 20 == 0 {
                    setStatus("正在备份「\(target.label)」\(i)/\(assets.count)：新传 \(counts.uploaded)，已有 \(counts.skipped)，失败 \(counts.failed)")
                }
                let asset = assets.object(at: i)
                for resource in PHAssetResource.assetResources(for: asset) where [.photo, .video, .pairedVideo].contains(resource.type) {
                    let key = "\(target.key)|\(asset.localIdentifier)|\(resource.type.rawValue)"
                    if lock.withLock({ done.contains(key) }) { continue }
                    do {
                        try await upload(resource, key: key, album: target.album)
                    } catch {
                        counts.failed += 1
                        lastError = "\(resource.originalFilename)：\(error.localizedDescription)"
                    }
                }
            }
        }
    }

    private func headers(_ req: inout URLRequest, name: String, album: PhoneAlbum?) {
        req.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
        if let album {
            if album.hidden {
                req.setValue("hidden", forHTTPHeaderField: "X-Xiaoju-Album")
            } else {
                req.setValue(encodedName(album.title), forHTTPHeaderField: "X-Xiaoju-Album-Name")
            }
        }
    }

    // 先问服务端有没有；没有就导出原图交给后台上传（同时最多 6 个排队，不占满手机空间）。
    private func upload(_ resource: PHAssetResource, key: String, album: PhoneAlbum?) async throws {
        guard let base = URL(string: server) else { throw URLError(.badURL) }
        if let size = resource.value(forKey: "fileSize") as? Int64, size > 0 {
            var ask = URLRequest(url: base.appendingPathComponent("api/ingest/exists"))
            headers(&ask, name: resource.originalFilename, album: album)
            ask.setValue(String(size), forHTTPHeaderField: "X-File-Size")
            let (data, _) = try await URLSession.shared.data(for: ask)
            if (try? JSONDecoder().decode([String: String].self, from: data))?["result"] == "exists" {
                markDone(key)
                counts.skipped += 1
                return
            }
        }
        // 大文件（视频）：分段断点续传，断了下次从断的地方接着传。导出的文件留着，下次不用再导出
        let size = resource.value(forKey: "fileSize") as? Int64 ?? 0
        if size > 16 << 20 {
            let file = spool.appendingPathComponent(Self.sha(key))
            if !FileManager.default.fileExists(atPath: file.path) {
                let part = file.appendingPathExtension("part")
                try? FileManager.default.removeItem(at: part)
                let opts = PHAssetResourceRequestOptions()
                opts.isNetworkAccessAllowed = true
                try await PHAssetResourceManager.default().writeData(for: resource, toFile: part, options: opts)
                try FileManager.default.moveItem(at: part, to: file)
            }
            var extra: [String: String] = [:]
            if let album {
                if album.hidden { extra["X-Xiaoju-Album"] = "hidden" } else { extra["X-Xiaoju-Album-Name"] = encodedName(album.title) }
            }
            // 同时最多 2 个大文件在后台分段传
            while await chunkTasks() >= 2 {
                if Task.isCancelled { return }
                try await Task.sleep(nanoseconds: 2_000_000_000)
            }
            let job = BigJob(key: key, file: file.path, name: resource.originalFilename, headers: extra, size: size)
            if try await startBig(job) { counts.uploaded += 1 } else { counts.skipped += 1 }
            return
        }
        while await session.allTasks.count >= 6 {
            if Task.isCancelled { return }
            try await Task.sleep(nanoseconds: 1_000_000_000)
        }
        let file = spool.appendingPathComponent(UUID().uuidString)
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
        try await PHAssetResourceManager.default().writeData(for: resource, toFile: file, options: opts)
        var req = URLRequest(url: base.appendingPathComponent("api/ingest"))
        req.httpMethod = "POST"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        headers(&req, name: resource.originalFilename, album: album)
        let task = session.uploadTask(with: req, fromFile: file)
        task.taskDescription = key + "\t" + file.path
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
        if parts.count == 2 { try? FileManager.default.removeItem(atPath: parts[1]) }
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        if error == nil, (200..<300).contains(code), let key = parts.first {
            markDone(key)
            saveDone(force: false)
        } else {
            lastError = "后台上传失败：\(error?.localizedDescription ?? "HTTP \(code)")"
        }
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
            lastError = "\(job.name)：\(error.localizedDescription)（下次接着传）"
            setStatus("\(Self.time()) \(lastError)")
        }
    }

    private func finishBig(_ job: BigJob) {
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
            do { _ = try await startBig(job) } catch { lastError = "\(job.name)：\(error.localizedDescription)" }
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

    // MARK: 通讯录、日历、提醒事项

    private func backupData() async {
        let day = Self.day()
        if CNContactStore.authorizationStatus(for: .contacts) == .notDetermined {
            _ = try? await CNContactStore().requestAccess(for: .contacts)
        }
        if CNContactStore.authorizationStatus(for: .contacts) == .authorized, let vcf = contactsVCard() {
            await uploadData(vcf, name: "通讯录 \(day).vcf", folder: "通讯录")
        }
        let store = EKEventStore()
        if EKEventStore.authorizationStatus(for: .event) == .notDetermined {
            _ = try? await store.requestFullAccessToEvents()
        }
        if EKEventStore.authorizationStatus(for: .event) == .fullAccess {
            await uploadData(Data(calendarICS(store).utf8), name: "日历 \(day).ics", folder: "日历")
        }
        if EKEventStore.authorizationStatus(for: .reminder) == .notDetermined {
            _ = try? await store.requestFullAccessToReminders()
        }
        if EKEventStore.authorizationStatus(for: .reminder) == .fullAccess {
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
                lastError = "\(name)：HTTP \(code) \(String(decoding: body, as: UTF8.self))"
            }
        } catch {
            counts.failed += 1
            lastError = "\(name)：\(error.localizedDescription)"
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
