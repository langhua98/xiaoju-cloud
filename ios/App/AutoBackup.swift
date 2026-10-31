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
//   3. App 里点「开始备份」。
// App 开着时导出的原图由 App 自己的连接直接传（快）；切到后台时正在传的交给系统的后台 URLSession，App 被挂起、锁屏了也会传完；传过的记在本地，下次跳过，服务端也按名字 + 大小跳过。
// 大视频从断点到结尾整个交给一个后台任务（以前 8MB 一段、传完一段叫醒 App 再交下一段：App 在后台时系统叫醒得越来越慢，
// 后台建的任务还会被系统推迟，切出 App 后大视频就停在那里不动了）；断了下次问服务端收到多少，从那里接着传。
// 在 App 里点了备份（或快捷指令打开 App 备份）时，再向系统申请「继续在后台运行」（BGContinuedProcessingTask，iOS 26）：
// 切到别的 App、锁屏后这一轮照样往下导出、交上传，系统会显示一个进度；这时新文件由 App 自己直接传（direct）。
// 苹果不让第三方 App 读的数据（短信、备忘录、健康……）这里做不了。
final class AutoBackup: NSObject, ObservableObject, URLSessionDataDelegate {
    static let shared = AutoBackup()
    static let taskID = "com.langhua98.xiaoju.backup.sync"
    static let continuedID = "com.langhua98.xiaoju.backup.continued"

