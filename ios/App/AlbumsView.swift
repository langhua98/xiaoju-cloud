import Photos
import SwiftUI

// 「相册」页：按手机上的相册原样搬到云盘。勾选相册后点上传，每个相册放进云盘「相册」下面同名的文件夹，
// 隐藏相册放进「隐藏相册」。传过的（同一个文件夹里同名同大小）自动跳过，所以随时可以再点一次补传新照片。
// 上传时 App 要开着（会保持亮屏）；切走后正在传的这张传完就停，回来再点继续。
struct AlbumsView: View {
    @State private var albums: [PhoneAlbum] = []
    @State private var selected = Set(UserDefaults.standard.stringArray(forKey: "selectedAlbums") ?? [])
    @StateObject private var uploader = AlbumUploader()

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
                        .disabled(selected.isEmpty)
                    }
                    if !uploader.status.isEmpty {
                        Text(uploader.status).font(.footnote).textSelection(.enabled)
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
    @Published var running = false
    @Published var status = ""
    private var task: Task<Void, Never>?

    private var server: String { Bundle.main.object(forInfoDictionaryKey: "XiaojuAPIURL") as? String ?? "" }

    func stop() { task?.cancel() }

    func start(_ albums: [PhoneAlbum]) {
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
        }
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
        let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: file) }
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // 原图在 iCloud 上时先下载
        try await PHAssetResourceManager.default().writeData(for: resource, toFile: file, options: opts)
        var req = try request("/api/ingest", resource, album)
        req.httpMethod = "POST"
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        req.timeoutInterval = 600
        let (data, resp) = try await URLSession.shared.upload(for: req, fromFile: file)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(code) else {
            throw NSError(domain: "xiaoju", code: code, userInfo: [NSLocalizedDescriptionKey: "HTTP \(code) \(String(decoding: data, as: UTF8.self))"])
        }
        let result = (try? JSONSerialization.jsonObject(with: data.trimmingSpaces()) as? [String: Any])?["result"] as? String
        if result == "error" {
            throw NSError(domain: "xiaoju", code: 0, userInfo: [NSLocalizedDescriptionKey: String(decoding: data, as: UTF8.self)])
        }
        return result != "skipped"
    }
}

private extension Data {
    // 大视频写入时服务端先发空格保持连接，JSON 在最后
    func trimmingSpaces() -> Data {
        Data(String(decoding: self, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines).utf8)
    }
}
