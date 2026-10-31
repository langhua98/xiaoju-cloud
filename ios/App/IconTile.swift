import SwiftUI

// 彩色圆角方块 + 白色图标（像系统「设置」里的那种），让「我的」、文件列表、备份列表一眼能分清是什么。
struct IconTile: View {
    let symbol: String
    let color: Color
    var size: CGFloat = 29

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.52, weight: .semibold))
            .foregroundStyle(.white)
            .frame(width: size, height: size)
            .background(
                RoundedRectangle(cornerRadius: size * 0.24, style: .continuous)
                    .fill(color.gradient)
            )
    }
}

// 列表里一行：左边彩色方块，右边文字（替代 Label，Label 的图标会被整个 App 的橘色主题染成同一个颜色）
struct TileLabel: View {
    let title: String
    let symbol: String
    let color: Color
    var titleColor: Color = .primary

    init(_ title: String, symbol: String, color: Color, titleColor: Color = .primary) {
        self.title = title
        self.symbol = symbol
        self.color = color
        self.titleColor = titleColor
    }

    var body: some View {
        Label {
            Text(title).foregroundStyle(titleColor)
        } icon: {
            IconTile(symbol: symbol, color: color)
        }
    }
}

// 文件按类型的图标和颜色：文件夹橘、照片粉、视频紫、压缩包棕、PDF 红、其他蓝
enum FileTile {
    static func symbol(_ item: CloudItem) -> String {
        if item.isFolder { return "folder.fill" }
        switch item.kind {
        case .image: return "photo.fill"
        case .video: return "video.fill"
        case .archive: return "doc.zipper"
        default: return item.ext == "pdf" ? "doc.richtext.fill" : "doc.fill"
        }
    }

    static func color(_ item: CloudItem) -> Color {
        if item.isFolder { return .orange }
        switch item.kind {
        case .image: return .pink
        case .video: return .purple
        case .archive: return .brown
        default: return item.ext == "pdf" ? .red : .blue
        }
    }
}
