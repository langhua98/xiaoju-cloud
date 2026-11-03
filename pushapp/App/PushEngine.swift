import CryptoKit
import Foundation

// 小橘云盘 agent 接口（/api/agent/v1，Bearer xa_…）+ GitHub Git Data 接口。
// 找改动（从便宜到贵）：
//  1. 云盘 sha256 和上次推送时一样、仓库里那个文件也没被别人改过 → 跳过，不下载；
//  2. 否则下载，本地算 git blob sha，和仓库里一样 → 跳过；
//  3. 不一样才放进这次提交。云盘删掉的文件不会在 GitHub 删除。

struct PushError: LocalizedError {
    let message: String
    init(_ m: String) { message = m }
    var errorDescription: String? { message }
}

struct CloudItem: Decodable, Hashable {
    let id: String
    let name: String
    let type: String
    let path: String?
    let size: Int64?
    let sha256: String?
    var isFolder: Bool { type == "folder" }
}

struct Repo: Decodable, Hashable {
    let full_name: String
    let default_branch: String
    let `private`: Bool
}

struct Change: Identifiable {
    var id: String { repoPath }
    let repoPath: String
    let isNew: Bool
    let data: Data
    let blob: String
    let mode: String
    let sha256: String
    let diff: FileDiff
}

struct Plan {
    let repo: String
    let branch: String
    let head: String
    let changes: [Change]
    let unchanged: Int
    let skipped: [String]
    /// 内容和仓库一样、只需要更新记录的文件
    let confirmed: [String: Pushed]

    func recordKey(_ path: String) -> String { "\(repo)@\(branch):\(path)" }
}

final class PushEngine {
    private let cloudBase: String
    private let cloudToken: String
    private let githubToken: String
    private let session: URLSession = {
        let c = URLSessionConfiguration.default
        c.timeoutIntervalForRequest = 60
        c.timeoutIntervalForResource = 600
        return URLSession(configuration: c)
    }()

    static let maxFileBytes: Int64 = 50 * 1024 * 1024
    static let skipNames: Set<String> = [".git", "node_modules", ".DS_Store"]

    init(cloudBase: String, cloudToken: String, githubToken: String) {
        var b = cloudBase.trimmingCharacters(in: .whitespacesAndNewlines)
        while b.hasSuffix("/") { b.removeLast() }
        self.cloudBase = b
        self.cloudToken = cloudToken.trimmingCharacters(in: .whitespacesAndNewlines)
        self.githubToken = githubToken.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    // MARK: 请求

    private func send(_ req: URLRequest, what: String) async throws -> Data {
        var lastError: Error?
        for attempt in 0..<3 {
            if Task.isCancelled { throw PushError("已取消") }
            do {
                let (data, resp) = try await session.data(for: req)
                let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
                if (200..<300).contains(code) { return data }
                // 云盘在 Hugging Face 上可能在睡觉（502/503），等一下再试
                if code >= 500 && attempt < 2 {
                    try await Task.sleep(nanoseconds: UInt64(3 + attempt * 5) * 1_000_000_000)
                    continue
                }
                throw HTTPFailure(code: code, body: data)
            } catch let e as HTTPFailure {
                throw PushError("\(what)失败（\(e.code)）：\(e.text)")
            } catch {
                if Task.isCancelled { throw PushError("已取消") }
                lastError = error
                if attempt < 2 { try await Task.sleep(nanoseconds: 3_000_000_000) }
            }
        }
        throw PushError("\(what)失败：\(lastError?.localizedDescription ?? "网络错误")")
    }

    private struct HTTPFailure: Error {
        let code: Int
        let body: Data
        var text: String {
            if let j = try? JSONSerialization.jsonObject(with: body) as? [String: Any] {
                if let m = j["message"] as? String { return m }
                if let e = j["error"] as? [String: Any], let m = e["message"] as? String { return m }
                if let m = j["error"] as? String { return m }
            }
            return String(data: body.prefix(200), encoding: .utf8) ?? ""
        }
    }

    private func cloud(_ path: String, query: [String: String] = [:]) async throws -> Data {
        guard var c = URLComponents(string: cloudBase + path) else { throw PushError("云盘地址不对") }
        if !query.isEmpty { c.queryItems = query.map { URLQueryItem(name: $0.key, value: $0.value) } }
        guard let url = c.url else { throw PushError("云盘地址不对") }
        var req = URLRequest(url: url)
        req.setValue("Bearer \(cloudToken)", forHTTPHeaderField: "Authorization")
        return try await send(req, what: "云盘请求")
    }

    private func github(_ method: String, _ path: String, body: [String: Any]? = nil,
                        allow404: Bool = false) async throws -> Any? {
        guard let url = URL(string: "https://api.github.com" + path) else { throw PushError("GitHub 地址不对") }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.setValue("Bearer \(githubToken)", forHTTPHeaderField: "Authorization")
        req.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        req.setValue("2022-11-28", forHTTPHeaderField: "X-GitHub-Api-Version")
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        do {
            let data = try await send(req, what: "GitHub ")
            return data.isEmpty ? nil : try JSONSerialization.jsonObject(with: data)
        } catch let e as PushError where allow404 && e.message.contains("（404）") {
            return nil
        }
    }

    private func dict(_ v: Any?) throws -> [String: Any] {
        guard let d = v as? [String: Any] else { throw PushError("GitHub 返回的内容看不懂") }
        return d
    }

    private static func enc(_ s: String) -> String {
        s.split(separator: "/").map { $0.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? String($0) }
            .joined(separator: "/")
    }

    static func clean(_ p: String) -> [String] {
        p.split(separator: "/").map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty && $0 != "." && $0 != ".." }
    }

