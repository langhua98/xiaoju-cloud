import Foundation
import Security
import SwiftUI

// 网盘接口（/api/*）。登录后拿到的会话（12 小时）存在钥匙串里，每个请求带 X-Xiaoju-Session。
// 会话过期（401）时回到登录页；开启了格子口令时，看照片、视频会被抽查（403 grid_required），
// 这时弹出格子题（GridGateView），答对了再接着看。
@MainActor
final class Cloud: ObservableObject {
    static let shared = Cloud()

    @Published private(set) var session: String? = Keychain.read("session")
    @Published var gridPrompt: GridPrompt? // 有值时弹出格子题
    private var gridWaiters: [CheckedContinuation<Bool, Never>] = []

    struct GridPrompt: Identifiable {
        let id = UUID()
    }

    // 家里电脑上的云盘
    let primary: URL = {
        let raw = Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? ""
        return URL(string: raw) ?? URL(string: "https://localhost")!
    }()
    // Hugging Face 上的只读备用：电脑关着、上面那个连不上时自动换过去，只能看、不能改；电脑开了再换回来
    let fallback: URL? = {
        let raw = Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIFallback") as? String ?? ""
        return raw.isEmpty ? nil : URL(string: raw)
    }()
    @Published private(set) var standby = false // 正在用只读备用
    private var picking: Task<Bool, Never>?
    private var lastHomeCheck = Date.distantPast

    var base: URL { standby ? (fallback ?? primary) : primary }

    let webURL: URL? = URL(string: Bundle.main.object(forInfoDictionaryKey: "XiaojuWebURL") as? String ?? "")

    var signedIn: Bool { session != nil }

    func signIn(session: String) {
        Keychain.write("session", session)
        self.session = session
    }

    func signOut() {
        Keychain.delete("session")
        session = nil
    }

    // 服务端给的相对地址（/thumb/…、/dl/…）补成完整地址
    func absolute(_ path: String?) -> URL? {
        guard let path, !path.isEmpty else { return nil }
        if path.hasPrefix("http") { return URL(string: path) }
        return URL(string: path, relativeTo: base)?.absoluteURL
    }

    // MARK: 请求

    func get<T: Decodable>(_ path: String, as type: T.Type = T.self) async throws -> T {
        try Self.decoder.decode(T.self, from: try await send("GET", path))
    }

    func post<T: Decodable>(_ path: String, json: [String: Any], as type: T.Type = T.self) async throws -> T {
        try Self.decoder.decode(T.self, from: try await send("POST", path, json: json))
    }

