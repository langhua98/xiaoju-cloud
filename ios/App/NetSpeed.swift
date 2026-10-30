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

// 「备份」页的网速小卡片：左边上传（绿）、右边下载（红），各一个大数字和最近 30 秒的小柱状图。
// 颜色和代码改动里新增、删减的一样（GitHub 的 diff 配色，深色、浅色各一套）
struct SpeedView: View {
    @State private var ups: [Double] = [] // 最近 30 秒每秒的速度，最新的在最后
    @State private var downs: [Double] = []

    static let upColor = Color(UIColor { $0.userInterfaceStyle == .dark ? rgb(0x3FB950) : rgb(0x1A7F37) })
    static let downColor = Color(UIColor { $0.userInterfaceStyle == .dark ? rgb(0xF85149) : rgb(0xCF222E) })

    var body: some View {
        HStack(spacing: 10) {
            SpeedTile(title: "上传", icon: "arrow.up", color: Self.upColor, history: ups)
            SpeedTile(title: "下载", icon: "arrow.down", color: Self.downColor, history: downs)
        }
        .task {
            var last = NetMeter.shared.totals
            var lastTime = Date()
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(1))
                let now = NetMeter.shared.totals, time = Date()
                let seconds = max(time.timeIntervalSince(lastTime), 0.1)
                ups = Array((ups + [Double(now.up - last.up) / seconds]).suffix(30))
                downs = Array((downs + [Double(now.down - last.down) / seconds]).suffix(30))
                (last, lastTime) = (now, time)
            }
        }
    }

    // 每秒字节数 → ("850", "KB/s")、("2.3", "MB/s")
    static func text(_ bytesPerSecond: Double) -> (number: String, unit: String) {
        let kb = bytesPerSecond / 1024
        if kb < 1000 { return ("\(Int(kb.rounded()))", "KB/s") }
        return (String(format: "%.1f", kb / 1024), "MB/s")
    }

    private static func rgb(_ hex: Int) -> UIColor {
        UIColor(red: CGFloat(hex >> 16 & 0xFF) / 255, green: CGFloat(hex >> 8 & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }
}

private struct SpeedTile: View {
    let title: String
    let icon: String
    let color: Color
    let history: [Double]

    // 显示最近 3 秒的平均（一个文件传完、下一个还没开始时不会一下跳到 0）
    private var current: Double {
        let recent = history.suffix(3)
        return recent.isEmpty ? 0 : recent.reduce(0, +) / Double(recent.count)
    }

    var body: some View {
        let speed = SpeedView.text(current)
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 5) {
                Image(systemName: icon).font(.caption.weight(.bold))
                    .frame(width: 20, height: 20)
                    .background(color.opacity(0.18), in: Circle())
                Text(title).font(.caption).foregroundStyle(.secondary)
            }
            .foregroundStyle(color)
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Text(speed.number).font(.title3.bold()).monospacedDigit()
                    .contentTransition(.numericText())
                    .animation(.default, value: speed.number)
                Text(speed.unit).font(.caption2).foregroundStyle(.secondary)
            }
            Bars(values: history, color: color).frame(height: 22)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(0.08), in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title) \(speed.number) \(speed.unit)")
    }
}

// 小柱状图：最近 30 秒，每秒一根，按这 30 秒里最快的那秒算高度；没有数据的位置画成一个点高
private struct Bars: View {
    let values: [Double]
    let color: Color

    var body: some View {
        Canvas { context, size in
            let count = 30
            let gap: CGFloat = 2
            let width = (size.width - gap * CGFloat(count - 1)) / CGFloat(count)
            let top = max(values.max() ?? 0, 1)
            let padded = Array(repeating: 0.0, count: max(count - values.count, 0)) + values.suffix(count)
            for (i, value) in padded.enumerated() {
                let height = max(size.height * CGFloat(value / top), 2)
                let rect = CGRect(x: CGFloat(i) * (width + gap), y: size.height - height, width: width, height: height)
                context.fill(Path(roundedRect: rect, cornerRadius: width / 2),
                             with: .color(value > 0 ? color : color.opacity(0.2)))
            }
        }
    }
}
