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

    // fromICloud：备份时先从苹果 iCloud 取原图（开了「优化 iPhone 储存空间」的照片）
    func received(_ bytes: Int64, fromICloud: Bool = false) {
        guard bytes > 0 else { return }
        lock.withLock {
            down += bytes
            if fromICloud { iCloudAt = Date() }
        }
    }

    private var iCloudAt: Date?
    // 最近 3 秒有没有在从 iCloud 取原图：下载卡片右上角显示「访问 iCloud」
    var iCloudActive: Bool { lock.withLock { iCloudAt.map { Date().timeIntervalSince($0) < 3 } ?? false } }

    var totals: (up: Int64, down: Int64) { lock.withLock { (up, down) } }
}

// 「备份」页的配色：只有一个主色（橙），其余是略带暖色的灰；上传绿、下载红只用在箭头和小曲线上，
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

// 「备份」页的网速小卡片：左边上传、右边下载，各一个大数字和最近 30 秒的小曲线；
// 曲线下面两行：打开 App 以来一共传了（下了）多少、最近 30 秒的峰值
struct SpeedView: View {
    @State private var ups: [Double] = [] // 最近 30 秒每秒的速度，最新的在最后
    @State private var downs: [Double] = []
    @State private var totals = NetMeter.shared.totals
    @State private var iCloud = false