    static func gitBlobSHA(_ data: Data) -> String {
        var h = Insecure.SHA1()
        h.update(data: Data("blob \(data.count)\0".utf8))
        h.update(data: data)
        return h.finalize().map { String(format: "%02x", $0) }.joined()
    }

    // MARK: 云盘

    private struct Listing: Decodable {
        let item: CloudItem?
        let children: [CloudItem]?
        let next_cursor: String?
    }

    func listFolder(_ path: String) async throws -> [CloudItem] {
        var out: [CloudItem] = []
        var cursor: String?
        repeat {
            var q = ["path": path, "limit": "1000"]
            if let cursor { q["cursor"] = cursor }
            let l = try JSONDecoder().decode(Listing.self, from: try await cloud("/api/agent/v1/items", query: q))
            if let item = l.item, !item.isFolder { throw PushError("云盘里的 \(path) 不是文件夹") }
            out += l.children ?? []
            cursor = l.next_cursor
        } while cursor != nil
        return out
    }

    /// 文件夹里所有文件（含子文件夹），返回（相对路径, 文件）。
    func walk(_ root: String) async throws -> [(String, CloudItem)] {
        var out: [(String, CloudItem)] = []
        var stack: [(String, String)] = [("", "/" + Self.clean(root).joined(separator: "/"))]
        while let top = stack.popLast() {
            let (rel, path) = top
            for c in try await listFolder(path) where !Self.skipNames.contains(c.name) {
                let r = rel.isEmpty ? c.name : rel + "/" + c.name
                if c.isFolder {
                    stack.append((r, (path == "/" ? "" : path) + "/" + c.name))
                } else {
                    out.append((r, c))
                }
            }
        }
        return out.sorted { $0.0 < $1.0 }
    }

    func download(_ item: CloudItem) async throws -> Data {
        try await cloud("/api/agent/v1/items/\(item.id)/content")
    }

    // MARK: GitHub

    func whoami() async throws -> String {
        (try dict(try await github("GET", "/user"))["login"] as? String) ?? "?"
    }

    func checkCloud() async throws -> String {
        let j = try JSONSerialization.jsonObject(with: try await cloud("/api/agent/v1/me")) as? [String: Any]
        if let a = j?["agent"] as? [String: Any], let n = a["name"] as? String { return n }
        return "已连接"
    }

    func repos() async throws -> [Repo] {
        var all: [Repo] = []
        for page in 1...10 {
            let v = try await github("GET", "/user/repos?per_page=100&sort=pushed&page=\(page)")
            let data = try JSONSerialization.data(withJSONObject: v ?? [])
            let batch = try JSONDecoder().decode([Repo].self, from: data)
            all += batch
            if batch.count < 100 { break }
        }
        return all
    }

    func branches(_ repo: String) async throws -> [String] {
        let v = try await github("GET", "/repos/\(repo)/branches?per_page=100")
        return (v as? [[String: Any]] ?? []).compactMap { $0["name"] as? String }
    }

    /// 仓库里某个文件现在的内容（用来算改了哪几行）
    func repoBlob(_ repo: String, _ sha: String) async throws -> Data {
        let b = try dict(try await github("GET", "/repos/\(repo)/git/blobs/\(sha)"))
        let s = (b["content"] as? String ?? "").replacingOccurrences(of: "\n", with: "")
        return Data(base64Encoded: s) ?? Data()
    }

