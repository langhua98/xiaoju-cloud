import ExtensionFoundation
import Foundation
import os
import Photos

// PhotoKit 后台上传扩展：系统在网络、电量合适的时候调用 processJobs()，这里给新照片、视频的每个原始资源建一个上传任务
// （POST /api/ingest），真正的上传由系统在后台做，锁屏、切到别的 App 也照样传。
// 只传最近拍的：第一次被调用时传最近 24 小时的，并记下当时的「变更令牌」，之后按令牌取新增的照片。
// 用的是 iOS 26.1 的 PHBackgroundResourceUploadExtension（iOS 27 起有新的 async 版本，但 GitHub 的 Mac 机器上
// 还没有 iOS 27 的 SDK；旧接口在 iOS 27 上照样能用）。
// 见 https://developer.apple.com/documentation/photokit/uploading-asset-resources-in-the-background
@main
final class BackgroundUploadExtension: PHBackgroundResourceUploadExtension {
    private let log = Logger(subsystem: "com.langhua98.xiaoju.backup", category: "upload")
    private let defaults = UserDefaults.standard
    private let lock = NSLock()
    private var cancelled = false

    private static let tokenKey = "changeToken"
    private static let pendingKey = "pendingAssets" // 还没建上传任务的照片（任务数到上限时留到下次）

    required init() {}

    func process() -> PHBackgroundResourceUploadProcessingResult {
        do {
            try retryFailedJobs()
            try acknowledgeFinishedJobs()
            try collectNewAssets()
            let more = try createJobs()
            return more ? .processing : .completed
        } catch PHPhotosError.limitExceeded {
            return .processing // 任务数到上限：等已建的传完再来
        } catch PHPhotosError.persistentChangeTokenExpired {
            log.error("change token expired, starting over from now")
            defaults.removeObject(forKey: Self.tokenKey)
            return .processing
        } catch {
            log.error("processJobs failed: \(error.localizedDescription, privacy: .public)")
            return .failure
        }
    }

    func notifyTermination() {
        lock.withLock { cancelled = true }
    }

    private var isCancelled: Bool { lock.withLock { cancelled } }

    // 失败的任务：网络断了、超时这类重试；服务端拒绝（4xx）的确认掉，不再传。
    private func retryFailedJobs() throws {
        let library = PHPhotoLibrary.shared()
        let jobs = PHAssetResourceUploadJob.fetchJobs(action: .retry, options: nil)
        for i in 0..<jobs.count {
            let job = jobs.object(at: i)
            let permanent: Bool
            if let err = job.error as? URLError {
                permanent = err.code == .badServerResponse || err.code == .userAuthenticationRequired
            } else {
                permanent = false
            }
            log.info("job failed: \(String(describing: job.error), privacy: .public) permanent=\(permanent)")
            try library.performChangesAndWait {
                guard let req = PHAssetResourceUploadJobChangeRequest(for: job) else { return }
                if permanent {
                    req.acknowledge()
                } else {
                    req.retry(destination: nil)
                }
            }
        }
    }

    // 传完（成功或失败）的任务要确认掉，才能腾出位置建新任务。
    private func acknowledgeFinishedJobs() throws {
        let library = PHPhotoLibrary.shared()
        let jobs = PHAssetResourceUploadJob.fetchJobs(action: .acknowledge, options: nil)
        for i in 0..<jobs.count {
            let job = jobs.object(at: i)
            let name = (job.resource as PHAssetResource?)?.originalFilename ?? "?"
            log.info("job done: \(name, privacy: .public) state=\(String(describing: job.state), privacy: .public)")
            try library.performChangesAndWait {
                PHAssetResourceUploadJobChangeRequest(for: job)?.acknowledge()
            }
        }
    }