    @discardableResult
    func send(_ method: String, _ path: String, json: [String: Any]? = nil, body: Data? = nil,
              contentType: String? = nil) async throws -> Data {
        await checkHomeBack()
        guard let url = URL(string: path, relativeTo: base) else { throw CloudError.server("地址不对：\(path)") }
        var req = URLRequest(url: url)
        req.httpMethod = method
        if let session { req.setValue(session, forHTTPHeaderField: "X-Xiaoju-Session") }
        if let json {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: json)
        } else if let body {
            req.setValue(contentType ?? "application/octet-stream", forHTTPHeaderField: "Content-Type")
            req.httpBody = body
        }
        // 被 Hugging Face 限流（429）：大家一起等一会儿再试（见 RateGate.swift），几次都不行才报错
        var attempt = 0
        var switched = false
        while true {
            try await RateGate.shared.wait()
            let data: Data, resp: URLResponse
            do {
                (data, resp) = try await URLSession.shared.data(for: req)
            } catch let err as URLError where err.code != .cancelled {
                // 连不上：换个服务端（电脑 ↔ 备用）再试一次
                guard !switched, await pickServer(), let again = URL(string: path, relativeTo: base) else { throw err }
                switched = true
                req.url = again
                continue
            }
            let http = resp as? HTTPURLResponse
            // 隧道那头的电脑关了：网关回 502/503/504
            if let code = http?.statusCode, [502, 503, 504].contains(code), !switched, await pickServer(),
               let again = URL(string: path, relativeTo: base) {
                switched = true
                req.url = again
                continue
            }
            if http?.statusCode == 429 && attempt < 3 {
                attempt += 1
                await RateGate.shared.limited(retryAfter: http?.value(forHTTPHeaderField: "Retry-After"))
                continue
            }
            if http.map({ (200..<300).contains($0.statusCode) }) ?? false { await RateGate.shared.ok() }
            try check(data, resp)
            return data
        }
    }

    // MARK: 只读备用

    // 重新挑服务端：先试家里电脑，不行再试备用。换了返回 true。同时只挑一次。
    func pickServer() async -> Bool {
        guard let fallback else { return false }
        if let picking { return await picking.value }
        let task = Task<Bool, Never> {
            let before = self.standby
            if await Self.alive(self.primary, timeout: 6) {
                self.standby = false
            } else if await Self.alive(fallback, timeout: 45) { // 备用在 Hugging Face 上，休眠了要唤醒
                self.standby = true
            }
            self.lastHomeCheck = Date()
            return self.standby != before
        }
        picking = task
        let changed = await task.value
        picking = nil
        return changed
    }

    // 用着备用时，每分钟最多看一次电脑开了没有，开了就换回去
    private func checkHomeBack() async {
        guard standby, Date().timeIntervalSince(lastHomeCheck) > 60 else { return }
        lastHomeCheck = Date()
        if await Self.alive(primary, timeout: 6) { standby = false }
    }

    private static func alive(_ base: URL, timeout: TimeInterval) async -> Bool {
        guard let url = URL(string: "/api/speedtest/ping", relativeTo: base) else { return false }
        var req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: timeout)
        req.httpMethod = "GET"
        guard let (_, resp) = try? await URLSession.shared.data(for: req) else { return false }
        return (resp as? HTTPURLResponse)?.statusCode == 204
    }

    // 上传文件（流式，从文件读）：POST /api/files?name=&parent_id=
    func upload(file: URL, name: String, parent: String?) async throws {
        var path = "/api/files?name=" + (name.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed.subtracting(CharacterSet(charactersIn: "&=+"))) ?? name)
        if let parent { path += "&parent_id=" + parent }
        guard let url = URL(string: path, relativeTo: base) else { throw CloudError.server("文件名不对") }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        if let session { req.setValue(session, forHTTPHeaderField: "X-Xiaoju-Session") }
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        let (data, resp) = try await URLSession.shared.upload(for: req, fromFile: file)
        try check(data, resp)
    }

    private func check(_ data: Data, _ resp: URLResponse) throws {
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if (200..<300).contains(status) { return }
        let body = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
        let code = body["code"] as? String ?? ""
        let message = body["error"] as? String
            ?? (status == 429 ? "服务器这会儿请求太多，被限流了（HTTP 429）。等一两分钟再下拉刷新。" : "HTTP \(status)")
        switch (status, code) {
        case (403, "grid_required"):
            throw CloudError.gridRequired
        case (401, _):
            signOut()
            throw CloudError.unauthorized
        default:
            throw CloudError.api(code: code, message: message)
        }
    }

    // 看照片、视频：被格子口令抽查时先答题，答对了再试一次
    func withGrid<T>(_ op: () async throws -> T) async throws -> T {
        do {
            return try await op()
        } catch CloudError.gridRequired {
            guard await askGrid() else { throw CloudError.unauthorized }
            return try await op()
        }
    }

    private func askGrid() async -> Bool {
        await withCheckedContinuation { cont in
            gridWaiters.append(cont)
            if gridPrompt == nil { gridPrompt = GridPrompt() }
        }
    }

    func finishGrid(_ ok: Bool) {
        gridPrompt = nil
        let waiters = gridWaiters
        gridWaiters = []
        waiters.forEach { $0.resume(returning: ok) }
    }

    // MARK: 常用接口

    func children(of folder: String?) async throws -> [CloudItem] {
        try await get("/api/folders/\(folder ?? "root")/children", as: ItemList.self).items
    }

    // 根目录下的某个文件夹（相册、隐藏相册……），没有返回 nil
    func rootFolder(named name: String) async throws -> CloudItem? {
        try await children(of: nil).first { $0.isFolder && $0.name == name }
    }

    // 照片、视频的查看地址：照片优先用大图预览（最长边 1600），视频和其他文件用原文件
    func link(for item: CloudItem) async throws -> FileLink {
        try await withGrid {
            try await self.post("/api/files/\(item.id)/link", json: [:], as: FileLink.self)
        }
    }

    func trash(_ item: CloudItem) async throws {
        try await send("DELETE", "/api/files/\(item.id)")
    }

    // 下载原文件到临时目录（保留文件名，存到「照片」「文件」时按扩展名认格式），边下边报进度 0…1，
    // 下到的字节同时记进网速表（「备份」页的下载卡片），取消了的也算。
    // 网络断了（切网络、服务端重启）就从断开的地方接着下（服务端支持 Range）；连续 5 次一点都没下到才算失败。
    func download(_ item: CloudItem, progress: @escaping @MainActor (Double) -> Void = { _ in }) async throws -> URL {
        let link = try await self.link(for: item)
        guard let url = absolute(link.url) else { throw CloudError.server("下载地址不对") }
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let dest = dir.appendingPathComponent(item.name)
        let box = DownloadBox()
        var resume: Data?
        var stalls = 0
        while true {
            do {
                return try await downloadOnce(url: url, resume: resume, to: dest, box: box, progress: progress)
            } catch {
                guard !Task.isCancelled,
                      let data = (error as NSError).userInfo[NSURLSessionDownloadTaskResumeData] as? Data else { throw error }
                if box.moved { stalls = 0 } else { stalls += 1 }
                guard stalls < 5 else { throw error }
                resume = data
                try await Task.sleep(for: .seconds(min(2 * (stalls + 1), 10)))
            }
        }
    }

    private func downloadOnce(url: URL, resume: Data?, to dest: URL, box: DownloadBox,
                              progress: @escaping @MainActor (Double) -> Void) async throws -> URL {
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<URL, Error>) in
                let done: @Sendable (URL?, URLResponse?, Error?) -> Void = { temp, resp, error in
                    box.observation?.invalidate()
                    if let error { return cont.resume(throwing: error) }
                    let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
                    guard let temp, (200..<300).contains(code) else {
                        return cont.resume(throwing: CloudError.server("下载失败（HTTP \(code)）"))
                    }
                    do {
                        try FileManager.default.moveItem(at: temp, to: dest) // 回调返回后系统会删掉 temp，必须在这里移走
                        let size = (try? FileManager.default.attributesOfItem(atPath: dest.path)[.size] as? Int64) ?? 0
                        box.count(upTo: size) // 补上最后一次进度回调之后到的
                        cont.resume(returning: dest)
                    } catch {
                        cont.resume(throwing: error)
                    }
                }
                let task = resume.map { URLSession.shared.downloadTask(withResumeData: $0, completionHandler: done) }
                    ?? URLSession.shared.downloadTask(with: url, completionHandler: done)
                box.task = task
                box.startAttempt()
                // 看已收到的字节数（服务端没给总大小时 fractionCompleted 不动，字节数照样涨；接着下时从断点算起）
                box.observation = task.progress.observe(\.completedUnitCount) { p, _ in
                    box.count(upTo: p.completedUnitCount)
                    box.saw(p.completedUnitCount)
                    let value = p.totalUnitCount > 0 ? p.fractionCompleted : 0
                    Task { @MainActor in progress(value) }
                }
                task.resume()
            }
        } onCancel: {
            box.task?.cancel()
        }
    }

    func createShare(_ item: CloudItem, days: Int, password: String) async throws -> CloudShare {
        try await post("/api/shares", json: ["file_id": item.id, "expires_days": days, "password": password], as: CloudShare.self)
    }

    // 直链：点开就下载（服务端 /d/{token}/文件名，没设提取码的单个文件才有）。末尾的文件名只是让下载工具好起名。
    func directURL(_ share: CloudShare, name: String) -> String {
        let encoded = name.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed.subtracting(CharacterSet(charactersIn: "/"))) ?? "file"
        return absolute("/d/\(share.token)/\(encoded)")?.absoluteString ?? share.token
    }

    func shareURL(_ share: CloudShare) -> String {
        guard let web = webURL else { return share.token }
        return URL(string: "share.html#\(share.token)", relativeTo: web)?.absoluteString ?? share.token
    }

    // MARK: 解析

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        // Go 的时间带纳秒（2026-10-20T12:34:56.123456789Z）：去掉小数再按 ISO 8601 解析
        d.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            let trimmed = raw.replacingOccurrences(of: #"\.\d+"#, with: "", options: .regularExpression)
            if let date = ISO8601DateFormatter().date(from: trimmed) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "bad date \(raw)"))
        }
        return d
    }()
}

