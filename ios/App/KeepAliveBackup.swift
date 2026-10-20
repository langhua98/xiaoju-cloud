import CoreLocation
import os
import Photos
import UIKit

// 保活备份：开着「iCloud 照片」时系统不调用第三方的后台上传扩展（苹果的 bug，见
// https://developer.apple.com/forums/thread/822256），所以 App 自己在后台待着传。
// 靠后台定位让系统不把 App 挂起（精度要求最低，只为保活），相册一有新照片就导出原图、交给系统的后台 URLSession 上传：
// 交出去之后就算 App 被挂起、锁屏，系统也会把文件传完。App 被系统杀掉后，「显著位置变化」会把它重新叫起来。
// 只传开关打开之后新加的（第一次打开时补传最近 24 小时的），放进云盘的「相册」，传过的服务端自动跳过。
final class KeepAliveBackup: NSObject, ObservableObject, CLLocationManagerDelegate, PHPhotoLibraryChangeObserver,
    URLSessionTaskDelegate, URLSessionDataDelegate
{
    static let shared = KeepAliveBackup()

    @Published private(set) var enabled = UserDefaults.standard.bool(forKey: Keys.enabled)
    @Published private(set) var status = UserDefaults.standard.string(forKey: Keys.status) ?? ""
    @Published private(set) var locationAuth = CLAuthorizationStatus.notDetermined

    var backgroundCompletion: (() -> Void)?

    private enum Keys {
        static let enabled = "keepAlive.enabled"
        static let token = "keepAlive.changeToken"
        static let pending = "keepAlive.pending"
        static let status = "keepAlive.status"
        static let uploaded = "keepAlive.uploaded"
    }

    private let log = Logger(subsystem: "com.langhua98.xiaoju.backup", category: "keepalive")
    private let defaults = UserDefaults.standard
    private let queue = DispatchQueue(label: "keepalive.scan") // 扫描、导出都在这条串行队列上
    private let location = CLLocationManager()
    private var scanning = false
    private var observing = false
    private var lastScan = Date.distantPast
    private var lastHeartbeat = Date.distantPast
    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.background(withIdentifier: "com.langhua98.xiaoju.backup.keepalive")
        config.isDiscretionary = false
        config.sessionSendsLaunchEvents = true
        config.allowsCellularAccess = true
        config.allowsExpensiveNetworkAccess = true
        config.allowsConstrainedNetworkAccess = true
        return URLSession(configuration: config, delegate: self, delegateQueue: nil)
    }()

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }
    private var spool: URL {
        let dir = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("upload")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }

    override private init() {
        super.init()
        location.delegate = self
        location.desiredAccuracy = kCLLocationAccuracyThreeKilometers // 只为保活，不要精确位置，省电
        location.distanceFilter = kCLDistanceFilterNone
        location.pausesLocationUpdatesAutomatically = false
        location.showsBackgroundLocationIndicator = false
        locationAuth = location.authorizationStatus
    }

    // App 启动（包括被系统在后台叫起来）时调用。
    func resume() {
        _ = session // 接上之前交给系统的上传
        if enabled { startRunning() }
    }

    func setEnabled(_ on: Bool) {
        enabled = on
        defaults.set(on, forKey: Keys.enabled)
        if on {
            if location.authorizationStatus == .notDetermined {
                location.requestWhenInUseAuthorization() // 先要「使用 App 期间」，回调里再要「始终」
            } else if location.authorizationStatus == .authorizedWhenInUse {
                location.requestAlwaysAuthorization()
            }
            startRunning()
        } else {
            location.stopUpdatingLocation()
            location.stopMonitoringSignificantLocationChanges()
            PHPhotoLibrary.shared().unregisterChangeObserver(self)
            observing = false
            setStatus("已关闭")
        }
    }

    private func startRunning() {
        if location.authorizationStatus == .authorizedAlways || location.authorizationStatus == .authorizedWhenInUse {
            location.allowsBackgroundLocationUpdates = true
            location.startUpdatingLocation()
            location.startMonitoringSignificantLocationChanges()
        }
        if !observing {
            PHPhotoLibrary.shared().register(self)
            observing = true
        }
        scan(reason: "start")
    }

    // MARK: 叫醒

    func locationManagerDidChangeAuthorization(_ manager: CLLocationManager) {
        DispatchQueue.main.async { self.locationAuth = manager.authorizationStatus }
        guard enabled else { return }
        if manager.authorizationStatus == .authorizedWhenInUse {
            manager.requestAlwaysAuthorization()
        }
        if manager.authorizationStatus == .authorizedAlways || manager.authorizationStatus == .authorizedWhenInUse {
            startRunning()
        }
    }

    func locationManager(_ manager: CLLocationManager, didUpdateLocations locations: [CLLocation]) {
        scan(reason: "location") // scan 自己会合并、限流
    }

    func locationManager(_ manager: CLLocationManager, didFailWithError error: Error) {
        log.error("location: \(error.localizedDescription, privacy: .public)")
    }

    func photoLibraryDidChange(_ changeInstance: PHChange) {
        queue.asyncAfter(deadline: .now() + 3) { self.scan(reason: "photos") } // 连拍时等一下再一起处理
    }

    // MARK: 扫描新照片，交给后台上传

    func scan(reason: String) {
        queue.async { [self] in
            guard enabled, !scanning, PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else { return }
            // 定位回调很频繁：只是保活，顺便每分钟最多查一次（新照片靠相册变化的通知）
            if reason == "location", Date().timeIntervalSince(lastScan) < 60 { return }
            lastScan = Date()
            scanning = true
            defer { scanning = false }
            // 导出原图要点时间：向系统多要一点后台时间
            var bg = UIBackgroundTaskIdentifier.invalid
            DispatchQueue.main.sync { bg = UIApplication.shared.beginBackgroundTask(withName: "keepalive.scan") }
            defer { DispatchQueue.main.async { UIApplication.shared.endBackgroundTask(bg) } }
            do {
                try collectNewAssets()
                let queued = try queueUploads()
                let pending = defaults.stringArray(forKey: Keys.pending)?.count ?? 0
                if queued > 0 || reason != "location" {
                    setStatus("\(Self.time()) 检查新照片：交给系统上传 \(queued) 个，待处理 \(pending) 个")
                }
                heartbeat("reason=\(reason) queued=\(queued) pending=\(pending)")
            } catch {
                log.error("scan failed: \(error.localizedDescription, privacy: .public)")
                setStatus("\(Self.time()) 出错：\(error.localizedDescription)")
                defaults.removeObject(forKey: Keys.token) // 令牌过期之类：下次从头开始（补最近 24 小时）
                heartbeat("reason=\(reason) error=\(error)")
            }
        }
    }

    private func collectNewAssets() throws {
        let library = PHPhotoLibrary.shared()
        guard let data = defaults.data(forKey: Keys.token),
              let token = try NSKeyedUnarchiver.unarchivedObject(ofClass: PHPersistentChangeToken.self, from: data)
        else {
            // 第一次：最近 24 小时拍的也补上
            try save(token: library.currentChangeToken)
            let options = PHFetchOptions()
            options.predicate = NSPredicate(format: "creationDate > %@", Date().addingTimeInterval(-24 * 3600) as NSDate)
            var recent: [String] = []
            PHAsset.fetchAssets(with: options).enumerateObjects { asset, _, _ in recent.append(asset.localIdentifier) }
            defaults.set(recent, forKey: Keys.pending)
            return
        }
        var pending = defaults.stringArray(forKey: Keys.pending) ?? []
        var last: PHPersistentChangeToken?
        for change in try library.fetchPersistentChanges(since: token) {
            for id in try change.changeDetails(for: .asset).insertedLocalIdentifiers where !pending.contains(id) {
                pending.append(id)
            }
            last = change.changeToken
        }
        defaults.set(pending, forKey: Keys.pending)
        if let last { try save(token: last) }
    }

    private func save(token: PHPersistentChangeToken) throws {
        defaults.set(try NSKeyedArchiver.archivedData(withRootObject: token, requiringSecureCoding: true), forKey: Keys.token)
    }

    // 把待处理的照片逐个导出原图，交给后台 URLSession。返回交出去的文件数。
    private func queueUploads() throws -> Int {
        var pending = defaults.stringArray(forKey: Keys.pending) ?? []
        guard !pending.isEmpty, let url = URL(string: server + "/api/ingest") else { return 0 }
        let assets = PHAsset.fetchAssets(withLocalIdentifiers: pending, options: nil)
        var byID: [String: PHAsset] = [:]
        assets.enumerateObjects { asset, _, _ in byID[asset.localIdentifier] = asset }
        var queued = 0
        while let id = pending.first, queued < 30 { // 一次最多交 30 个，剩下的下次
            if let asset = byID[id] {
                let resources = PHAssetResource.assetResources(for: asset)
                    .filter { [.photo, .video, .pairedVideo].contains($0.type) }
                for resource in resources {
                    let file = spool.appendingPathComponent(UUID().uuidString)
                    try export(resource, to: file)
                    var req = URLRequest(url: url)
                    req.httpMethod = "POST"
                    req.setValue(encodedName(resource.originalFilename), forHTTPHeaderField: "X-File-Name")
                    req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
                    let task = session.uploadTask(with: req, fromFile: file)
                    task.taskDescription = file.path
                    task.resume()
                    queued += 1
                }
            }
            pending.removeFirst() // 照片已经删了的也去掉
            defaults.set(pending, forKey: Keys.pending)
        }
        return queued
    }

    private func export(_ resource: PHAssetResource, to file: URL) throws {
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
        let done = DispatchSemaphore(value: 0)
        var failure: Error?
        PHAssetResourceManager.default().writeData(for: resource, toFile: file, options: opts) { error in
            failure = error
            done.signal()
        }
        done.wait()
        if let failure { throw failure }
    }

    // MARK: 后台上传的结果

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        if let path = task.taskDescription { try? FileManager.default.removeItem(atPath: path) }
        let code = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        if error == nil, (200..<300).contains(code) {
            let n = defaults.integer(forKey: Keys.uploaded) + 1
            defaults.set(n, forKey: Keys.uploaded)
            setStatus("\(Self.time()) 后台已传完一个（累计 \(n) 个）")
        } else {
            setStatus("\(Self.time()) 上传失败：\(error?.localizedDescription ?? "HTTP \(code)")")
        }
    }

    func urlSessionDidFinishEvents(forBackgroundURLSession session: URLSession) {
        DispatchQueue.main.async {
            self.backgroundCompletion?()
            self.backgroundCompletion = nil
        }
    }

    // MARK: 状态

    private func setStatus(_ text: String) {
        defaults.set(text, forKey: Keys.status)
        DispatchQueue.main.async { self.status = text }
    }

    private static func time() -> String {
        let f = DateFormatter()
        f.dateFormat = "HH:mm"
        return f.string(from: Date())
    }

    // 测试版用：最多每 10 分钟问服务端一声 /api/ingest/exists，服务端日志里看得到 App 在后台活着。
    private func heartbeat(_ text: String) {
        guard Date().timeIntervalSince(lastHeartbeat) > 600 || !text.hasPrefix("reason=location"),
              let url = URL(string: server + "/api/ingest/exists")
        else { return }
        lastHeartbeat = Date()
        var req = URLRequest(url: url, timeoutInterval: 10)
        req.setValue(encodedName("xiaoju-app-heartbeat " + text), forHTTPHeaderField: "X-File-Name")
        req.setValue("0", forHTTPHeaderField: "X-File-Size")
        URLSession.shared.dataTask(with: req).resume()
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        KeepAliveBackup.shared.resume()
        return true
    }

    func application(_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String,
                     completionHandler: @escaping () -> Void) {
        KeepAliveBackup.shared.backgroundCompletion = completionHandler
        KeepAliveBackup.shared.resume()
    }
}
