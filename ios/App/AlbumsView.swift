import CryptoKit
import Photos
import SwiftUI

// 「相册」页：按手机上的相册原样搬到云盘。勾选相册后点上传，每个相册放进云盘「相册」下面同名的文件夹，
// 隐藏相册放进「隐藏相册」。传过的（同一个文件夹里同名同大小）自动跳过，所以随时可以再点一次补传新照片。
// 上传时 App 要开着（会保持亮屏）；切走后正在传的这张传完就停，回来再点继续。
struct AlbumsView: View {
    @State private var albums: [PhoneAlbum] = []
    @State private var selected = Set(UserDefaults.standard.stringArray(forKey: "selectedAlbums") ?? [])
    @ObservedObject private var uploader = AlbumUploader.shared
    @ObservedObject private var auto = AutoBackup.shared

    var body: some View {
        NavigationStack {
            List {
                Section {
                    ForEach(albums) { album in
                        Button { toggle(album) } label: {
                            HStack {
                                Image(systemName: selected.contains(album.id) ? "checkmark.circle.fill" : "circle")
                                    .foregroundStyle(selected.contains(album.id) ? Color.accentColor : .secondary)
                                VStack(alignment: .leading) {
                                    Text(album.title).foregroundStyle(.primary)
                                    Text("\(album.count) 项 → 云盘「\(album.target)」").font(.caption).foregroundStyle(.secondary)
                                }
                            }
                        }
                        .disabled(uploader.running)
                    }
                } footer: {
                    Text("每个相册放进云盘「相册」下面同名的文件夹，隐藏相册放进「隐藏相册」。传过的自动跳过，之后再点一次就是补传新照片。上传时请让 App 开着。")
                }
                Section {
                    if uploader.running {
                        Button("停止", role: .destructive) { uploader.stop() }
                    } else {
                        Button("上传选中的 \(selected.count) 个相册") {
                            uploader.start(albums.filter { selected.contains($0.id) })
                        }
                        .disabled(selected.isEmpty || auto.running)
                        if auto.running {
                            Text("「备份」页的「现在备份」正在跑，它会先传隐藏相册，也包含勾选的相册。两个一起传会抢网速，等它跑完或停掉再在这里传。")
                                .font(.footnote).foregroundStyle(.secondary)
                        }
                    }
                    if !uploader.status.isEmpty {
                        Text(uploader.status).font(.footnote).textSelection(.enabled)
                    }
                    if uploader.running && !uploader.detail.isEmpty {
                        Text(uploader.detail).font(.footnote).foregroundStyle(.secondary)
                    }
                }
            }
            .navigationTitle("相册")
            .refreshable { load() }
            .task { load() }
        }
    }

    private func toggle(_ album: PhoneAlbum) {
        if selected.contains(album.id) { selected.remove(album.id) } else { selected.insert(album.id) }
        UserDefaults.standard.set(Array(selected), forKey: "selectedAlbums")
    }

    private func load() {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else { return }
        albums = PhoneAlbum.all()
    }
}

struct PhoneAlbum: Identifiable {
    let collection: PHAssetCollection
    let title: String
    let hidden: Bool
    let count: Int

    var id: String { collection.localIdentifier }
    var target: String { hidden ? "隐藏相册" : "相册/\(title)" }

    func assets() -> PHFetchResult<PHAsset> {
        let options = PHFetchOptions()
        options.includeHiddenAssets = hidden
        options.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: true)]
        return PHAsset.fetchAssets(in: collection, options: options)
    }

    // 隐藏相册在最前，然后是自己建的相册（按名字排）。
    static func all() -> [PhoneAlbum] {
        var out: [PhoneAlbum] = []
        let hiddenOptions = PHFetchOptions()
        hiddenOptions.includeHiddenAssets = true
        PHAssetCollection.fetchAssetCollections(with: .smartAlbum, subtype: .smartAlbumAllHidden, options: nil)
            .enumerateObjects { c, _, _ in
                out.append(PhoneAlbum(collection: c, title: c.localizedTitle ?? "隐藏", hidden: true,
                                      count: PHAsset.fetchAssets(in: c, options: hiddenOptions).count))
            }
        var mine: [PhoneAlbum] = []
        PHAssetCollection.fetchAssetCollections(with: .album, subtype: .any, options: nil)
            .enumerateObjects { c, _, _ in
                let title = c.localizedTitle ?? "未命名"
                mine.append(PhoneAlbum(collection: c, title: title, hidden: false,
                                       count: PHAsset.fetchAssets(in: c, options: nil).count))
            }
        return out + mine.sorted { $0.title.localizedStandardCompare($1.title) == .orderedAscending }
    }
}

@MainActor
final class AlbumUploader: ObservableObject {
    static let shared = AlbumUploader()
    @Published var running = false
    @Published var status = ""
    @Published var detail = "" // 大视频的上传进度
    private var task: Task<Void, Never>?

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    func stop() { task?.cancel() }