    @Published private(set) var running = false
    @Published private(set) var status = UserDefaults.standard.string(forKey: Keys.status) ?? ""
    @Published private(set) var transfers: [Transfer] = [] // 正在传的每个文件和进度
    @Published private(set) var failures: [Failure] = [] // 这一轮没传上去的，下次备份（或点「重试」）再传
    @Published private(set) var pausedUntil = UserDefaults.standard.object(forKey: Keys.pausedUntil) as? Date
    @Published private(set) var lastFinished = UserDefaults.standard.object(forKey: Keys.lastFinished) as? Date
    // 正在和云盘核对；checked：这次打开 App 后核对完过一遍。
    // 「备份」页一打开就用本地记下的（传过的、上次核对认出来的）先算三个数，核对时边核对边更新，不再等核对完：
    // checkedPhotos / checkPhotos 是核对到第几张（界面上显示进度），matched 每认出一批加一（界面上重算一次三个数）
    @Published private(set) var checking = false
    @Published private(set) var checked = false
    @Published private(set) var checkedPhotos = 0
    @Published private(set) var checkPhotos = 0
    @Published private(set) var matched = 0

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
        static let lastChecked = "auto.lastChecked"
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
    // 这一轮的统计。几张照片同时在准备、上传（见 backupPhotos），改的时候拿着 lock（用 count）
    private var tally = (uploaded: 0, skipped: 0, failed: 0)
    private var counts: (uploaded: Int, skipped: Int, failed: Int) { lock.withLock { tally } }
    private func count(_ change: (inout (uploaded: Int, skipped: Int, failed: Int)) -> Void) {
        lock.withLock { change(&tally) }
    }
    private var lastError = ""

    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.background(withIdentifier: "com.langhua98.xiaoju.backup.auto")
        config.isDiscretionary = false
        // 默认 60 秒没动静就算超时：几个大视频一起传时，小文件可能排队一两分钟才轮到，放宽到 5 分钟
        config.timeoutIntervalForRequest = 300
        config.httpMaximumConnectionsPerHost = 8 // 默认只有 4 个；服务端有多个 Telegram 机器人轮流存，可以多传几个
        config.sessionSendsLaunchEvents = true
        return URLSession(configuration: config, delegate: self, delegateQueue: nil)
    }()

    // App 自己的连接：App 在前台，或者「继续在后台运行」撑着 App 时用。系统的后台上传由系统排时间、限速，
    // 就算 App 开着也常常一个文件传几十秒、传一半停住；自己的连接快得多。
    // App 切到后台又没人撑着时，正在传的交给系统的后台上传接着传（见 handOff）。
    private var direct: URLSession!
    private var handingOff: Set<Int> = [] // 交给后台上传时取消掉的任务：它们的完成回调不算失败
    private var appActive = false
    private var continued: BGContinuedProcessingTask?
    // 「继续在后台运行」的进度：扫过的照片数 + 交出去、传出去的 MB 数
    private var scanTotal: Int64 = 0
    private var scanned: Int64 = 0
    private var queuedBytes: Int64 = 0
    private var movedBytes: Int64 = 0
    private var checkTotal: Int64 = 0 // 和云盘核对：要过一遍的照片数、已经过了几张
    private var checkedItems: Int64 = 0
    private var lastReport = Date.distantPast

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
        let config = URLSessionConfiguration.default
        config.httpMaximumConnectionsPerHost = 8
        config.timeoutIntervalForRequest = 300
        direct = URLSession(configuration: config, delegate: self, delegateQueue: nil)
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
        BGTaskScheduler.shared.register(forTaskWithIdentifier: Self.continuedID, using: nil) { task in
            self.attach(task as! BGContinuedProcessingTask)
        }
        let center = NotificationCenter.default
        center.addObserver(forName: UIApplication.didBecomeActiveNotification, object: nil, queue: nil) { _ in
            let again = self.lock.withLock { () -> Bool in
                self.appActive = true
                defer { self.resumeWhenActive = false }
                return self.resumeWhenActive
            }
            if again && self.enabled { self.restartWhenStopped() }
        }
        center.addObserver(forName: UIApplication.willResignActiveNotification, object: nil, queue: nil) { _ in
            self.lock.withLock { self.appActive = false }
        }
        center.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: nil) { _ in
            if self.lock.withLock({ self.continued == nil }) { self.handOff() }
        }
    }

    // 自己连接上正在传的交给系统的后台上传：照片用同一个请求重传；大文件问服务端还缺哪些，从那里接着传。
    // 切到后台后系统只给几秒，先申请一点时间做完。
    private func handOff() {
        let app = UIApplication.shared
        var grant = UIBackgroundTaskIdentifier.invalid
        grant = app.beginBackgroundTask(withName: "xiaoju.handoff") { app.endBackgroundTask(grant) }
        Task.detached { [self] in
            defer { DispatchQueue.main.async { app.endBackgroundTask(grant) } }
            var big: Set<String> = []
            for task in await direct.allTasks where task.state == .running || task.state == .suspended {
                let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
                lock.withLock {
                    handingOff.insert(task.taskIdentifier)
                    laneSent[task.taskIdentifier] = nil
                    if parts.count == 5, let start = Int64(parts[4]) { laneBusy[parts[1]]?.removeValue(forKey: start) }
                }
                task.cancel()
                if parts.first == "chunk", parts.count >= 4 {
                    big.insert(parts[1])
                    if parts[2] != loadJobs()[parts[1]]?.file { try? FileManager.default.removeItem(atPath: parts[2]) }
                } else if parts.count == 2, let req = task.originalRequest {
                    let again = session.uploadTask(with: req, fromFile: URL(fileURLWithPath: parts[1]))
                    again.taskDescription = task.taskDescription
                    again.resume()
                }
            }
            let jobs = loadJobs()
            for key in big {
                guard let job = jobs[key] else { continue }
                do {
                    _ = try await startBig(job)
                } catch {
                    fail(job.name, "\(error.localizedDescription)（下次接着传）")
                }
            }
        }
    }

    // 在 App 里点了备份：向系统申请「继续在后台运行」，切走、锁屏后这一轮接着跑（系统不给就和以前一样，切走后停下）。
    private func keepRunningInBackground() {
        let req = BGContinuedProcessingTaskRequest(identifier: Self.continuedID, title: "小桔备份", subtitle: "正在备份照片和视频")
        req.strategy = .fail // 现在跑不了就算了，不排队
        do {
            try BGTaskScheduler.shared.submit(req)
        } catch {
            setStatus("没能在后台继续：\(error.localizedDescription)（切走 App 后会停下）")
        }
    }

    private func attach(_ task: BGContinuedProcessingTask) {
        let running = lock.withLock { () -> Bool in
            guard job != nil else { return false }
            continued = task
            return true
        }
        guard running else {
            task.setTaskCompleted(success: true)
            return
        }
        task.expirationHandler = { [weak self] in
            guard let self else { return }
            // 系统收回了：在后台就停下这一轮，回到 App 时自动接着备份（以前要自己再点一次，看着像传失败就停了）
            let inBackground = lock.withLock { () -> Bool in
                if !self.appActive { self.resumeWhenActive = true }
                return !self.appActive
            }
            finishContinued(success: false)
            if inBackground {
                stop()
                handOff()
            }
        }
        reportContinued(force: true)
    }

    private var resumeWhenActive = false

    // 等上一轮停稳了再开始新的一轮
    private func restartWhenStopped() {
        Task.detached { [self] in
            for _ in 0..<60 where lock.withLock({ job != nil }) {
                try? await Task.sleep(nanoseconds: 500_000_000)
            }
            start(reason: "回到 App，接着备份")
        }
    }

    private func finishContinued(success: Bool) {
        let task: BGContinuedProcessingTask? = lock.withLock {
            defer { continued = nil }
            return continued
        }
        task?.setTaskCompleted(success: success)
    }

    // 系统显示的进度：照片扫到哪了、大文件传了多少。一直不动的话系统会收回，所以每秒报一次。
    private func reportContinued(force: Bool = false) {
        let snap: (BGContinuedProcessingTask, Int64, Int64)? = lock.withLock {
            guard let task = continued, force || Date().timeIntervalSince(lastReport) > 1 else { return nil }
            lastReport = Date()
            let total = max(checkTotal + scanTotal + (queuedBytes >> 20), 1)
            return (task, total, min(checkedItems + scanned + (movedBytes >> 20), total))
        }
        guard let snap else { return }
        snap.0.progress.totalUnitCount = snap.1
        snap.0.progress.completedUnitCount = snap.2
        snap.0.updateTitle("小桔备份", subtitle: "新传 \(counts.uploaded) 个，已有 \(counts.skipped) 个")
    }

    // 交上传用哪个连接：App 在前台、或者「继续在后台运行」撑着时自己传，其他时候交给系统的后台上传
    private func uploadSession() -> URLSession {
        lock.withLock { appActive || continued != nil } ? direct! : session
    }

    private func allTasks() async -> [URLSessionTask] {
        let a = await session.allTasks
        let b = await direct.allTasks
        return a + b
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
        start(reason: "后台任务", fromApp: false) { ok in task.setTaskCompleted(success: ok) }
    }

    func stop() { job?.cancel() }

    var isPaused: Bool { (pausedUntil ?? .distantPast) > Date() }

    // 暂停：停掉正在跑的这一轮，到时间之前后台任务、快捷指令都不跑（点「开始备份」会先恢复）。
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
    // 系统里已经是这个状态就不再设：以前每次打开「备份」页都再开一遍，已经开着时再开可能报错（截图里的
    // PHPhotosErrorDomain 3202），页面上就显示「系统后台上传没打开」，其实系统里是开着的。
    // 刚允许照片权限、刚装好 App 的那一下，系统也可能还没准备好：失败了隔 2 秒再试，最多试 4 次；
    // 报错但系统里已经是要的状态也算成功，都不行才显示原因。
    func setAutomatic(_ on: Bool) async -> String? {
        await MainActor.run { enabled = on }
        if on && PHPhotoLibrary.authorizationStatus(for: .readWrite) != .authorized { return nil }
        let library = PHPhotoLibrary.shared()
        var last: Error?
        for attempt in 0..<4 {
            if library.uploadJobExtensionEnabled == on { return nil }
            if attempt > 0 { try? await Task.sleep(nanoseconds: 2_000_000_000) }
            do {
                try library.setUploadJobExtensionEnabled(on)
                return nil
            } catch {
                last = error
            }
        }
        if library.uploadJobExtensionEnabled == on { return nil }
        let code = (last as NSError?).map { "\($0.domain) \($0.code)" } ?? ""
        return "系统后台上传没打开（\(code)）：\(last?.localizedDescription ?? "")（关掉「自动备份」再打开试试）"
    }

    // fromApp：在 App 里（或快捷指令打开 App）开始的，申请「继续在后台运行」；系统的后台任务不用。
    func start(reason: String, fromApp: Bool = true, completion: ((Bool) -> Void)? = nil) {
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
        tally = (0, 0, 0) // 还拿着 lock
        lastError = ""
        failed = [] // 这一轮会把上次没传上去的再传一遍
        (scanTotal, scanned, queuedBytes, movedBytes, checkTotal, checkedItems) = (0, 0, 0, 0, 0, 0)
        job = Task.detached(priority: .utility) { [self] in
            await run(reason: reason)
            lock.withLock { job = nil }
            finishContinued(success: !Task.isCancelled)
            completion?(!Task.isCancelled)
        }
        lock.unlock()
        publishFailures()
        if fromApp { keepRunningInBackground() }
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
        lock.withLock {
            lastError = "\(name)：\(reason)"
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
        // 先把所有要备份的相册都和云盘核对完，再开始传：「已备份」一开始就是准的，传的时候也不会把云盘里有的再传一遍
        await reconcileAll()
        if Task.isCancelled { return }
        // 还在传的（上一轮交出去的大视频、照片）：这一轮不再交一次。再交一次会让服务端停掉正在传的那个请求，
        // 手机上就报「The network connection was lost」
        let busy = await busyKeys()
        let targets = photoTargets()
        lock.withLock { scanTotal = Int64(targets.reduce(0) { $0 + $1.assets.count }) }
        // 同时准备几张（问服务端有没有、从相册导出、可能还要先从 iCloud 下载原图），以前是一张张来：
        // 导出、下载慢的时候，后台上传那边经常是空着的
        await withTaskGroup(of: Void.self) { group in
            var preparing = 0
            for target in targets {
                let assets = target.assets
                for i in 0..<assets.count {
                    if Task.isCancelled { return }
                    lock.withLock { scanned += 1 }
                    reportContinued()
                    if i % 20 == 0 {
                        setStatus("正在备份「\(target.label)」\(i)/\(assets.count)：新传 \(counts.uploaded)，已有 \(counts.skipped)，失败 \(counts.failed)")
                    }
                    let asset = assets.object(at: i)
                    let known = lock.withLock { inCloud.contains("\(target.key)|\(asset.localIdentifier)") }
                    for resource in PHAssetResource.assetResources(for: asset) where [.photo, .video, .pairedVideo].contains(resource.type) {
                        let key = "\(target.key)|\(asset.localIdentifier)|\(resource.type.rawValue)"
                        if busy.contains(key) || lock.withLock({ done.contains(key) }) { continue }
                        if known && resource.type != .pairedVideo { continue } // 快捷指令、网页传过这张：不重传（实况照片的视频照常问）
                        if preparing >= Self.preparing {
                            await group.next()
                            preparing -= 1
                        }
                        preparing += 1
                        group.addTask { [self] in
                            do {
                                try await upload(resource, key: key, album: target.album)
                            } catch {
                                count { $0.failed += 1 }
                                fail(resource.originalFilename, error.localizedDescription)
                            }
                        }
                    }
                }
            }
        }
    }

    // 同时准备几张照片
    private static let preparing = 4

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

    // 和云盘核对：先按“名字|大小”（matchNames），再按名字 + 拍摄日期（planMatch）。
    // 每张照片的 id、拍摄日期和要传的资源一万张取一遍要几十秒，两种比对共用一份（以前各取一遍）
    private typealias Item = (id: String, date: Date?, resources: [PHAssetResource])

    func reconcile(target: String, assets: PHFetchResult<PHAsset>, album: PhoneAlbum?) async {
        // 云盘里的“名字|大小”摘要和下面取一遍照片同时进行（以前取完一万张才去问服务端）
        let names = Task { await cloudNames(album: album) }
        var items: [Item] = []
        items.reserveCapacity(assets.count)
        // 核对也算进「继续在后台运行」的进度：一万张取一遍要几十秒，以前这段时间进度不动，锁屏后容易被系统收回
        let backingUp = lock.withLock { () -> Bool in
            guard job != nil else { return false }
            checkTotal += Int64(assets.count)
            return true
        }
        let start = await MainActor.run { checkedPhotos }
        assets.enumerateObjects { asset, i, _ in
            let resources = PHAssetResource.assetResources(for: asset).filter { [.photo, .video, .pairedVideo].contains($0.type) }
            items.append((asset.localIdentifier, asset.creationDate, resources))
            if backingUp {
                self.lock.withLock { self.checkedItems += 1 }
                if i % 50 == 0 { self.reportContinued() }
            }
            if i % 200 == 199 { Task { @MainActor in self.checkedPhotos = max(self.checkedPhotos, start + i + 1) } }
        }
        await MainActor.run { checkedPhotos = start + assets.count }
        if let known = await names.value { matchNames(target: target, items: items, known: known) }
        await planMatch(target: target, items: items, album: album)
    }

    // 云盘里已经有的（快捷指令、网页、之前装的 App 传过的）：取一次这个相册里每个文件“名字|大小”的摘要
    // （GET /api/ingest/names），在手机上比对（matchNames），对上的记成传过。这样「已备份」一打开就是对的，备份时也不用每张都去问。
    private func cloudNames(album: PhoneAlbum?) async -> Set<String>? {
        guard let url = URL(string: server + "/api/ingest/names") else { return nil }
        var req = URLRequest(url: url)
        if let album {
            if album.hidden {
                req.setValue("hidden", forHTTPHeaderField: "X-Xiaoju-Album")
            } else {
                req.setValue(encodedName(album.title), forHTTPHeaderField: "X-Xiaoju-Album-Name")
            }
        }
        guard let reply = try? await URLSession.shared.data(for: req),
              (reply.1 as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        let known = Set(String(decoding: reply.0, as: UTF8.self).split(separator: "\n").map(String.init))
        return known.isEmpty ? nil : known
    }

    private func matchNames(target: String, items: [Item], known: Set<String>) {
        var found = 0
        for item in items {
            for resource in item.resources {
                let key = "\(target)|\(item.id)|\(resource.type.rawValue)"
                guard let size = resource.value(forKey: "fileSize") as? Int64, size > 0,
                      known.contains(Self.sha("\(resource.originalFilename)|\(size)")),
                      !lock.withLock({ done.contains(key) }) else { continue }
                markDone(key)
                found += 1
            }
        }
        if found > 0 {
            saveDone(force: true)
            Task { @MainActor in matched += 1 }
        }
    }

    // 快捷指令、网页传的照片常常和手机上的原件大小不一样（导出时重新编码），按“名字|大小”认不出来，
    // 「已备份」就只剩 App 自己传的几十张。所以再把每张的名字和拍摄日期一次发给服务端（POST /api/ingest/plan，
    // 和快捷指令同一套比法：名字不分大小写、不比扩展名，拍摄日期差一天以内），认出来的记进 inCloud：
    // 算进「已备份」，备份时不再重传；但「释放手机空间」只认按原件核对过的（done），不会因为这个删照片。
    // 服务端只认顶层的「相册」「隐藏相册」，勾选的相册（相册/名字）还是只按“名字|大小”认。
    private func planMatch(target: String, items: [Item], album: PhoneAlbum?) async {
        guard album?.hidden ?? true, let url = URL(string: server + "/api/ingest/plan") else { return }
        let day = DateFormatter()
        day.locale = Locale(identifier: "en_US_POSIX")
        day.dateFormat = "yyyy-MM-dd"
        var ids: [String] = [], names: [String] = [], dates: [String] = []
        for item in items {
            guard let main = item.resources.first(where: { $0.type == .photo || $0.type == .video }) else { continue }
            ids.append(item.id)
            names.append(main.originalFilename.replacingOccurrences(of: "\n", with: " "))
            dates.append(item.date.map(day.string(from:)) ?? "")
        }
        guard !ids.isEmpty else { return }
        var req = URLRequest(url: url, timeoutInterval: 180) // 一万张的清单服务端要比一会儿
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
        await MainActor.run { matched += 1 }
    }

    // 把每个要备份的相册都和云盘核对一遍：「备份」页打开时、每一轮备份开始传之前。
    // 同一时间只跑一遍；已经在核对时就等它核对完。
    func reconcileAll() async {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else { return }
        let task: Task<Void, Never> = lock.withLock {
            if let running = reconciling { return running }
            let task = Task.detached(priority: .userInitiated) { [self] in
                let targets = photoTargets()
                let total = targets.reduce(0) { $0 + $1.assets.count }
                await MainActor.run {
                    checkedPhotos = 0
                    checkPhotos = total
                    checking = true
                }
                for target in targets {
                    await reconcile(target: target.key, assets: target.assets, album: target.album)
                }
                lock.withLock { reconciling = nil }
                defaults.set(Date(), forKey: Keys.lastChecked)
                await MainActor.run {
                    checking = false
                    checked = true
                }
            }
            reconciling = task
            return task
        }
        await task.value
    }

    // 「备份」页打开时：几分钟内刚核对过就不再核对（三个数直接用本地记下的），下拉刷新、开始备份时照常核对
    func reconcileIfStale() async {
        if let last = defaults.object(forKey: Keys.lastChecked) as? Date, Date().timeIntervalSince(last) < 10 * 60 {
            await MainActor.run { checked = true }
            return
        }
        await reconcileAll()
    }

    private var reconciling: Task<Void, Never>?

    // 正在传的文件（照片、大视频）的 key
    private func busyKeys() async -> Set<String> {
        Set(await allTasks().compactMap { task -> String? in
            let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
            if parts.first == "chunk" { return parts.count > 1 ? parts[1] : nil }
            return parts.first.flatMap { $0.isEmpty ? nil : $0 }
        })
    }

    // 放进哪个相册的头部：隐藏相册 X-Xiaoju-Album: hidden，其他相册 X-Xiaoju-Album-Name: 相册名
    private func albumHeaders(_ album: PhoneAlbum?) -> [String: String] {
        guard let album else { return [:] }
        return album.hidden ? ["X-Xiaoju-Album": "hidden"] : ["X-Xiaoju-Album-Name": encodedName(album.title)]
    }

    private func upload(_ resource: PHAssetResource, key: String, album: PhoneAlbum?) async throws {
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
        let size = resource.value(forKey: "fileSize") as? Int64 ?? 0
        // 从 iCloud 下载原图、大视频可能要好几分钟：下载进度也报给「继续在后台运行」。
        // 系统资源紧张时先收回进度不动的任务，以前这段时间进度一直不动，锁屏后容易被收回。
        final class Fetched { var bytes: Int64 = -1 } // -1：还没算进总数
        let fetched = Fetched()
        opts.progressHandler = { [weak self] fraction in
            guard let self, size > 0 else { return }
            let now = Int64(Double(size) * fraction)
            self.lock.withLock {
                if fetched.bytes < 0 {
                    self.queuedBytes += size
                    fetched.bytes = 0
                }
                let moved = max(now - fetched.bytes, 0)
                self.movedBytes += moved
                NetMeter.shared.received(moved) // 从 iCloud 下载原图
                fetched.bytes = max(now, fetched.bytes)
            }
            self.reportContinued()
        }
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
                count { $0.skipped += 1 }
                return
            }
        }
        // 大文件（视频）：分段断点续传，断了下次从断的地方接着传。导出的文件留着，下次不用再导出
        if size > 16 << 20 {
            // 同时最多 3 个大文件在后台分段传。先等再导出：几张同时在准备，不先等的话会同时导出好几个大视频，占满手机空间
            while await chunkTasks() >= 3 {
                if Task.isCancelled { return }
                reportContinued()
                try await Task.sleep(nanoseconds: 2_000_000_000)
            }
            let file = spool.appendingPathComponent(Self.sha(key))
            if !FileManager.default.fileExists(atPath: file.path) {
                let part = file.appendingPathExtension("part")
                try? FileManager.default.removeItem(at: part)
                try await export(part)
                try FileManager.default.moveItem(at: part, to: file)
            }
            let job = BigJob(key: key, file: file.path, name: name, headers: extra, size: size)
            if try await startBig(job) { count { $0.uploaded += 1 } } else { count { $0.skipped += 1 } }
            return
        }
        while await allTasks().count >= 10 {
            if Task.isCancelled { return }
            reportContinued()
            try await Task.sleep(nanoseconds: 1_000_000_000)
        }
        let file = spool.appendingPathComponent(UUID().uuidString)
        try await export(file)
        var req = URLRequest(url: base.appendingPathComponent("api/ingest"))
        req.httpMethod = "POST"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        req.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
        for (k, v) in extra { req.setValue(v, forHTTPHeaderField: k) }
        let task = uploadSession().uploadTask(with: req, fromFile: file)
        task.taskDescription = key + "\t" + file.path
        lock.withLock { lastNames[key] = name }
        track(key, name: name, sent: 0, total: size)
        task.resume()
        count { $0.uploaded += 1 }
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.withLock { replies[dataTask.taskIdentifier, default: Data()].append(data) }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        let reply = lock.withLock { replies.removeValue(forKey: task.taskIdentifier) } ?? Data()
        if session === direct, lock.withLock({ handingOff.remove(task.taskIdentifier) != nil }) {
            return // 已经交给后台上传接着传了
        }
        if task.taskDescription?.hasPrefix("chunk\t") == true {
            chunkDone(task, reply: reply, error: error)
            return
        }
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        // 服务端先回了 200（收文件时一直发心跳），存的时候才出错：回复里是 {"result":"error"}，不能当成传好了
        let result = ((try? JSONSerialization.jsonObject(with: reply)) as? [String: Any])?["result"] as? String
        let ok = error == nil && (200..<300).contains(code) && result != "error"
        if !ok && retrySmall(task, parts: parts, code: code) { return }
        untrack(parts[0])
        defer { lock.withLock { lastNames[parts[0]] = nil } }
        if parts.count == 2 { try? FileManager.default.removeItem(atPath: parts[1]) }
        lock.withLock { retries[parts[0]] = nil }
        if ok, let key = parts.first {
            markDone(key)
            saveDone(force: false)
        } else {
            let name = lock.withLock { lastNames[parts[0]] } ?? "照片"
            fail(name, error?.localizedDescription ?? (result == "error" ? "服务端保存失败（下次再传）" : "服务器返回 \(code)"))
        }
    }

    // 照片、小视频没传上去（超时、网断了一下、服务端一时出错）：导出的文件还在，过 5、10、15 秒用同一个请求再传，
    // 连着 3 次都不行才算失败。以前一次超时就进了「没传上去的」，要等下一轮或手动重试。
    private func retrySmall(_ task: URLSessionTask, parts: [String], code: Int) -> Bool {
        let transient: Set<URLError.Code> = [.timedOut, .networkConnectionLost, .notConnectedToInternet,
                                             .cannotConnectToHost, .cannotFindHost, .dnsLookupFailed, .secureConnectionFailed]
        let retryable = (task.error as? URLError).map { transient.contains($0.code) } ?? (code >= 500 || code == 429)
        guard retryable, parts.count == 2, let req = task.originalRequest,
              FileManager.default.fileExists(atPath: parts[1]) else { return false }
        let key = parts[0]
        let tries = lock.withLock { () -> Int in
            retries[key, default: 0] += 1
            return retries[key]!
        }
        guard tries <= 3 else { return false }
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + .seconds(tries * 5)) { [self] in
            let again = uploadSession().uploadTask(with: req, fromFile: URL(fileURLWithPath: parts[1]))
            again.taskDescription = task.taskDescription
            again.resume()
        }
        return true
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didSendBodyData bytesSent: Int64,
                    totalBytesSent: Int64, totalBytesExpectedToSend: Int64)
    {
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        if parts.first == "chunk", parts.count == 5 {
            // 分段同时传：服务端已经收到的 + 每条连接这一段已发出的
            let sent = lock.withLock { () -> Int64 in
                laneSent[task.taskIdentifier] = (parts[1], totalBytesSent)
                return (laneBase[parts[1]] ?? 0) + laneSent.values.filter { $0.key == parts[1] }.reduce(0) { $0 + $1.sent }
            }
            if lock.withLock({ meta[parts[1]] }) == nil, let job = loadJobs()[parts[1]] {
                lock.withLock { meta[job.key] = (job.name, job.size) }
            }
            updateProgress(parts[1], sent: sent)
        } else if parts.first == "chunk", parts.count == 4, let end = Int64(parts[3]) {
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
            if meta[key] == nil { queuedBytes += max(total - sent, 0) }
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
        lock.withLock {
            let moved = max(sent - (sentBytes[key] ?? sent), 0)
            movedBytes += moved
            NetMeter.shared.sent(moved)
            sentBytes[key] = sent
        }
        publishProgress(force: false)
        reportContinued()
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

    // MARK: 大文件：从断点到结尾整个交给一个上传任务（锁屏、App 被挂起也由系统传完，不用叫醒 App 交下一段）

    struct BigJob: Codable {
        var key: String
        var file: String // 导出的原文件，传完才删
        var name: String
        var headers: [String: String]
        var size: Int64
        var id = ""
        var chunk: Int64 = 8 << 20
        // 服务端支持分段同时传时：建议开几条连接、分块多大（一段不跨过分块边界）。旧版本存的任务里没有
        var lanes: Int?
        var part: Int64?
    }

    // 分段同时传：每段多大。一段一个后台任务，太小任务太多，太大断了要重传的多、占手机空间多
    private static let laneSegment: Int64 = 32 << 20
    // 正在传的段：key → [起点: 终点]（挑下一段时避开）；laneSeeded：App 重启后从系统的任务列表里补过了
    private var laneBusy: [String: [Int64: Int64]] = [:]
    private var laneSeeded: Set<String> = []
    // 进度：key → 服务端已经收到的字节数；任务 ID → (key, 这一段已发出的字节数)
    private var laneBase: [String: Int64] = [:]
    private var laneSent: [Int: (key: String, sent: Int64)] = [:]

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

    // 在后台传的大文件有几个（分段同时传时一个文件有好几个任务）
    private func chunkTasks() async -> Int {
        Set(await allTasks().compactMap { task -> String? in
            let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
            return parts.first == "chunk" && parts.count > 1 ? parts[1] : nil
        }).count
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
        job.lanes = (out["lanes"] as? NSNumber)?.intValue
        job.part = (out["part"] as? NSNumber)?.int64Value
        saveJob(job, key: job.key)
        if (job.lanes ?? 1) > 1, let missing = Self.ranges(out["missing"]) {
            await queueLanes(job, missing: missing)
        } else {
            try queueChunk(job, offset: (out["offset"] as? NSNumber)?.int64Value ?? 0)
        }
        return true
    }

    // 服务端回复里的 missing：[[起点, 终点], …]
    private static func ranges(_ v: Any?) -> [(Int64, Int64)]? {
        guard let list = v as? [[NSNumber]] else { return nil }
        return list.compactMap { $0.count == 2 ? ($0[0].int64Value, $0[1].int64Value) : nil }
    }

    // 分段同时传：从还缺的地方（missing）挑没人在传的段，补到 lanes 条连接。一段一个后台任务，
    // 从导出的原文件里把这一段复制出来传（PUT …?offset=起点&parallel=1）。
    private func queueLanes(_ job: BigJob, missing: [(Int64, Int64)]) async {
        if !lock.withLock({ laneSeeded.contains(job.key) }) {
            // App 重启过：系统还在传的段先记上，免得同一段交两次（新的会打断旧的）
            let running = await allTasks().compactMap { task -> (Int64, Int64)? in
                let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
                guard parts.count == 5, parts[0] == "chunk", parts[1] == job.key,
                      let start = Int64(parts[4]), let end = Int64(parts[3]) else { return nil }
                return (start, end)
            }
            lock.withLock {
                for (a, b) in running { laneBusy[job.key, default: [:]][a] = b }
                laneSeeded.insert(job.key)
            }
        }
        let lanes = max(job.lanes ?? 1, 1)
        let part = job.part ?? 0
        let picked: [(Int64, Int64)] = lock.withLock {
            laneBase[job.key] = job.size - missing.reduce(0) { $0 + $1.1 - $1.0 }
            var busy = laneBusy[job.key] ?? [:]
            var out: [(Int64, Int64)] = []
            for (a, b) in missing {
                var x = a
                while x < b, busy.count < lanes {
                    var end = min(b, x + Self.laneSegment)
                    if part > 0 { end = min(end, (x / part + 1) * part) }
                    if !busy.contains(where: { $0.key < end && x < $0.value }) {
                        busy[x] = end
                        out.append((x, end))
                    }
                    x = end
                }
            }
            laneBusy[job.key] = busy
            return out
        }
        track(job.key, name: job.name, sent: lock.withLock { laneBase[job.key] ?? 0 }, total: job.size)
        for (a, b) in picked {
            do {
                let body = spool.appendingPathComponent("\(job.id)-\(a)-\(b)")
                try Self.copyRange(of: URL(fileURLWithPath: job.file), from: a, to: b, dest: body)
                var req = URLRequest(url: URL(string: server + "/api/ingest/uploads/\(job.id)?offset=\(a)&parallel=1")!)
                req.httpMethod = "PUT"
                req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
                let task = uploadSession().uploadTask(with: req, fromFile: body)
                task.taskDescription = ["chunk", job.key, body.path, String(b), String(a)].joined(separator: "\t")
                task.resume()
            } catch {
                _ = lock.withLock { laneBusy[job.key]?.removeValue(forKey: a) }
                fail(job.name, "\(error.localizedDescription)（下次接着传）")
            }
        }
        if !picked.isEmpty {
            let count = lock.withLock { laneBusy[job.key]?.count ?? 0 }
            setStatus("后台在传 \(job.name)：\(count) 条连接，已收到 \((lock.withLock { laneBase[job.key] ?? 0 }) >> 20)/\(job.size >> 20) MB")
        }
    }

    // 把 [from, to) 复制到 dest（一块块复制，不把整个视频读进内存）
    private static func copyRange(of src: URL, from: Int64, to: Int64, dest: URL) throws {
        let input = try FileHandle(forReadingFrom: src)
        defer { try? input.close() }
        try input.seek(toOffset: UInt64(from))
        try? FileManager.default.removeItem(at: dest)
        FileManager.default.createFile(atPath: dest.path, contents: nil)
        let output = try FileHandle(forWritingTo: dest)
        defer { try? output.close() }
        var left = to - from
        while left > 0 {
            let block = try autoreleasepool { try input.read(upToCount: Int(min(left, 8 << 20))) ?? Data() }
            if block.isEmpty { break }
            try output.write(contentsOf: block)
            left -= Int64(block.count)
        }
    }

    // 从 offset 到文件结尾一个请求传完（服务端一直收到结尾）。offset 是 0 时直接传导出的文件，否则把后半截复制出来。
    private func queueChunk(_ job: BigJob, offset: Int64) throws {
        guard FileManager.default.fileExists(atPath: job.file) else { throw URLError(.fileDoesNotExist) }
        var body = URL(fileURLWithPath: job.file)
        if offset > 0 {
            body = spool.appendingPathComponent("\(job.id)-\(offset)")
            try Self.copyTail(of: URL(fileURLWithPath: job.file), from: offset, to: body)
        }
        var req = URLRequest(url: URL(string: server + "/api/ingest/uploads/\(job.id)?offset=\(offset)")!)
        req.httpMethod = "PUT"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        let task = uploadSession().uploadTask(with: req, fromFile: body)
        task.taskDescription = ["chunk", job.key, body.path, String(job.size)].joined(separator: "\t")
        track(job.key, name: job.name, sent: offset, total: job.size)
        task.resume()
        setStatus("后台在传 \(job.name)：从 \(offset >> 20)/\(job.size >> 20) MB 接着传")
    }

    // 一块块复制，不把整个视频读进内存
    private static func copyTail(of src: URL, from offset: Int64, to dest: URL) throws {
        let input = try FileHandle(forReadingFrom: src)
        defer { try? input.close() }
        try input.seek(toOffset: UInt64(offset))
        try? FileManager.default.removeItem(at: dest)
        FileManager.default.createFile(atPath: dest.path, contents: nil)
        let output = try FileHandle(forWritingTo: dest)
        defer { try? output.close() }
        while true {
            let block = try autoreleasepool { try input.read(upToCount: 8 << 20) ?? Data() }
            if block.isEmpty { break }
            try output.write(contentsOf: block)
        }
    }

    private func chunkDone(_ task: URLSessionTask, reply: Data, error: Error?) {
        let parts = (task.taskDescription ?? "").components(separatedBy: "\t")
        let lane = parts.count == 5 ? Int64(parts[4]) : nil // 分段同时传的一段：它的起点
        lock.withLock {
            laneSent[task.taskIdentifier] = nil
            if let lane, parts.count > 1 { laneBusy[parts[1]]?.removeValue(forKey: lane) }
        }
        guard parts.count >= 4, let job = loadJobs()[parts[1]] else { return }
        if parts[2] != job.file { try? FileManager.default.removeItem(atPath: parts[2]) } // 导出的原文件传完才删
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        let out = (try? JSONSerialization.jsonObject(with: reply)) as? [String: Any] ?? [:]
        let next = (out["offset"] as? NSNumber)?.int64Value ?? Int64(parts[3]) ?? 0
        do {
            switch code {
            case 201:
                finishBig(job)
            case 200 where out["same_content"] as? Bool == true:
                finishBig(job)
            case 200 where lane != nil:
                lock.withLock { retries[job.key] = nil }
                let missing = Self.ranges(out["missing"]) ?? []
                Task.detached { [self] in await queueLanes(job, missing: missing) }
            case 409 where lane != nil: // 这段别的连接已经传过了：问服务端还缺哪些
                Task.detached { [self] in
                    guard let url = URL(string: server + "/api/ingest/uploads/\(job.id)"),
                          let reply = try? await URLSession.shared.data(from: url),
                          let status = (try? JSONSerialization.jsonObject(with: reply.0)) as? [String: Any],
                          let missing = Self.ranges(status["missing"]) else { return }
                    await queueLanes(job, missing: missing)
                }
            case 200, 409:
                try queueChunk(job, offset: next)
            default:
                throw NSError(domain: "xiaoju", code: code, userInfo: [NSLocalizedDescriptionKey:
                    error?.localizedDescription ?? "HTTP \(code) \(String(decoding: reply, as: UTF8.self))"])
            }
        } catch {
            // 断了（网络断了、服务端说找不到这个上传）：过几秒自己问服务端收到多少，接着传；连着断 5 次才算失败
            untrack(job.key)
            let tries = lock.withLock { () -> Int in
                retries[job.key, default: 0] += 1
                return retries[job.key]!
            }
            if tries <= 5 {
                setStatus("\(Self.time()) \(job.name) 断了，\(tries * 5) 秒后接着传")
                Task.detached { [self] in
                    try? await Task.sleep(nanoseconds: UInt64(tries) * 5_000_000_000)
                    // 这期间新的一轮已经接着传了：不再交一次（会打断它）
                    if await busyKeys().contains(job.key) { return }
                    do {
                        _ = try await startBig(job)
                    } catch {
                        fail(job.name, "\(error.localizedDescription)（下次接着传）")
                    }
                }
                return
            }
            // 留着这个任务，下次备份时问服务端收到多少接着传
            fail(job.name, "\(error.localizedDescription)（下次接着传）")
            setStatus("\(Self.time()) \(lastError)")
        }
    }

    private var retries: [String: Int] = [:] // 大文件断了自己接着传的次数

    private func finishBig(_ job: BigJob) {
        lock.withLock {
            retries[job.key] = nil
            laneBusy[job.key] = nil
            laneBase[job.key] = nil
        }
        untrack(job.key)
        try? FileManager.default.removeItem(atPath: job.file)
        saveJob(nil, key: job.key)
        markDone(job.key)
        saveDone(force: true)
        setStatus("\(Self.time()) 后台传完 \(job.name)")
    }

    // 上次没传完的大文件（断网、App 被杀）：接着传。
    private func resumeBigJobs() async {
        let active = Set(await allTasks().compactMap { $0.taskDescription?.components(separatedBy: "\t") }
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
                    count { $0.failed += 1 }
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
                count { $0.uploaded += 1 }
            } else if code == 200 {
                count { $0.skipped += 1 }
            } else {
                count { $0.failed += 1 }
                fail(name, "服务器返回 \(code) \(String(decoding: body, as: UTF8.self))")
            }
        } catch {
            count { $0.failed += 1 }
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
