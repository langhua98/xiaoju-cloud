import Photos
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

// 隐藏相册：iPhone 默认给「隐藏」相册上了锁（设置 → App → 照片 →「使用面容 ID」）。开着这个锁时，系统不把隐藏的照片
// 交给任何 App（PhotoKit 拿到的隐藏相册是空的，includeHiddenAssets 也没用），自动备份就看到 0 张，
// 像是「都备份过了」。这里说清楚怎么办，另外可以从系统的照片选择器里手动选（选择器里解锁后能选隐藏照片）传上去。
struct HiddenBackupView: View {
    @State private var count = PhoneAlbum.hiddenAlbum()?.count ?? 0
    @State private var picked: [PhotosPickerItem] = []
    @State private var status = ""
    @State private var busy = false
    @State private var result: (uploaded: Int, skipped: Int, failed: Int)?

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    var body: some View {
        List {
            Section {
                if count > 0 {
                    Text("App 能读到隐藏相册里的 \(count) 项，自动备份会把它们传到云盘「隐藏相册」。")
                } else {
                    Text("App 读到的隐藏相册是 0 项。如果你的隐藏相册里其实有照片，是 iPhone 把它锁起来了：开着「使用面容 ID」时，系统不让任何 App 读隐藏的照片，所以自动备份传不了，还会显示成都备份过了。")
                }
            } header: {
                Text("隐藏相册")
            }
            Section {
                Label("打开 iPhone 的「设置」", systemImage: "1.circle")
                Label("点「App」→「照片」", systemImage: "2.circle")
                Label("关掉「使用面容 ID」", systemImage: "3.circle")
                Label("回到这里，下拉刷新", systemImage: "4.circle")
            } header: {
                Text("让自动备份能传隐藏照片")
            } footer: {
                Text("关掉后，在「照片」App 里看隐藏相册就不用刷脸了。传到云盘的「隐藏相册」还是私密的，要解锁才能看。")
            }
            Section {
                PhotosPicker(selection: $picked, maxSelectionCount: 500, matching: nil,
                             preferredItemEncoding: .current, photoLibrary: .shared()) {
                    Label(busy ? "正在上传…" : "选照片传到「隐藏相册」", systemImage: "lock.rectangle.stack")
                }
                .disabled(busy)
                if !status.isEmpty {
                    Text(status).font(.footnote).foregroundStyle(.secondary)
                }
                if let result {
                    Text("新传 \(result.uploaded)，云盘里已有 \(result.skipped)，失败 \(result.failed)").font(.footnote)
                }
            } header: {
                Text("不想关面容 ID：手动选")
            } footer: {
                Text("在弹出的选择器里点「相簿」→「隐藏」，刷脸解锁后选照片。以后新隐藏的照片要再来选一次。")
            }
        }
        .navigationTitle("隐藏相册")
        .refreshable { count = PhoneAlbum.hiddenAlbum()?.count ?? 0 }
        .onChange(of: picked) { _, items in
            guard !items.isEmpty else { return }
            Task { await upload(items) }
        }
    }

    private func upload(_ items: [PhotosPickerItem]) async {
        busy = true
        result = nil
        var tally = (uploaded: 0, skipped: 0, failed: 0)
        defer {
            busy = false
            picked = []
            result = tally
        }
        for (i, item) in items.enumerated() {
            status = "正在传第 \(i + 1) / \(items.count) 个"
            do {
                guard let file = try await item.loadTransferable(type: PickedFile.self) else {
                    tally.failed += 1
                    continue
                }
                defer { try? FileManager.default.removeItem(at: file.url.deletingLastPathComponent()) }
                if try await send(file.url) { tally.uploaded += 1 } else { tally.skipped += 1 }
            } catch {
                tally.failed += 1
                status = "第 \(i + 1) 个没传上去：\(error.localizedDescription)"
            }
        }
        if tally.failed == 0 { status = "" }
    }

    // 传进云盘「隐藏相册」（大文件分段续传）；被限流时等一会儿再试
    private func send(_ file: URL) async throws -> Bool {
        var attempt = 0
        while true {
            do {
                try await RateGate.shared.wait()
                return try await ResumableIngest.send(file: file, name: file.lastPathComponent, server: server,
                                                      headers: ["X-Xiaoju-Album": "hidden"])
            } catch {
                attempt += 1
                guard attempt < 3, error.localizedDescription.hasPrefix("HTTP 429") else { throw error }
                await RateGate.shared.limited(retryAfter: nil)
            }
        }
    }
}

// 选择器交出来的原文件：系统给的临时文件回调结束就删，复制一份到自己的临时目录（保留文件名）
struct PickedFile: Transferable {
    let url: URL

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(importedContentType: .item) { received in
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            let dest = dir.appendingPathComponent(received.file.lastPathComponent)
            try FileManager.default.copyItem(at: received.file, to: dest)
            return PickedFile(url: dest)
        }
    }
}
