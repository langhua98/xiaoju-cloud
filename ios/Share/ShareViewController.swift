import SwiftUI
import UIKit
import UniformTypeIdentifiers

// 分享菜单里的「小桔云盘」：在微信、QQ、照片、「文件」里选好照片、视频、文件 → 分享 → 小桔云盘，
// 选放进网盘哪里，开着这个界面一个个传上去（大文件 8MB 一段，断了重试，见 App/ResumableIngest.swift）。
// 传过的服务端按名字 + 大小认出来跳过。分享扩展不能在后台长时间运行，传完前不要关掉这个界面。
final class ShareViewController: UIViewController {
    override func viewDidLoad() {
        super.viewDidLoad()
        let model = ShareModel(context: extensionContext)
        let host = UIHostingController(rootView: ShareView(model: model))
        addChild(host)
        host.view.frame = view.bounds
        host.view.autoresizingMask = [.flexibleWidth, .flexibleHeight]
        view.addSubview(host.view)
        host.didMove(toParent: self)
        model.load()
    }
}

final class ShareModel: ObservableObject {
    enum Target: String, CaseIterable, Identifiable {
        case album = "相册"
        case hidden = "隐藏相册"
        case data = "手机数据/分享"
        var id: String { rawValue }

        var headers: [String: String] {
            switch self {
            case .album: return [:]
            case .hidden: return ["X-Xiaoju-Album": "hidden"]
            case .data: return ["X-Xiaoju-Album": "data", "X-Xiaoju-Album-Name": encodedName("分享")]
            }
        }
    }

    struct Item: Identifiable {
        let id = UUID()
        let file: URL
        let isMedia: Bool
        var sent: Int64 = 0
        var total: Int64
        var state = "" // 空：还没传；“✓” 传完；“已有” 跳过；其他是错误
    }

    @Published var items: [Item] = []
    @Published var target = Target.album
    @Published var loading = true
    @Published var running = false
    @Published var finished = false
    @Published var error = ""

    private weak var context: NSExtensionContext?
    private var job: Task<Void, Never>?
    private let dir = FileManager.default.temporaryDirectory.appendingPathComponent("share-" + UUID().uuidString)

    init(context: NSExtensionContext?) { self.context = context }

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    // 把分享进来的东西拷成临时文件（系统给的地址只在回调里有效）
    func load() {
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let providers = (context?.inputItems as? [NSExtensionItem] ?? []).flatMap { $0.attachments ?? [] }
        Task {
            var loaded: [Item] = []
            for provider in providers {
                if let item = try? await copy(provider) { loaded.append(item) }
            }
            await MainActor.run {
                items = loaded
                // 都是照片、视频时默认放「相册」，有别的文件时默认放「手机数据/分享」
                target = loaded.allSatisfy(\.isMedia) ? .album : .data
                loading = false
                if loaded.isEmpty { error = "没有能上传的文件" }
            }
        }
    }

    private func copy(_ provider: NSItemProvider) async throws -> Item? {
        let types = provider.registeredTypeIdentifiers.compactMap(UTType.init)
        guard let type = types.first(where: { $0.conforms(to: .movie) }) ?? types.first(where: { $0.conforms(to: .image) })
            ?? types.first(where: { $0.conforms(to: .data) && !$0.conforms(to: .url) }) ?? types.first(where: { $0.conforms(to: .fileURL) })
        else { return nil }
        let dest: URL = try await withCheckedThrowingContinuation { cont in
            _ = provider.loadFileRepresentation(forTypeIdentifier: type.identifier) { url, err in
                guard let url else {
                    cont.resume(throwing: err ?? URLError(.cannotOpenFile))
                    return
                }
                var name = url.lastPathComponent
                if (name as NSString).pathExtension.isEmpty || name.hasPrefix(".") {
                    let base = provider.suggestedName ?? "分享 \(Int(Date().timeIntervalSince1970))"
                    name = type.preferredFilenameExtension.map { base + "." + $0 } ?? base
                }
                let sub = self.dir.appendingPathComponent(UUID().uuidString)
                let out = sub.appendingPathComponent(name)
                do {
                    try FileManager.default.createDirectory(at: sub, withIntermediateDirectories: true)
                    try FileManager.default.copyItem(at: url, to: out)
                    cont.resume(returning: out)
                } catch {
                    cont.resume(throwing: error)
                }
            }
        }
        let size = Int64((try? dest.resourceValues(forKeys: [.fileSizeKey]))?.fileSize ?? 0)
        return Item(file: dest, isMedia: type.conforms(to: .image) || type.conforms(to: .movie), total: size)
    }

