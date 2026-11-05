import SwiftUI
import UIKit

// 服务端在 Hugging Face 上，Hugging Face 的门口按 IP 限流：一段时间里请求太多，就回 429（不经过我们的服务端）。
// 照片网格一屏几十上百张缩略图，快速往下滑几屏就是几千个请求，手机这个 IP 被限住后，文件列表等别的请求也一起 429。
// 所以：
//   - 被限流时所有请求一起停一会儿（按 Retry-After，没有就从 5 秒起翻倍），停完再接着发，不是马上报错；
//   - 缩略图同时最多下几张，滑过去没显示的就不发了（以前每个格子一出现就发）。
actor RateGate {
    static let shared = RateGate()

    private var until = Date.distantPast
    private var strikes = 0 // 连续被限几次：等的时间翻倍
    private var lastHit = Date.distantPast

    // 正在被限流时等到能发
    func wait() async throws {
        while true {
            let left = until.timeIntervalSinceNow
            if left <= 0 { return }
            try await Task.sleep(for: .seconds(left))
        }
    }

    // 收到 429：记下要停多久（返回要等的秒数）
    @discardableResult
    func limited(retryAfter: String?) -> TimeInterval {
        let now = Date()
        if now.timeIntervalSince(lastHit) > 120 { strikes = 0 }
        lastHit = now
        strikes += 1
        let fallback = min(5 * pow(2, Double(strikes - 1)), 60)
        let wait = retryAfter.flatMap { Double($0) }.map { min(max($0, 1), 120) } ?? fallback
        until = max(until, now.addingTimeInterval(wait))
        return wait
    }

    // 有一个请求成功了：不再翻倍
    func ok() {
        strikes = 0
    }

    // 缩略图同时下载的个数
    private let thumbSlots = 6
    private var thumbBusy = 0
    private var thumbWaiters: [CheckedContinuation<Void, Never>] = []

    func takeThumbSlot() async {
        if thumbBusy < thumbSlots {
            thumbBusy += 1
            return
        }
        await withCheckedContinuation { thumbWaiters.append($0) }
    }

    func giveThumbSlot() {
        if thumbWaiters.isEmpty {
            thumbBusy -= 1
        } else {
            thumbWaiters.removeFirst().resume()
        }
    }
}

// 缩略图：服务端给的签名地址（/thumb/…，6–12 小时有效）。内存里留一份，系统的网络缓存（URLCache）存在磁盘上，
// 来回滑不用重下。格子滑出屏幕时还没轮到的就取消，不发请求；被限流时等一会儿再下，不显示成坏图。
@MainActor
final class ThumbLoader {
    static let shared = ThumbLoader()

    private let memory: NSCache<NSURL, UIImage> = {
        let c = NSCache<NSURL, UIImage>()
        c.countLimit = 600
        return c
    }()

    func cached(_ url: URL) -> UIImage? { memory.object(forKey: url as NSURL) }

    func load(_ url: URL) async -> UIImage? {
        if let hit = cached(url) { return hit }
        // 系统缓存里有就不用排队，也不算请求
        let request = URLRequest(url: url, cachePolicy: .returnCacheDataElseLoad, timeoutInterval: 30)
        if let stored = URLCache.shared.cachedResponse(for: request), let image = UIImage(data: stored.data) {
            memory.setObject(image, forKey: url as NSURL)
            return image
        }
        await RateGate.shared.takeThumbSlot()
        defer { Task { await RateGate.shared.giveThumbSlot() } }
        for _ in 0..<4 {
            if Task.isCancelled { return nil }
            do {
                try await RateGate.shared.wait()
                if Task.isCancelled { return nil }
                let (data, resp) = try await URLSession.shared.data(for: request)
                let http = resp as? HTTPURLResponse
                if http?.statusCode == 429 {
                    await RateGate.shared.limited(retryAfter: http?.value(forHTTPHeaderField: "Retry-After"))
                    continue
                }
                guard http.map({ (200..<300).contains($0.statusCode) }) ?? false, let image = UIImage(data: data) else { return nil }
                await RateGate.shared.ok()
                memory.setObject(image, forKey: url as NSURL)
                return image
            } catch {
                return nil
            }
        }
        return nil
    }
}

// 缩略图格子
struct Thumb: View {
    let item: CloudItem
    @State private var image: UIImage?
    @State private var failed = false

    private var url: URL? { Cloud.shared.absolute(item.thumb ?? item.cover) }

    var body: some View {
        Color.secondary.opacity(0.15)
            .overlay {
                if let image = image ?? url.flatMap({ ThumbLoader.shared.cached($0) }) {
                    Image(uiImage: image).resizable().scaledToFill()
                } else if item.isFolder {
                    Image(systemName: "folder.fill").font(.largeTitle).foregroundStyle(Color.brand)
                } else if failed || url == nil {
                    Image(systemName: item.isVideo ? "video" : "photo").foregroundStyle(.secondary)
                }
            }
            .clipped()
            .overlay(alignment: .bottomLeading) {
                if item.isVideo, let d = item.duration, d > 0 {
                    Text(Duration.seconds(d).formatted(.time(pattern: .minuteSecond)))
                        .font(.caption2.bold()).foregroundStyle(.white).shadow(radius: 2).padding(4)
                }
            }
            // 滑出屏幕时系统取消这个任务：还在排队的就不下了
            .task(id: url) {
                guard let url, image == nil else { return }
                image = await ThumbLoader.shared.load(url)
                if image == nil && !Task.isCancelled { failed = true }
            }
    }
}
