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

    let base: URL = {
        let raw = Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? ""
        return URL(string: raw) ?? URL(string: "https://localhost")!
    }()

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
        let (data, resp) = try await URLSession.shared.data(for: req)
        try check(data, resp)
        return data
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
        let message = body["error"] as? String ?? "HTTP \(status)"
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

    func createShare(_ item: CloudItem, days: Int, password: String) async throws -> CloudShare {
        try await post("/api/shares", json: ["file_id": item.id, "expires_days": days, "password": password], as: CloudShare.self)
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