    func start() {
        running = true
        error = ""
        let target = target, server = server
        job = Task {
            for i in items.indices where items[i].state != "✓" && items[i].state != "已有" {
                if Task.isCancelled { break }
                let item = items[i]
                do {
                    let uploaded = try await ResumableIngest.send(file: item.file, name: item.file.lastPathComponent,
                                                                  server: server, headers: target.headers) { sent, _ in
                        DispatchQueue.main.async { self.items[i].sent = sent }
                    }
                    await MainActor.run {
                        items[i].sent = item.total
                        items[i].state = uploaded ? "✓" : "已有"
                    }
                } catch {
                    await MainActor.run { items[i].state = error.localizedDescription }
                }
            }
            await MainActor.run {
                running = false
                let failed = items.filter { !["✓", "已有"].contains($0.state) }.count
                finished = failed == 0
                if failed > 0 { error = "\(failed) 个没传上去，点「重试」接着传（传过的会跳过）" }
            }
        }
    }

    func close() {
        job?.cancel()
        try? FileManager.default.removeItem(at: dir)
        context?.completeRequest(returningItems: nil)
    }
}

struct ShareView: View {
    @ObservedObject var model: ShareModel

    var body: some View {
        NavigationStack {
            Form {
                if model.loading {
                    Section { ProgressView("正在读取…") }
                } else {
                    Section("放进网盘哪里") {
                        Picker("文件夹", selection: $model.target) {
                            ForEach(ShareModel.Target.allCases) { Text($0.rawValue).tag($0) }
                        }
                        .pickerStyle(.segmented)
                        .disabled(model.running || model.finished)
                    }
                    Section("\(model.items.count) 个") {
                        ForEach(model.items) { item in
                            VStack(alignment: .leading, spacing: 4) {
                                HStack {
                                    Text(item.file.lastPathComponent).font(.footnote).lineLimit(1).truncationMode(.middle)
                                    Spacer()
                                    Text(item.state.isEmpty ? Self.size(item) : item.state)
                                        .font(.caption).foregroundStyle(.secondary).lineLimit(2)
                                }
                                if model.running || !item.state.isEmpty {
                                    ProgressView(value: Double(item.sent), total: Double(max(item.total, 1)))
                                }
                            }
                        }
                    }
                }
                if !model.error.isEmpty {
                    Section { Text(model.error).font(.footnote).foregroundStyle(.red) }
                }
                if model.finished {
                    Section { Text("✅ 都传好了，在网盘的「\(model.target.rawValue)」").font(.footnote) }
                }
            }
            .navigationTitle("小桔云盘")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(model.finished ? "完成" : "取消") { model.close() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    if !model.finished {
                        Button(model.error.isEmpty ? "上传" : "重试") { model.start() }
                            .disabled(model.loading || model.running || model.items.isEmpty)
                    }
                }
            }
        }
    }

    private static func size(_ item: ShareModel.Item) -> String {
        ByteCountFormatter.string(fromByteCount: item.total, countStyle: .file)
    }
}

// 文件名 URL 编码后放进请求头（请求头放不了中文，服务端会解码）。和 App 里的一样
func encodedName(_ name: String) -> String {
    name.addingPercentEncoding(withAllowedCharacters: .alphanumerics.union(CharacterSet(charactersIn: "-._~"))) ?? name
}
