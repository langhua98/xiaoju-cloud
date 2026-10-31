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

// 「备份」页的配色：只有一个主色（橙），其余是略带暖色的灰；上传绿、下载红只用在箭头和小柱状图上，
// 备份完成时圆环变绿。深色、浅色各一套，跟着手机的设置走
enum Palette {
    static let bg = dynamic(dark: 0x0F0E0D, light: 0xF5F2EE)
    static let card = dynamic(dark: 0x1B1917, light: 0xFFFFFF)
    static let well = dynamic(dark: 0x262320, light: 0xF1ECE6) // 卡片里再垫一层的底板（三个数）
    static let sub = dynamic(dark: 0x978D82, light: 0x8A7F73)
    static let track = dynamic(dark: 0x2C2926, light: 0xECE5DC)
    static let accent = dynamic(dark: 0xFF9B42, light: 0xF07E17)
    static let up = dynamic(dark: 0x4CC38A, light: 0x1F9D63)
    static let down = dynamic(dark: 0xF07A6A, light: 0xD9533F)

    private static func dynamic(dark: Int, light: Int) -> Color {
        Color(UIColor { $0.userInterfaceStyle == .dark ? rgb(dark) : rgb(light) })
    }

    private static func rgb(_ hex: Int) -> UIColor {
        UIColor(red: CGFloat(hex >> 16 & 0xFF) / 255, green: CGFloat(hex >> 8 & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
    }
}

// 「备份」页的网速小卡片：左边上传、右边下载，各一个大数字和最近 30 秒的小柱状图；
// 柱状图下面一行：左边是打开 App 以来一共传了（下了）多少，右边是最近 30 秒的峰值
struct SpeedView: View {
    @State private var ups: [Double] = [] // 最近 30 秒每秒的速度，最新的在最后
    @State private var downs: [Double] = []
    @State private var totals = NetMeter.shared.totals

    var body: some View {
        HStack(spacing: 10) {
            SpeedTile(title: "上传", done: "已传", icon: "arrow.up", color: Palette.up, history: ups, total: totals.up)
            SpeedTile(title: "下载", done: "已下", icon: "arrow.down", color: Palette.down, history: downs, total: totals.down)
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
                (last, lastTime, totals) = (now, time, now)
            }
        }
    }

    // 每秒字节数 → ("850", "KB/s")、("2.3", "MB/s")
    static func text(_ bytesPerSecond: Double) -> (number: String, unit: String) {
        let kb = bytesPerSecond / 1024
        if kb < 1000 { return ("\(Int(kb.rounded()))", "KB/s") }
        return (String(format: "%.1f", kb / 1024), "MB/s")
    }

    // 字节数 → 「850 KB」「340 MB」「1.24 GB」
    static func size(_ bytes: Int64) -> String {
        let mb = Double(bytes) / 1_048_576
        if mb < 1 { return "\(bytes >> 10) KB" }
        if mb < 1024 { return "\(Int(mb.rounded())) MB" }
        return String(format: "%.2f GB", mb / 1024)
    }
}

private struct SpeedTile: View {
    let title: String
    let done: String // 「已传」「已下」
    let icon: String
    let color: Color
    let history: [Double]
    let total: Int64

    // 显示最近 3 秒的平均（一个文件传完、下一个还没开始时不会一下跳到 0）
    private var current: Double {
        let recent = history.suffix(3)
        return recent.isEmpty ? 0 : recent.reduce(0, +) / Double(recent.count)
    }

    var body: some View {
        let speed = SpeedView.text(current)
        let peak = history.max() ?? 0
        VStack(alignment: .leading, spacing: 6) {
            // 第一行：箭头 + 当前网速，右边小字「上传」「下载」
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Image(systemName: icon).font(.caption.weight(.bold)).foregroundStyle(color)
                Text(speed.number).font(.title3.bold()).monospacedDigit()
                    .contentTransition(.numericText())
                    .animation(.default, value: speed.number)
                Text(speed.unit).font(.caption2).foregroundStyle(Palette.sub)
                Spacer(minLength: 4)
                Text(title).font(.caption2).foregroundStyle(Palette.sub)
            }
            Bars(values: history, color: color).frame(height: 20)
            // 最后一行：左边一共传了多少，右边最近 30 秒峰值（都没动：空闲）
            HStack(spacing: 4) {
                meta(done, total > 0 ? SpeedView.size(total) : "0 KB")
                Spacer(minLength: 4)
                if peak > 0 {
                    meta("峰值", SpeedView.text(peak).number + " " + SpeedView.text(peak).unit)
                } else {
                    meta("空闲", nil)
                }
            }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Palette.card, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title) \(speed.number) \(speed.unit)")
    }

    // 右边一行小灰字，数字加粗
    private func meta(_ label: String, _ value: String?) -> some View {
        HStack(spacing: 3) {
            Text(label)
            if let value { Text(value).fontWeight(.semibold).foregroundStyle(.primary) }
        }
        .font(.caption2).monospacedDigit().foregroundStyle(Palette.sub).lineLimit(1)
        .minimumScaleFactor(0.8)
    }
}

// 小柱状图：最近 30 秒，每秒一根，按这 30 秒里最快的那秒算高度；没有数据的位置画成一条灰的短柱
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
                             with: .color(value > 0 ? color.opacity(0.85) : Palette.track))
            }
        }
    }
}