enum CloudError: LocalizedError {
    case unauthorized
    case gridRequired
    case api(code: String, message: String)
    case server(String)

    var errorDescription: String? {
        switch self {
        case .unauthorized: return "登录已过期，请重新登录"
        case .gridRequired: return "需要先答一道格子题"
        case .api(_, let message): return message
        case .server(let message): return message
        }
    }
}

struct ItemList: Decodable {
    let items: [CloudItem]
}

extension Error {
    // 被取消的请求（切走页面、下拉刷新时界面更新，系统取消了加载任务）：不是出错，不用显示
    var isCancellation: Bool {
        self is CancellationError || (self as? URLError)?.code == .cancelled
    }
}

struct CloudItem: Decodable, Identifiable, Hashable {
    let id: String
    let parentID: String?
    let name: String
    let type: String
    let size: Int64
    let mime: String
    let takenAt: Date?
    let createdAt: Date
    let trashedAt: Date?
    let isPrivate: Bool?
    let thumb: String?
    let duration: Double?
    let counts: Counts?
    let cover: String?

    struct Counts: Decodable, Hashable {
        let photos: Int
        let videos: Int
        let others: Int
    }

    enum CodingKeys: String, CodingKey {
        case id, name, type, size, mime, thumb, duration, counts, cover
        case parentID = "parent_id"
        case takenAt = "taken_at"
        case createdAt = "created_at"
        case trashedAt = "trashed_at"
        case isPrivate = "private"
    }

