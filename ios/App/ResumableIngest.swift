import CryptoKit
import Foundation

// 断点续传：大文件分成 8MB 一段传（POST /api/ingest/uploads 开始，PUT …/{id}?offset= 一段段传）。
// 断网、App 被挂起都没关系：下次再传同一个文件（名字、大小、开头结尾的摘要都一样），服务端从收到的地方接着收。
enum ResumableIngest {
    // 传完返回 true；服务端早就有了（同名同大小）返回 false。headers 加上放进哪个相册的头部。
    static func send(file: URL, name: String, server: String, headers: [String: String],
                     progress: ((Int64, Int64) -> Void)? = nil) async throws -> Bool
    {
        guard let base = URL(string: server) else { throw URLError(.badURL) }
        let handle = try FileHandle(forReadingFrom: file)
        defer { try? handle.close() }
        let size = Int64(try handle.seekToEnd())

        var start = URLRequest(url: base.appendingPathComponent("api/ingest/uploads"))
        start.httpMethod = "POST"
        start.setValue(encodedName(name), forHTTPHeaderField: "X-File-Name")
        start.setValue(String(size), forHTTPHeaderField: "X-File-Size")
        start.setValue(try probe(handle, size: size), forHTTPHeaderField: "X-Upload-Probe")
        for (k, v) in headers { start.setValue(v, forHTTPHeaderField: k) }
        let begun = try await json(start)
        if begun["result"] as? String == "skipped" { return false }
        guard let id = begun["id"] as? String else { throw failure("开始上传没有返回 ID：\(begun)") }
        var offset = (begun["offset"] as? NSNumber)?.int64Value ?? 0
        let chunk = max((begun["chunk"] as? NSNumber)?.int64Value ?? 8 << 20, 1 << 20)
        let target = base.appendingPathComponent("api/ingest/uploads/\(id)")

        var retries = 0
        while true {
            try Task.checkCancellation()
            progress?(offset, size)
            try handle.seek(toOffset: UInt64(offset))
            let piece = try handle.read(upToCount: Int(min(chunk, size - offset))) ?? Data()
            var put = URLRequest(url: URL(string: target.absoluteString + "?offset=\(offset)")!)
            put.httpMethod = "PUT"
            put.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
            put.timeoutInterval = 120
            do {
                let (data, resp) = try await URLSession.shared.upload(for: put, from: piece)
                let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
                let out = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
                switch code {
                case 201:
                    return true
                case 200 where out["same_content"] as? Bool == true:
                    return false
                case 200, 409: // 409：起点对不上（上一段其实收到了），从服务端说的地方接着传
                    guard let next = (out["offset"] as? NSNumber)?.int64Value else { throw failure("HTTP \(code) 没有进度") }
                    offset = next
                    retries = 0
                default:
                    throw failure("HTTP \(code) \(String(decoding: data, as: UTF8.self))")
                }
            } catch let error as URLError {
                // 网络断了：等一下，问服务端收到了多少，接着传
                retries += 1
                if retries > 5 { throw error }
                try await Task.sleep(nanoseconds: UInt64(retries) * 2_000_000_000)
                if let status = try? await json(URLRequest(url: target)),
                   let next = (status["offset"] as? NSNumber)?.int64Value
                {
                    offset = next
                }
            }
        }
    }

    // 文件开头、结尾各 1MB 的 SHA-256：同名同大小的旧上传，只有对上了服务端才接着传。
    private static func probe(_ handle: FileHandle, size: Int64) throws -> String {
        var hash = SHA256()
        try handle.seek(toOffset: 0)
        hash.update(data: try handle.read(upToCount: 1 << 20) ?? Data())
        if size > 1 << 20 {
            try handle.seek(toOffset: UInt64(max(size - (1 << 20), 1 << 20)))
            hash.update(data: try handle.read(upToCount: 1 << 20) ?? Data())
        }
        return hash.finalize().map { String(format: "%02x", $0) }.joined()
    }

    private static func json(_ req: URLRequest) async throws -> [String: Any] {
        let (data, resp) = try await URLSession.shared.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(code) else { throw failure("HTTP \(code) \(String(decoding: data, as: UTF8.self))") }
        return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
    }

    private static func failure(_ text: String) -> NSError {
        NSError(domain: "xiaoju", code: 0, userInfo: [NSLocalizedDescriptionKey: text])
    }
}