    func start(_ albums: [PhoneAlbum]) {
        if let other = UploadGate.enter("相册") {
            status = "「\(other)」正在上传，等它传完再传相册"
            return
        }
        running = true
        UIApplication.shared.isIdleTimerDisabled = true // 传的时候不自动锁屏
        task = Task {
            var uploaded = 0, skipped = 0, failed = 0
            var lastError = ""
            for album in albums {
                let assets = album.assets()
                for i in 0..<assets.count {
                    if Task.isCancelled { break }
                    status = "「\(album.title)」\(i + 1)/\(assets.count)　新传 \(uploaded)，跳过 \(skipped)，失败 \(failed)"
                    let resources = PHAssetResource.assetResources(for: assets.object(at: i))
                        .filter { [.photo, .video, .pairedVideo].contains($0.type) }
                    for resource in resources {
                        if Task.isCancelled { break }
                        do {
                            if try await upload(resource, to: album) { uploaded += 1 } else { skipped += 1 }
                        } catch {
                            failed += 1
                            lastError = "\(resource.originalFilename)：\(error.localizedDescription)"
                        }
                    }
                }
            }
            let head = Task.isCancelled ? "已停止" : "传完了"
            status = "\(head)：新传 \(uploaded)，跳过 \(skipped)（传过的），失败 \(failed)" + (lastError.isEmpty ? "" : "\n最后一个错误：\(lastError)")
            running = false
            UIApplication.shared.isIdleTimerDisabled = false
            UploadGate.leave()
        }
    }

    private func spoolFile(for resource: PHAssetResource) -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("albums")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let key = "\(resource.assetLocalIdentifier)|\(resource.type.rawValue)|\(resource.originalFilename)"
        let name = SHA256.hash(data: Data(key.utf8)).map { String(format: "%02x", $0) }.joined()
        return dir.appendingPathComponent(name)
    }

    private func request(_ path: String, _ resource: PHAssetResource, _ album: PhoneAlbum) throws -> URLRequest {
        guard let url = URL(string: server + path) else { throw URLError(.badURL) }
        var req = URLRequest(url: url)
        req.setValue(encodedName(resource.originalFilename), forHTTPHeaderField: "X-File-Name")
        if album.hidden {
            req.setValue("hidden", forHTTPHeaderField: "X-Xiaoju-Album")
        } else {
            req.setValue(encodedName(album.title), forHTTPHeaderField: "X-Xiaoju-Album-Name")
        }
        return req
    }

    // 传一个资源：服务端已经有了返回 false。先问一声，传过的就不用从 iCloud 下载原图、也不用再发一遍。
    private func upload(_ resource: PHAssetResource, to album: PhoneAlbum) async throws -> Bool {
        if let size = resource.value(forKey: "fileSize") as? Int64, size > 0 {
            var ask = try request("/api/ingest/exists", resource, album)
            ask.setValue(String(size), forHTTPHeaderField: "X-File-Size")
            let (data, _) = try await URLSession.shared.data(for: ask)
            if (try? JSONDecoder().decode([String: String].self, from: data))?["result"] == "exists" {
                return false
            }
        }
        // 断点续传：导出的文件放在固定的地方，断了下次直接接着传，不用再导出
        let file = spoolFile(for: resource)
        if !FileManager.default.fileExists(atPath: file.path) {
            let opts = PHAssetResourceRequestOptions()
            opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
            let part = file.appendingPathExtension("part")
            try? FileManager.default.removeItem(at: part)
            try await PHAssetResourceManager.default().writeData(for: resource, toFile: part, options: opts)
            try FileManager.default.moveItem(at: part, to: file)
        }
        var headers: [String: String] = [:]
        if album.hidden {
            headers["X-Xiaoju-Album"] = "hidden"
        } else {
            headers["X-Xiaoju-Album-Name"] = encodedName(album.title)
        }
        let uploaded = try await ResumableIngest.send(file: file, name: resource.originalFilename, server: server, headers: headers) { done, total in
            if total > 32 << 20 {
                Task { @MainActor in self.detail = "\(resource.originalFilename) \(done >> 20)/\(total >> 20) MB" }
            }
        }
        try? FileManager.default.removeItem(at: file)
        return uploaded
    }
}

// 同一时间只跑一个上传：「现在备份」和「相册」页一起传会抢网速，还会同时传同一个大视频。
// （「现在备份」总是先传隐藏相册，也包含勾选的相册。）
enum UploadGate {
    private static let lock = NSLock()
    private static var owner: String?

    // 拿到了返回 nil；别的任务正在传，返回它的名字
    static func enter(_ name: String) -> String? {
        lock.withLock {
            if let owner { return owner }
            owner = name
            return nil
        }
    }

    static func leave() { lock.withLock { owner = nil } }
}