    /// 找出要推的文件，不提交。
    func plan(_ m: Mapping, pushed: [String: Pushed], progress: @escaping (String) -> Void) async throws -> Plan {
        let repo = m.repo.trimmingCharacters(in: .whitespaces)
        guard repo.contains("/") else { throw PushError("仓库要写成 owner/name") }
        var branch = m.branch.trimmingCharacters(in: .whitespaces)
        if branch.isEmpty {
            branch = (try dict(try await github("GET", "/repos/\(repo)"))["default_branch"] as? String) ?? "main"
        }
        progress("读取 \(repo) 的文件列表…")
        guard let ref = try await github("GET", "/repos/\(repo)/git/ref/heads/\(Self.enc(branch))", allow404: true) else {
            throw PushError("\(repo) 没有分支 \(branch)")
        }
        let head = (try dict(try dict(ref)["object"])["sha"] as? String) ?? ""
        let commit = try dict(try await github("GET", "/repos/\(repo)/git/commits/\(head)"))
        let baseTree = (try dict(commit["tree"])["sha"] as? String) ?? ""
        let tree = try dict(try await github("GET", "/repos/\(repo)/git/trees/\(baseTree)?recursive=1"))
        var inRepo: [String: (sha: String, mode: String)] = [:]
        for e in tree["tree"] as? [[String: Any]] ?? [] where e["type"] as? String == "blob" {
            if let p = e["path"] as? String, let s = e["sha"] as? String {
                inRepo[p] = (s, e["mode"] as? String ?? "100644")
            }
        }

        progress("读取云盘 \(m.cloudPath)…")
        let files = try await walk(m.cloudPath)
        let prefix = Self.clean(m.repoPath).joined(separator: "/")
        var changes: [Change] = []
        var skipped: [String] = []
        var confirmed: [String: Pushed] = [:]
        var unchanged = 0
        for (i, (rel, item)) in files.enumerated() {
            let rp = prefix.isEmpty ? rel : prefix + "/" + rel
            let key = "\(repo)@\(branch):\(rp)"
            let cur = inRepo[rp]
            if let s = item.sha256, let old = pushed[key], old.sha256 == s, let cur, cur.sha == old.blob {
                unchanged += 1
                continue
            }
            if (item.size ?? 0) > Self.maxFileBytes {
                skipped.append(rp + "（超过 50MB）")
                continue
            }
            if Task.isCancelled { throw PushError("已取消") }
            progress("比较 \(i + 1)/\(files.count)：\(rel)")
            let data = try await download(item)
            let blob = Self.gitBlobSHA(data)
            if blob == cur?.sha {
                unchanged += 1
                confirmed[key] = Pushed(sha256: item.sha256 ?? "", blob: blob)
                continue
            }
            var old: Data?
            if let cur { old = try await repoBlob(repo, cur.sha) }
            changes.append(Change(repoPath: rp, isNew: cur == nil, data: data, blob: blob,
                                  mode: cur?.mode ?? "100644", sha256: item.sha256 ?? "",
                                  diff: FileDiff.make(old: old, new: data)))
        }
        return Plan(repo: repo, branch: branch, head: head, changes: changes, unchanged: unchanged,
                    skipped: skipped, confirmed: confirmed)
    }

    /// 上传改动并提交；返回提交的网址。分支在这期间被别人推过，就在新的 HEAD 上重做（不强推）。
    func commit(_ plan: Plan, message: String, progress: @escaping (String) -> Void) async throws -> (sha: String, url: String) {
        guard !plan.changes.isEmpty else { throw PushError("没有改动，不提交") }
        var entries: [[String: Any]] = []
        for (i, c) in plan.changes.enumerated() {
            progress("上传 \(i + 1)/\(plan.changes.count)：\(c.repoPath)")
            let b = try dict(try await github("POST", "/repos/\(plan.repo)/git/blobs",
                                              body: ["content": c.data.base64EncodedString(), "encoding": "base64"]))
            entries.append(["path": c.repoPath, "mode": c.mode, "type": "blob", "sha": b["sha"] as? String ?? c.blob])
        }
        var head = plan.head
        let refPath = "/repos/\(plan.repo)/git/refs/heads/\(Self.enc(plan.branch))"
        for attempt in 0..<3 {
            progress("提交…")
            let parent = try dict(try await github("GET", "/repos/\(plan.repo)/git/commits/\(head)"))
            let baseTree = (try dict(parent["tree"])["sha"] as? String) ?? ""
            let tree = try dict(try await github("POST", "/repos/\(plan.repo)/git/trees",
                                                 body: ["base_tree": baseTree, "tree": entries]))
            let commit = try dict(try await github("POST", "/repos/\(plan.repo)/git/commits",
                                                   body: ["message": message, "tree": tree["sha"] ?? "", "parents": [head]]))
            let sha = commit["sha"] as? String ?? ""
            do {
                progress("推送…")
                _ = try await github("PATCH", refPath, body: ["sha": sha, "force": false])
                return (sha, "https://github.com/\(plan.repo)/commit/\(sha)")
            } catch let e as PushError where e.message.contains("（422）") && attempt < 2 {
                let ref = try dict(try await github("GET", "/repos/\(plan.repo)/git/ref/heads/\(Self.enc(plan.branch))"))
                head = (try dict(ref["object"])["sha"] as? String) ?? head
            }
        }
        throw PushError("分支一直在变，没推上去，稍后再试")
    }
}
