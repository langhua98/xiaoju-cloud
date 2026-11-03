import Network
import SwiftUI
import UIKit

// 自动测速：测手机到小橘服务器的延迟、下载、上传（服务端接口见 internal/api/speedtest.go）。
// 什么时候自动测：打开「备份」页时上次结果超过 15 分钟、App 回到前台时同样、换网络（Wi‑Fi ↔ 蜂窝）时。
// 正在备份、下载时不自动测（会互相抢网速，测不准），点卡片可以手动测。
// 蜂窝网络少用流量：下载 2MB、上传 1MB；Wi‑Fi 下载 12MB、上传 4MB。开了「低数据模式」不自动测。
@MainActor
final class SpeedTest: ObservableObject {
    static let shared = SpeedTest()

    struct Result: Codable, Equatable {
        var down: Double // 字节 / 秒
        var up: Double
        var ping: Double // 毫秒
        var at: Date
        var network: String // 「Wi‑Fi」「蜂窝」「有线」「网络」
    }

    enum Phase: String { case ping = "测延迟", down = "测下载", up = "测上传" }

    @Published private(set) var last: Result? = SpeedTest.load()
    @Published private(set) var phase: Phase?
    @Published private(set) var failed = false

    private let monitor = NWPathMonitor()
    private var network: String?
    private var lowData = false // 开了「低数据模式」
    private var started = false

    private init() {}

    // 卡片出现时调用：开始盯网络变化，结果太旧就测一次
    func start() {
        if !started {
            started = true
            monitor.pathUpdateHandler = { [weak self] path in
                Task { @MainActor in self?.pathChanged(path) }
            }
            monitor.start(queue: DispatchQueue(label: "speedtest.path"))
            NotificationCenter.default.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil,
                                                   queue: .main) { [weak self] _ in
                Task { @MainActor in self?.runIfStale() }
            }
        }
        runIfStale()
    }

    private func runIfStale() {
        guard let last else { return run(manual: false) }
        if Date().timeIntervalSince(last.at) > 15 * 60 { run(manual: false) }
    }

    private func pathChanged(_ path: NWPath) {
        guard path.status == .satisfied else { network = nil; return }
        let kind = path.usesInterfaceType(.wifi) ? "Wi‑Fi" : path.usesInterfaceType(.cellular) ? "蜂窝"
            : path.usesInterfaceType(.wiredEthernet) ? "有线" : "网络"
        lowData = path.isConstrained
        let changed = network != nil && network != kind
        network = kind
        // 换了网络：等 3 秒让新网络稳定再测
        if changed {
            Task {
                try? await Task.sleep(for: .seconds(3))
                run(manual: false)
            }
        }
    }

    func run(manual: Bool) {
        guard phase == nil else { return }
        if !manual {
            // 低数据模式不自动测；正在传东西也不测（NetMeter 最近 5 秒有动静）
            if lowData { return }
            if NetMeter.shared.busy { return }
        }
        Task { await measure() }
    }

    private func measure() async {
        let base = Cloud.shared.base
        let small = network == "蜂窝" || lowData
        let (downBytes, upBytes) = small ? (2 << 20, 1 << 20) : (12 << 20, 4 << 20)
        let config = URLSessionConfiguration.ephemeral
        config.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        config.timeoutIntervalForRequest = 20
        let session = URLSession(configuration: config)
        defer { session.finishTasksAndInvalidate() }
        failed = false
        do {
            // 延迟：连发 4 次取最小（第一次要建连接，通常最慢）
            phase = .ping
            var pings: [Double] = []
            for _ in 0..<4 {
                let t = Date()
                try await fetch(session, URLRequest(url: base.appending(path: "/api/speedtest/ping")))
                pings.append(Date().timeIntervalSince(t) * 1000)
            }
            let ping = pings.min() ?? 0

            // 下载、上传：总时间减去一次来回，剩下的算传数据的时间。
            // 网很快时（不到 1.5 秒就传完）数据太少测不准，换 25MB / 20MB 再测一次
            phase = .down
            var (down, took) = try await downTest(session, base, downBytes, ping: ping)
            if !small && took < 1.5 { (down, took) = try await downTest(session, base, 25 << 20, ping: ping) }

            phase = .up
            var up: Double
            (up, took) = try await upTest(session, base, upBytes, ping: ping)
            if !small && took < 1.5 { (up, took) = try await upTest(session, base, 20 << 20, ping: ping) }

            let result = Result(down: down, up: up, ping: ping, at: Date(), network: network ?? "网络")
            last = result
            Self.save(result)
        } catch {
            failed = true
        }
        phase = nil
    }

    // 返回（每秒字节数, 用了几秒）
    private func downTest(_ session: URLSession, _ base: URL, _ bytes: Int, ping: Double) async throws -> (Double, Double) {
        let req = URLRequest(url: base.appending(path: "/api/speedtest/down")
            .appending(queryItems: [URLQueryItem(name: "bytes", value: String(bytes))]))
        let t = Date()
        let got = try await fetch(session, req)
        let took = max(Date().timeIntervalSince(t) - ping / 1000, 0.05)
        return (Double(got) / took, took)
    }

    private func upTest(_ session: URLSession, _ base: URL, _ bytes: Int, ping: Double) async throws -> (Double, Double) {
        var req = URLRequest(url: base.appending(path: "/api/speedtest/up"))
        req.httpMethod = "POST"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        var payload = Data(count: bytes)
        payload.withUnsafeMutableBytes { _ = SecRandomCopyBytes(kSecRandomDefault, bytes, $0.baseAddress!) }
        let t = Date()
        let (_, resp) = try await session.upload(for: req, from: payload)
        try Self.check(resp)
        let took = max(Date().timeIntervalSince(t) - ping / 1000, 0.05)
        return (Double(bytes) / took, took)
    }

    @discardableResult
    private func fetch(_ session: URLSession, _ req: URLRequest) async throws -> Int {
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp)
        return data.count
    }

    private static func check(_ resp: URLResponse) throws {
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(status) else { throw URLError(.badServerResponse) }
    }

    private static let key = "speedtest.last"

    private static func load() -> Result? {
        guard let data = UserDefaults.standard.data(forKey: key) else { return nil }
        return try? JSONDecoder().decode(Result.self, from: data)
    }

    private static func save(_ r: Result) {
        if let data = try? JSONEncoder().encode(r) { UserDefaults.standard.set(data, forKey: key) }
    }
}