    var body: some View {
        HStack(spacing: 10) {
            SpeedTile(title: "上传", done: "已传", icon: "arrow.up", color: Palette.up, history: ups, total: totals.up)
            SpeedTile(title: "下载", done: "已下", icon: "arrow.down", color: Palette.down, history: downs, total: totals.down,
                      badge: iCloud ? "访问 iCloud" : nil)
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
                iCloud = NetMeter.shared.iCloudActive
            }
        }
    }

    // 每秒字节数 → ("850", "KB/s")、("2.3", "MB/s")
    static func text(_ bytesPerSecond: Double) -> (number: String, unit: String) {
        let kb = bytesPerSecond / 1024
        // 数字最多 3 位：太大就换单位（KB → MB → GB），字数不会变长
        if kb < 1000 { return ("\(Int(kb.rounded()))", "KB/s") }
        let mb = kb / 1024
        if mb < 100 { return (String(format: "%.1f", mb), "MB/s") }
        if mb < 1000 { return ("\(Int(mb.rounded()))", "MB/s") }
        return (String(format: "%.1f", mb / 1024), "GB/s")
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
    var badge: String? = nil // 右上角的小提示，比如「访问 iCloud」
    // 曲线的刻度：每秒往最新的最大值靠 35%，突然来个高峰不会一下把整条线压扁
    @State private var scale: Double = 0

    // 显示最近 3 秒的平均（一个文件传完、下一个还没开始时不会一下跳到 0）
    private var current: Double {
        let recent = history.suffix(3)
        return recent.isEmpty ? 0 : recent.reduce(0, +) / Double(recent.count)
    }

    var body: some View {
        let speed = SpeedView.text(current)
        let peak = history.max() ?? 0
        let active = current >= 1024 // 不到 1 KB/s 当作没在传
        VStack(alignment: .leading, spacing: 6) {
            // 第一行：加大的箭头（带淡色底）+「上传」「下载」
            HStack(spacing: 6) {
                // 没在传时只把箭头调暗（变灰），卡片本身不变
                Image(systemName: icon).font(.footnote.weight(.bold)).foregroundStyle(active ? color : Palette.sub)
                    .frame(width: 24, height: 24)
                    .background((active ? color : Palette.sub).opacity(0.16), in: RoundedRectangle(cornerRadius: 7, style: .continuous))
                    .opacity(active ? 1 : 0.6)
                    .animation(.easeInOut(duration: 0.4), value: active)
                Text(title).font(.footnote.weight(.semibold)).fixedSize()
                Spacer(minLength: 4)
                if let badge {
                    Label(badge, systemImage: "icloud")
                        .font(.caption2).foregroundStyle(Palette.sub)
                        .lineLimit(1).minimumScaleFactor(0.8)
                        .transition(.opacity)
                }
            }
            .animation(.easeInOut(duration: 0.3), value: badge)
            // 第二行：当前网速；没在传时是灰色的「—」
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                if active {
                    Text(speed.number).font(.title2.bold()).monospacedDigit()
                        .contentTransition(.numericText())
                        .animation(.default, value: speed.number)
                    Text(speed.unit).font(.caption2).foregroundStyle(Palette.sub)
                } else {
                    Text("—").font(.title2.weight(.medium)).foregroundStyle(Palette.sub)
                }
            }
            Curve(values: history, scale: scale, color: color).frame(height: 22)
            Divider().overlay(Palette.track)
            // 最后两行各放一个信息：一共传了多少、最近 30 秒峰值（都没动：空闲）
            row(done, total > 0 ? SpeedView.size(total) : "0 KB")
            row("峰值", peak > 0 ? SpeedView.text(peak).number + " " + SpeedView.text(peak).unit : "空闲", dim: peak == 0)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Palette.card, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .onChange(of: history) {
            let target = max(history.max() ?? 0, 1)
            scale = scale == 0 ? target : scale + (target - scale) * 0.35
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(title) \(speed.number) \(speed.unit)")
    }

    // 左边灰色名字，右边加粗数字
    private func row(_ label: String, _ value: String, dim: Bool = false) -> some View {
        HStack {
            Text(label).foregroundStyle(Palette.sub)
            Spacer(minLength: 4)
            Text(value).fontWeight(dim ? .regular : .semibold).foregroundStyle(dim ? Palette.sub : .primary)
        }
        .font(.caption2).monospacedDigit().lineLimit(1).minimumScaleFactor(0.8)
    }
}

// 面积曲线：最近 30 秒，每秒一个点，新的从右边进来；下面一层淡渐变，最右端小圆点是「现在」，淡虚线是这 30 秒的峰值
private struct Curve: View {
    let values: [Double]
    let scale: Double
    let color: Color

    var body: some View {
        Canvas { context, size in
            let count = 30
            let padded = Array(repeating: 0.0, count: max(count - values.count, 0)) + values.suffix(count)
            let top = max(scale, 1)
            let inset: CGFloat = 2 // 留出线宽，顶上和底下不被切掉
            func y(_ v: Double) -> CGFloat { size.height - inset - (size.height - inset * 2) * CGFloat(min(v / top, 1)) }
            let points = padded.enumerated().map { CGPoint(x: size.width * CGFloat($0.offset) / CGFloat(count - 1), y: y($0.element)) }
            guard (padded.max() ?? 0) > 0 else {
                // 30 秒都没动：贴底一条灰线
                var flat = Path()
                flat.move(to: CGPoint(x: 0, y: size.height - inset))
                flat.addLine(to: CGPoint(x: size.width, y: size.height - inset))
                context.stroke(flat, with: .color(Palette.track), lineWidth: 2)
                return
            }
            var line = Path()
            line.move(to: points[0])
            for i in 1..<points.count {
                let (p0, p1) = (points[i - 1], points[i])
                let mid = (p0.x + p1.x) / 2
                line.addCurve(to: p1, control1: CGPoint(x: mid, y: p0.y), control2: CGPoint(x: mid, y: p1.y))
            }
            var area = line
            area.addLine(to: CGPoint(x: size.width, y: size.height))
            area.addLine(to: CGPoint(x: 0, y: size.height))
            area.closeSubpath()
            context.fill(area, with: .linearGradient(Gradient(colors: [color.opacity(0.35), color.opacity(0)]),
                                                     startPoint: .zero, endPoint: CGPoint(x: 0, y: size.height)))
            context.stroke(line, with: .color(color), style: StrokeStyle(lineWidth: 2, lineCap: .round, lineJoin: .round))
            if let last = points.last {
                let dot = CGRect(x: last.x - 6, y: last.y - 3, width: 6, height: 6)
                context.fill(Path(ellipseIn: dot), with: .color(color))
            }
            let peakY = y(padded.max() ?? 0)
            var dash = Path()
            dash.move(to: CGPoint(x: 0, y: peakY))
            dash.addLine(to: CGPoint(x: size.width, y: peakY))
            context.stroke(dash, with: .color(color.opacity(0.45)), style: StrokeStyle(lineWidth: 1, dash: [4, 4]))
        }
    }
}
