import Foundation
import Security

// 令牌存钥匙串；推送任务和「上次推过什么」存 UserDefaults。
enum Keychain {
    private static let service = "com.langhua98.xiaoju.push"

    static func read(_ key: String) -> String {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                    kSecAttrAccount as String: key, kSecReturnData as String: true]
        var out: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &out) == errSecSuccess, let data = out as? Data else { return "" }
        return String(data: data, encoding: .utf8) ?? ""
    }

    static func write(_ key: String, _ value: String) {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                                    kSecAttrAccount as String: key]
        SecItemDelete(query as CFDictionary)
        guard !value.isEmpty else { return }
        var add = query
        add[kSecValueData as String] = Data(value.utf8)
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        SecItemAdd(add as CFDictionary, nil)
    }
}

/// 一个推送任务：云盘文件夹 → 仓库 / 分支 / 仓库里的目录。
struct Mapping: Codable, Identifiable, Hashable {
    var id = UUID()
    var cloudPath = ""
    var repo = ""      // owner/name
    var branch = ""    // 空 = 仓库默认分支
    var repoPath = ""  // 空 = 仓库根目录

    var title: String {
        let name = cloudPath.split(separator: "/").last.map(String.init) ?? cloudPath
        return name.isEmpty ? "未命名" : name
    }

    var target: String {
        var s = repo.isEmpty ? "（没选仓库）" : repo
        if !branch.isEmpty { s += "@" + branch }
        if !repoPath.isEmpty { s += ":/" + repoPath }
        return s
    }
}

/// 某个文件上次推送时的云盘 sha256 和 git blob sha。
struct Pushed: Codable {
    var sha256: String
    var blob: String
}

@MainActor
final class Store: ObservableObject {
    static let defaultCloudURL = "https://langhua1998-xiaoju-cloud.hf.space"

    @Published var cloudURL: String { didSet { UserDefaults.standard.set(cloudURL, forKey: "cloudURL") } }
    @Published var cloudToken: String { didSet { Keychain.write("cloudToken", cloudToken) } }
    @Published var githubToken: String { didSet { Keychain.write("githubToken", githubToken) } }
    @Published var mappings: [Mapping] { didSet { save(mappings, "mappings") } }
    private(set) var pushed: [String: Pushed]

    init() {
        let d = UserDefaults.standard
        cloudURL = d.string(forKey: "cloudURL") ?? Store.defaultCloudURL
        cloudToken = Keychain.read("cloudToken")
        githubToken = Keychain.read("githubToken")
        mappings = Store.load("mappings") ?? []
        pushed = Store.load("pushed") ?? [:]
    }

    var ready: Bool { !cloudToken.isEmpty && !githubToken.isEmpty }

    func engine() -> PushEngine {
        PushEngine(cloudBase: cloudURL.isEmpty ? Store.defaultCloudURL : cloudURL,
                   cloudToken: cloudToken, githubToken: githubToken)
    }

    func upsert(_ m: Mapping) {
        if let i = mappings.firstIndex(where: { $0.id == m.id }) { mappings[i] = m } else { mappings.append(m) }
    }

    func remove(_ m: Mapping) { mappings.removeAll { $0.id == m.id } }

    func record(_ entries: [String: Pushed]) {
        pushed.merge(entries) { _, new in new }
        save(pushed, "pushed")
    }

    private func save<T: Encodable>(_ v: T, _ key: String) {
        if let data = try? JSONEncoder().encode(v) { UserDefaults.standard.set(data, forKey: key) }
    }

    private static func load<T: Decodable>(_ key: String) -> T? {
        guard let data = UserDefaults.standard.data(forKey: key) else { return nil }
        return try? JSONDecoder().decode(T.self, from: data)
    }
}
