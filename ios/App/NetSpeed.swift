import SwiftUI
import UIKit

// 网速：上传（交给服务端的字节）、下载（从 iCloud 取原图、从云盘恢复到手机）各记一个累计数，
// 界面上每秒取一次，和上一次比算出每秒多少。
final class NetMeter {
    static let shared = NetMeter()
    private let lock = NSLock()
    private var up: Int64 = 0
    private var down: Int64 = 0

    func sent(_ bytes: Int64) {
        guard bytes > 0 else { return }
        lock.withLock { up += bytes }
    }

    func received(_ bytes: Int64) {
        guard bytes > 0 else { return }
        lock.withLock { down += bytes }
    }

    var totals: (up: Int64, down: Int64) { lock.withLock { (up, down) } }
}

// 「↑ 2.3 MB/s  ↓ 850 KB/s」：上传绿、下载红，和代码改动里新增、删减的颜色一样（GitHub 的 diff 配色，深色、浅色各一套）
struct SpeedView: View {
    @State private var up: Double = 0
    @State private var down: Double = 0

    static let upColor = Color(UIColor { $0.userInterfaceStyle == .dark ? rgb(0x3FB950) : rgb(0x1A7F37) })
    static let downColor = Color(UIColor { $0.userInterfaceStyle == .dark ? rgb(0xF85149) : rgb(0xCF222E) })

    var body: some View {
        HStack(spacing: 18) {
            Label(Self.text(up), systemImage: "arrow.up").foregroundStyle(Self.upColor)
            Label(Self.text(down), systemImage: "arrow.down").foregroundStyle(Self.downColor)
        }
        .font(.footnote.weight(.medium).monospacedDigit())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("上传 \(Self.text(up))，下载 \(Self.text(down))")
        .task {
            // 每秒取一次，显示最近 3 秒的平均（一个文件传完、下一个还没开始时不会一下跳到 0）
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
                up = ups.reduce(0, +) / Double(ups.count)
                down = downs.reduce(0, +) / Double(downs.count)
            }
        }
    }

    // 每秒字节数 → 「850 KB/s」「2.3 MB/s」
    static func text(_ bytesPerSecond: Double) -> String {
        let kb = bytesPerSecond / 1024
        if kb < 1000 { return "\(Int(kb.rounded())) KB/s" }
        return String(format: "%.1f MB/s", kb / 1024)
    }

    private static func rgb(_ hex: Int) -> UIColor {
        UIColor(red: CGFloat(hex >> 16 & 0xFF) / 255, green: CGFloat(hex >> 8 & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }
}