// 「备份」页「已完成」右边的小卡片：三行 ↓ 下载、↑ 上传、延迟。
// 平时显示上次测速的结果（顶上写「测速 · 几分钟前」）；有东西在传时自动换成实时网速（顶上绿点「实时」）。
// 点一下重新测。
struct SpeedBadge: View {
    @ObservedObject private var test = SpeedTest.shared
    @State private var live: (up: Double, down: Double)? // 正在传时的实时网速（最近 3 秒平均）

    var body: some View {
        Button { test.run(manual: true) } label: {
            VStack(alignment: .leading, spacing: 5) {
                header
                line("arrow.down", Palette.down, live?.down ?? test.last?.down)
                line("arrow.up", Palette.up, live?.up ?? test.last?.up)
                HStack(spacing: 4) {
                    Image(systemName: "timer").font(.system(size: 10, weight: .bold)).foregroundStyle(Palette.sub)
                        .frame(width: 14)
                    if let ping = test.last?.ping {
                        Text("\(Int(ping.rounded())) ms").font(.caption.weight(.semibold)).monospacedDigit()
                    } else {
                        Text("—").font(.caption).foregroundStyle(Palette.sub)
                    }
                }
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 8)
            .frame(width: 116, alignment: .leading)
            .background(Palette.well, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
            .contentShape(RoundedRectangle(cornerRadius: 14))
        }
        .buttonStyle(.plain)
        .disabled(test.phase != nil)
        .task {
            test.start()
            // 每秒看一次 NetMeter，和下面的上传、下载卡片同一个数
            var last = NetMeter.shared.totals
            var lastTime = Date()
            var ups: [Double] = [], downs: [Double] = []
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                let now = NetMeter.shared.totals, time = Date()
                let seconds = max(time.timeIntervalSince(lastTime), 0.1)
                ups = Array((ups + [Double(now.up - last.up) / seconds]).suffix(3))
                downs = Array((downs + [Double(now.down - last.down) / seconds]).suffix(3))
                (last, lastTime) = (now, time)
                let up = ups.reduce(0, +) / Double(ups.count), down = downs.reduce(0, +) / Double(downs.count)
                withAnimation(.easeInOut(duration: 0.3)) {
                    live = up + down >= 1024 ? (up, down) : nil // 不到 1 KB/s 当作没在传
                }
            }
        }
        .accessibilityLabel(accessibility)
        .accessibilityHint("点一下重新测速")
    }

    @ViewBuilder
    private var header: some View {
        HStack(spacing: 4) {
            if let phase = test.phase {
                ProgressView().controlSize(.mini)
                Text(phase.rawValue)
            } else if test.failed && live == nil {
                Text("测速失败，点重试")
            } else if live != nil {
                Circle().fill(Palette.up).frame(width: 6, height: 6)
                Text("实时")
            } else if let at = test.last?.at {
                TimelineView(.periodic(from: .now, by: 30)) { context in
                    Text("测速 · " + Self.ago(at, now: context.date))
                }
            } else {
                Text("测速")
            }
        }
        .font(.caption2)
        .foregroundStyle(test.failed && test.phase == nil && live == nil ? Palette.down : Palette.sub)
        .lineLimit(1)
        .minimumScaleFactor(0.8)
    }

    private func line(_ icon: String, _ color: Color, _ speed: Double?) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 4) {
            Image(systemName: icon).font(.system(size: 10, weight: .bold)).foregroundStyle(color).frame(width: 14)
            if let speed {
                let t = SpeedView.text(speed)
                Text(t.number).font(.subheadline.bold()).monospacedDigit().layoutPriority(1)
                    .contentTransition(.numericText())
                    .animation(.default, value: t.number)
                Text(t.unit).font(.system(size: 9)).foregroundStyle(Palette.sub)
            } else {
                Text("—").font(.subheadline).foregroundStyle(Palette.sub)
            }
        }
        .lineLimit(1)
        .minimumScaleFactor(0.5) // 字号调大了也不出现「…」，整行缩小
    }

    // 「刚刚」「5 分钟前」「3 小时前」「2 天前」
    static func ago(_ date: Date, now: Date) -> String {
        let s = Int(now.timeIntervalSince(date))
        if s < 60 { return "刚刚" }
        if s < 3600 { return "\(s / 60) 分钟前" }
        if s < 86400 { return "\(s / 3600) 小时前" }
        return "\(s / 86400) 天前"
    }

    private var accessibility: String {
        guard let r = test.last else { return "还没测速" }
        let d = SpeedView.text(live?.down ?? r.down), u = SpeedView.text(live?.up ?? r.up)
        return "下载 \(d.number) \(d.unit)，上传 \(u.number) \(u.unit)，延迟 \(Int(r.ping)) 毫秒"
    }
}