    var isFolder: Bool { type == "folder" }
    var isVideo: Bool { mime.hasPrefix("video/") || ["mov", "mp4", "m4v"].contains(ext) }
    var isImage: Bool { mime.hasPrefix("image/") || ["heic", "jpg", "jpeg", "png", "gif", "webp", "dng"].contains(ext) }
    var isMedia: Bool { !isFolder && (isImage || isVideo) }
    var ext: String { (name as NSString).pathExtension.lowercased() }
    var date: Date { takenAt ?? createdAt }

    // 文件页的类型筛选
    var kind: FileKind {
        if isFolder { return .all }
        if isImage { return .image }
        if isVideo { return .video }
        if ["zip", "rar", "7z", "tar", "gz"].contains(ext) { return .archive }
        return .document
    }
}

enum FileKind: String, CaseIterable, Identifiable {
    case all = "全部", image = "图片", video = "视频", document = "文档", archive = "压缩包"
    var id: String { rawValue }
}

struct FileLink: Decodable {
    let url: String
    let preview: String?
}

struct CloudShare: Decodable, Identifiable {
    let id: String
    let token: String
    let expiresAt: Date?
    let views: Int
    let createdAt: Date
    let file: CloudItem?
    let hasPassword: Bool
    let expired: Bool

    enum CodingKeys: String, CodingKey {
        case id, token, views, file, expired
        case expiresAt = "expires_at"
        case createdAt = "created_at"
        case hasPassword = "has_password"
    }
}

struct DriveStats: Decodable {
    let files: Int
    let photos: Int
    let videos: Int
    let others: Int
    let bytes: Int64
    let photoBytes: Int64
    let videoBytes: Int64
    // 「其他」再细分；老服务器没有这几项
    let documents: Int?
    let audios: Int?
    let archives: Int?
    let documentBytes: Int64?
    let audioBytes: Int64?
    let archiveBytes: Int64?

    enum CodingKeys: String, CodingKey {
        case files, photos, videos, others, bytes, documents, audios, archives
        case photoBytes = "photo_bytes"
        case videoBytes = "video_bytes"
        case documentBytes = "document_bytes"
        case audioBytes = "audio_bytes"
        case archiveBytes = "archive_bytes"
    }
}

// 文件大小：「3.4 MB」
func sizeText(_ bytes: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file)
}

// 钥匙串里存会话（比 UserDefaults 安全，删掉 App 后也不会被别的 App 读到）
enum Keychain {
    private static let service = "com.langhua98.xiaoju.backup"

    static func read(_ key: String) -> String? {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                    kSecAttrAccount as String: key, kSecReturnData as String: true]
        var out: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &out) == errSecSuccess, let data = out as? Data else { return nil }
        return String(data: data, encoding: .utf8)
    }

    static func write(_ key: String, _ value: String) {
        delete(key)
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                    kSecAttrAccount as String: key, kSecValueData as String: Data(value.utf8),
                                    kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly]
        SecItemAdd(query as CFDictionary, nil)
    }

    static func delete(_ key: String) {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                    kSecAttrAccount as String: key]
        SecItemDelete(query as CFDictionary)
    }
}

// 下载任务和进度监听，取消时要拿到任务；counted 是已经记进网速表的字节数
private final class DownloadBox: @unchecked Sendable {
    var task: URLSessionDownloadTask?
    var observation: NSKeyValueObservation?
    private let lock = NSLock()
    private var counted: Int64 = 0
    private var attemptLast: Int64 = -1
    private var progressed = false

    // 这一次（接着下的那次）有没有下到新数据：断线重试时判断是不是卡死了
    var moved: Bool { lock.withLock { progressed } }

    func startAttempt() { lock.withLock { attemptLast = -1; progressed = false } }

    func saw(_ bytes: Int64) {
        lock.withLock {
            if attemptLast >= 0 && bytes > attemptLast { progressed = true }
            attemptLast = bytes
        }
    }

    // 收到的字节数涨到 bytes：把多出来的记进网速表
    func count(upTo bytes: Int64) {
        let delta: Int64 = lock.withLock {
            let d = bytes - counted
            if d > 0 { counted = bytes }
            return d
        }
        NetMeter.shared.received(delta)
    }
}
