import SwiftUI

// 彩色圆角方块 + 白色图标（像系统「设置」里的那种），让「我的」、文件列表、备份列表一眼能分清是什么。
struct IconTile: View {
    let symbol: String
    let color: Color
    var size: CGFloat = 29
    var glyph: Color = .white

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.52, weight: .semibold))
            .foregroundStyle(glyph)
            .frame(width: size, height: size)
            .background(
                RoundedRectangle(cornerRadius: size * 0.24, style: .continuous)
                    .fill(color)
            )
    }
}

// 列表里一行：左边彩色方块，右边文字（替代 Label，Label 的图标会被整个 App 的橘色主题染成同一个颜色）
struct TileLabel: View {
    let title: String
    let symbol: String
    let color: Color
    var titleColor: Color = .primary
    var chevron = false

    // chevron：按钮（不是 NavigationLink）也画个右箭头，和其他行看起来一样
    init(_ title: String, symbol: String, color: Color, titleColor: Color = .primary, chevron: Bool = false) {
        self.title = title
        self.symbol = symbol
        self.color = color
        self.titleColor = titleColor
        self.chevron = chevron
    }

    var body: some View {
        HStack {
            Label {
                Text(title).foregroundStyle(titleColor)
            } icon: {
                IconTile(symbol: symbol, color: color)
            }
            if chevron {
                Spacer()
                Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(.tertiary)
            }
        }
    }
}

// 「云上小橘」配色规范：品牌鲜橙贯穿全 App，其他颜色只表示文件分类；设置里的图标也从这几个颜色里取
extension Color {
    static let brand = Color(hex: 0xFF7900)
    static let brandHover = Color(hex: 0xE66D00)
    static let filePhoto = Color(hex: 0xFF7900)
    static let fileVideo = Color(hex: 0x8B5CF6)
    static let fileDocument = Color(hex: 0x3B82F6)
    static let fileAudio = Color(hex: 0x10B981)
    static let fileOther = Color(hex: 0x64748B)
    static let fileArchive = Color(hex: 0xEC4899)
    static let danger = Color(hex: 0xEF4444)
    // 文件夹方块的浅橙底（深色模式用 #3A2518）
    static let folderTile = Color(UIColor { $0.userInterfaceStyle == .dark
        ? UIColor(red: 0x3A / 255, green: 0x25 / 255, blue: 0x18 / 255, alpha: 1)
        : UIColor(red: 1, green: 0xF1 / 255, blue: 0xE6 / 255, alpha: 1) })

    init(hex: Int) {
        self.init(red: Double(hex >> 16 & 0xFF) / 255, green: Double(hex >> 8 & 0xFF) / 255, blue: Double(hex & 0xFF) / 255)
    }
}

// 文件按类型的图标和颜色：照片橙、视频紫、文档蓝、音频绿、压缩包玫红、其他石板灰；文件夹是浅橙底配橙色图标
enum FileTile {
    static let audioExts: Set<String> = ["mp3", "m4a", "aac", "wav", "flac", "ogg", "opus", "amr", "wma", "aif", "aiff"]

    static func symbol(_ item: CloudItem) -> String {
        if item.isFolder { return "folder.fill" }
        if audioExts.contains(item.ext) || item.mime.hasPrefix("audio/") { return "music.note" }
        switch item.kind {
        case .image: return "photo.fill"
        case .video: return "video.fill"
        case .archive: return "doc.zipper"
        default: return isDocument(item) ? "doc.text.fill" : "ellipsis"
        }
    }

    static func color(_ item: CloudItem) -> Color {
        if item.isFolder { return .folderTile }
        if audioExts.contains(item.ext) || item.mime.hasPrefix("audio/") { return .fileAudio }
        switch item.kind {
        case .image: return .filePhoto
        case .video: return .fileVideo
        case .archive: return .fileArchive
        default: return isDocument(item) ? .fileDocument : .fileOther
        }
    }

    static func glyph(_ item: CloudItem) -> Color { item.isFolder ? .brand : .white }

    private static func isDocument(_ item: CloudItem) -> Bool {
        item.mime.hasPrefix("text/") || ["pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "rtf", "csv",
                                         "pages", "numbers", "key", "epub", "odt", "ods", "odp"].contains(item.ext)
    }
}