    // 按变更令牌找新加进相册的照片，放进待传列表。
    private func collectNewAssets() throws {
        let library = PHPhotoLibrary.shared()
        guard let data = defaults.data(forKey: Self.tokenKey),
              let token = try NSKeyedUnarchiver.unarchivedObject(ofClass: PHPersistentChangeToken.self, from: data)
        else {
            // 第一次：记下现在的令牌，之后按令牌取新增的。系统第一次叫醒扩展可能是打开开关之后很久，
            // 这期间拍的照片令牌里没有，所以把最近 24 小时拍的也放进待传列表，再早的不传。
            try save(token: library.currentChangeToken)
            let options = PHFetchOptions()
            options.predicate = NSPredicate(format: "creationDate > %@", Date().addingTimeInterval(-24 * 3600) as NSDate)
            var recent: [String] = []
            PHAsset.fetchAssets(with: options).enumerateObjects { asset, _, _ in recent.append(asset.localIdentifier) }
            defaults.set(recent, forKey: Self.pendingKey)
            log.info("first run: \(recent.count) photos from the last 24 hours, then new ones from now on")
            return
        }
        var pending = Set(defaults.stringArray(forKey: Self.pendingKey) ?? [])
        var last: PHPersistentChangeToken?
        for change in try library.fetchPersistentChanges(since: token) {
            let details = try change.changeDetails(for: .asset)
            pending.formUnion(details.insertedLocalIdentifiers)
            last = change.changeToken
        }
        defaults.set(Array(pending), forKey: Self.pendingKey)
        if let last {
            try save(token: last)
        }
    }

    private func save(token: PHPersistentChangeToken) throws {
        defaults.set(try NSKeyedArchiver.archivedData(withRootObject: token, requiringSecureCoding: true), forKey: Self.tokenKey)
    }

    // 给待传的照片建上传任务：照片原图、视频原片，实况照片的视频部分也传。还有没建完的返回 true。
    private func createJobs() throws -> Bool {
        var pending = defaults.stringArray(forKey: Self.pendingKey) ?? []
        guard !pending.isEmpty else { return false }
        guard let base = Bundle.main.object(forInfoDictionaryKey: "BackgroundUploadURLBase") as? String,
              let url = URL(string: base + "/api/ingest")
        else {
            log.error("BackgroundUploadURLBase missing")
            return false
        }
        let library = PHPhotoLibrary.shared()
        let assets = PHAsset.fetchAssets(withLocalIdentifiers: pending, options: nil)
        var byID: [String: PHAsset] = [:]
        assets.enumerateObjects { asset, _, _ in byID[asset.localIdentifier] = asset }
        defer { defaults.set(pending, forKey: Self.pendingKey) }
        while let id = pending.first {
            if isCancelled { return true }
            if let asset = byID[id] {
                let resources = PHAssetResource.assetResources(for: asset)
                    .filter { [.photo, .video, .pairedVideo].contains($0.type) }
                try library.performChangesAndWait {
                    for resource in resources {
                        var req = URLRequest(url: url)
                        req.httpMethod = "POST"
                        // 用流量也传（系统可能还是会优先等 Wi-Fi）
                        req.allowsCellularAccess = true
                        req.allowsExpensiveNetworkAccess = true
                        req.allowsConstrainedNetworkAccess = true
                        req.setValue(encodedName(resource.originalFilename), forHTTPHeaderField: "X-File-Name")
                        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
                        _ = PHAssetResourceUploadJobChangeRequest.creationRequestForJob(destination: req, resource: resource)
                    }
                }
                log.info("queued \(resources.map(\.originalFilename).joined(separator: ","), privacy: .public)")
            }
            pending.removeFirst() // 照片已经删了的也从列表里去掉
        }
        return false
    }
}

// 文件名 URL 编码后放进 X-File-Name（请求头放不了中文，服务端会解码）。
func encodedName(_ name: String) -> String {
    name.addingPercentEncoding(withAllowedCharacters: .alphanumerics.union(CharacterSet(charactersIn: "-._~"))) ?? name
}
