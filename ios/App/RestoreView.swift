import Contacts
import Photos
import SwiftUI

// 恢复到手机：换新手机或误删后，把网盘里的「相册」「隐藏相册」存回照片、「手机数据/通讯录」最新的一份导回通讯录。
// App 自己不能登录（刷脸要关联域名），网页上点「恢复到手机」刷脸后换一个 24 小时的恢复凭证，
// 跳到 xiaojubackup://restore?server=…&token=… 交给这里（服务端见 internal/api/restore.go）。
// 手机上已经有的（文件名 + 大小一样）跳过，所以中断了再点一次就接着恢复，不会重复。
final class Restorer: ObservableObject {
    static let shared = Restorer()

    @Published var show = false
    @Published private(set) var running = false
    @Published private(set) var status = ""
    @Published private(set) var progress: (done: Int, total: Int) = (0, 0)
    @Published private(set) var server = UserDefaults.standard.string(forKey: "restore.server") ?? ""
    @Published private(set) var token = UserDefaults.standard.string(forKey: "restore.token") ?? ""

    private var job: Task<Void, Never>?

    var hasToken: Bool { !token.isEmpty && !server.isEmpty }

    func open(_ url: URL) {
        let query = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems ?? []
        let value = { (name: String) in query.first { $0.name == name }?.value ?? "" }
        if !value("server").isEmpty, !value("token").isEmpty {
            server = value("server").trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            token = value("token")
            UserDefaults.standard.set(server, forKey: "restore.server")
            UserDefaults.standard.set(token, forKey: "restore.token")
            status = "已拿到恢复凭证（24 小时内有效），选要恢复的内容。"
        }
        show = true
    }

    func stop() { job?.cancel() }

    func start(photos: Bool, hidden: Bool, contacts: Bool) {
        guard job == nil else { return }
        running = true
        job = Task.detached(priority: .userInitiated) { [self] in
            var lines: [String] = []
            do {
                if photos || hidden {
                    lines.append(try await restorePhotos(normal: photos, hidden: hidden))
                }
                if contacts, !Task.isCancelled {
                    lines.append(try await restoreContacts())
                }
                if Task.isCancelled { lines.append("已停止：再点一次会接着恢复。") }
            } catch {
                lines.append("出错了：\(error.localizedDescription)")
            }
            await MainActor.run {
                status = lines.joined(separator: "\n")
                running = false
                job = nil
            }
        }
    }

    private func setStatus(_ text: String, done: Int, total: Int) {
        DispatchQueue.main.async {
            self.status = text
            self.progress = (done, total)
        }
    }

    // MARK: 网络

    struct Item: Decodable {
        let id: String
        let path: String
        let size: Int64
        let mime: String
        var name: String { String(path.split(separator: "/").last ?? "") }
        var folder: String { path.split(separator: "/").dropLast().joined(separator: "/") }
    }

    private func request(_ path: String) throws -> URLRequest {
        guard let url = URL(string: server + path) else { throw URLError(.badURL) }
        var req = URLRequest(url: url)
        req.setValue(token, forHTTPHeaderField: "X-Xiaoju-Restore")
        req.timeoutInterval = 300
        return req
    }

    private func list(_ folder: String) async throws -> [Item] {
        let q = folder.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? folder
        let (data, resp) = try await URLSession.shared.data(for: request("/api/restore/list?folder=\(q)"))
        try check(resp, data)
        struct Out: Decodable { let items: [Item] }
        return try JSONDecoder().decode(Out.self, from: data).items
    }

    // 下载到临时文件（文件名保留，PhotoKit 按扩展名认格式）
    private func download(_ item: Item) async throws -> URL {
        let (tmp, resp) = try await URLSession.shared.download(for: request("/api/restore/files/\(item.id)"))
        try check(resp, nil)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let dest = dir.appendingPathComponent(item.name)
        try FileManager.default.moveItem(at: tmp, to: dest)
        NetMeter.shared.received((try? FileManager.default.attributesOfItem(atPath: dest.path)[.size] as? Int64) ?? 0)
        return dest
    }

    private func check(_ resp: URLResponse, _ data: Data?) throws {
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 401 {
            throw NSError(domain: "xiaoju", code: code, userInfo: [NSLocalizedDescriptionKey:
                "恢复凭证过期了：回网盘网页菜单里再点一次「恢复到手机」"])
        }
        guard (200..<300).contains(code) else {
            throw NSError(domain: "xiaoju", code: code, userInfo: [NSLocalizedDescriptionKey:
                "HTTP \(code) \(String(decoding: data ?? Data(), as: UTF8.self))"])
        }
    }

    // MARK: 照片

    private func restorePhotos(normal: Bool, hidden: Bool) async throws -> String {
        if PHPhotoLibrary.authorizationStatus(for: .readWrite) != .authorized {
            _ = await PHPhotoLibrary.requestAuthorization(for: .readWrite)
        }
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else { return "照片：没有相册权限" }
        setStatus("正在看手机上已经有哪些照片…", done: 0, total: 0)
        let have = Self.existingResources()
        var items: [(Item, Bool)] = []
        if normal { items += try await list(AlbumFolder).map { ($0, false) } }
        if hidden { items += try await list(HiddenAlbumFolder).map { ($0, true) } }
        items = items.filter { Self.isMedia($0.0) }

        // 实况照片：同一个文件夹里同名的照片 + .MOV（备份时是分开传的）合成一张
        var paired: [String: Item] = [:]
        for (item, _) in items where Self.isVideo(item) { paired[Self.stem(item)] = item }
        var jobs: [(photo: Item, video: Item?, hidden: Bool)] = []
        var usedVideos = Set<String>()
        for (item, isHidden) in items where !Self.isVideo(item) {
            let video = paired[Self.stem(item)]
            if let video { usedVideos.insert(video.id) }
            jobs.append((item, video, isHidden))
        }
        for (item, isHidden) in items where Self.isVideo(item) && !usedVideos.contains(item.id) {
            jobs.append((item, nil, isHidden))
        }
        jobs.removeAll { have.contains("\($0.photo.name)|\($0.photo.size)") }

        var saved = 0, failed = 0
        var lastError = ""
        var albums: [String: PHAssetCollection] = [:]
        for (i, job) in jobs.enumerated() {
            if Task.isCancelled { break }
            setStatus("正在恢复照片 \(i + 1)/\(jobs.count)：\(job.photo.name)", done: i, total: jobs.count)
            do {
                let photo = try await download(job.photo)
                var video: URL?
                if let v = job.video { video = try await download(v) }
                defer {
                    try? FileManager.default.removeItem(at: photo.deletingLastPathComponent())
                    if let video { try? FileManager.default.removeItem(at: video.deletingLastPathComponent()) }
                }
                // 「相册/旅行/…」放回手机上叫「旅行」的相簿（没有就建一个）
                let parts = job.photo.folder.split(separator: "/")
                let album = parts.count > 1 ? String(parts[1]) : nil
                if let album, albums[album] == nil { albums[album] = try await Self.album(named: album) }
                do {
                    try await Self.save(photo: photo, video: video, hidden: job.hidden, album: album.flatMap { albums[$0] })
                } catch _ where video != nil { // 合不成实况照片：只存照片
                    try await Self.save(photo: photo, video: nil, hidden: job.hidden, album: album.flatMap { albums[$0] })
                }
                saved += 1
            } catch {
                failed += 1
                lastError = "\(job.photo.name)：\(error.localizedDescription)"
                if (error as NSError).code == 401 { throw error }
            }
        }
        setStatus("", done: jobs.count, total: jobs.count)
        return "照片：存回 \(saved) 个，手机上已有跳过 \(items.count - usedVideos.count - jobs.count) 个，失败 \(failed) 个"
            + (lastError.isEmpty ? "" : "\n最后一个错误：\(lastError)")
    }

    private static func save(photo: URL, video: URL?, hidden: Bool, album: PHAssetCollection?) async throws {
        try await PHPhotoLibrary.shared().performChanges {
            let req = PHAssetCreationRequest.forAsset()
            let opts = PHAssetResourceCreationOptions()
            opts.originalFilename = photo.lastPathComponent
            req.addResource(with: isVideo(photo.lastPathComponent) ? .video : .photo, fileURL: photo, options: opts)
            if let video {
                let vopts = PHAssetResourceCreationOptions()
                vopts.originalFilename = video.lastPathComponent
                req.addResource(with: .pairedVideo, fileURL: video, options: vopts)
            }
            req.isHidden = hidden
            if let album, let placeholder = req.placeholderForCreatedAsset {
                PHAssetCollectionChangeRequest(for: album)?.addAssets([placeholder] as NSArray)
            }
        }
    }

    private static func album(named name: String) async throws -> PHAssetCollection? {
        let opts = PHFetchOptions()
        opts.predicate = NSPredicate(format: "localizedTitle == %@", name)
        if let found = PHAssetCollection.fetchAssetCollections(with: .album, subtype: .albumRegular, options: opts).firstObject {
            return found
        }
        try await PHPhotoLibrary.shared().performChanges {
            _ = PHAssetCollectionChangeRequest.creationRequestForAssetCollection(withTitle: name)
        }
        return PHAssetCollection.fetchAssetCollections(with: .album, subtype: .albumRegular, options: opts).firstObject
    }

    // 手机上已有的照片、视频：“文件名|大小”（包括隐藏的）
    private static func existingResources() -> Set<String> {
        let opts = PHFetchOptions()
        opts.includeHiddenAssets = true
        let assets = PHAsset.fetchAssets(with: opts)
        var out = Set<String>()
        assets.enumerateObjects { asset, _, _ in
            for r in PHAssetResource.assetResources(for: asset) where [.photo, .video].contains(r.type) {
                out.insert("\(r.originalFilename)|\(r.value(forKey: "fileSize") as? Int64 ?? 0)")
            }
        }
        return out
    }

    private static let videoExts: Set<String> = ["mov", "mp4", "m4v", "3gp", "avi", "mkv"]
    private static let photoExts: Set<String> = ["heic", "heif", "jpg", "jpeg", "png", "gif", "dng", "tif", "tiff", "webp", "bmp"]
    private static func ext(_ name: String) -> String { (name as NSString).pathExtension.lowercased() }
    private static func isVideo(_ name: String) -> Bool { videoExts.contains(ext(name)) }
    private static func isVideo(_ item: Item) -> Bool { item.mime.hasPrefix("video/") || isVideo(item.name) }
    private static func isMedia(_ item: Item) -> Bool {
        item.mime.hasPrefix("image/") || isVideo(item) || photoExts.contains(ext(item.name))
    }
    private static func stem(_ item: Item) -> String {
        item.folder + "/" + (item.name as NSString).deletingPathExtension.lowercased()
    }

    // MARK: 通讯录

    private func restoreContacts() async throws -> String {
        let store = CNContactStore()
        if CNContactStore.authorizationStatus(for: .contacts) == .notDetermined {
            _ = try? await store.requestAccess(for: .contacts)
        }
        guard CNContactStore.authorizationStatus(for: .contacts) == .authorized else { return "通讯录：没有通讯录权限" }
        setStatus("正在找最新的通讯录备份…", done: 0, total: 0)
        // 「手机数据/通讯录/通讯录 2026-10-21.vcf」：文件名里的日期排序，取最新一份
        let backups = try await list(PhoneDataFolder).filter {
            $0.folder == "\(PhoneDataFolder)/通讯录" && Self.ext($0.name) == "vcf"
        }
        guard let latest = backups.max(by: { $0.name < $1.name }) else { return "通讯录：云盘里还没有通讯录备份" }
        let file = try await download(latest)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let contacts = try CNContactVCardSerialization.contacts(with: Data(contentsOf: file))

        // 已有的联系人（名字 + 电话一样）跳过
        let keys = [CNContactGivenNameKey, CNContactFamilyNameKey, CNContactOrganizationNameKey, CNContactPhoneNumbersKey] as [CNKeyDescriptor]
        var have = Set<String>()
        try store.enumerateContacts(with: CNContactFetchRequest(keysToFetch: keys)) { c, _ in have.insert(Self.contactKey(c)) }
        let save = CNSaveRequest()
        var added = 0
        for c in contacts where !have.contains(Self.contactKey(c)) {
            guard let copy = c.mutableCopy() as? CNMutableContact else { continue }
            save.add(copy, toContainerWithIdentifier: nil)
            have.insert(Self.contactKey(c))
            added += 1
        }
        if added > 0 { try store.execute(save) }
        return "通讯录（\(latest.name)）：导回 \(added) 个，手机上已有跳过 \(contacts.count - added) 个"
    }

    private static func contactKey(_ c: CNContact) -> String {
        let phones = c.phoneNumbers.map { $0.value.stringValue.filter(\.isNumber) }.sorted()
        return [c.familyName, c.givenName, c.organizationName].joined(separator: "|") + "|" + phones.joined(separator: ",")
    }
}

// 网盘里的根目录文件夹名，和服务端 internal/api 里的一样
let AlbumFolder = "相册"
let HiddenAlbumFolder = "隐藏相册"
let PhoneDataFolder = "手机数据"

struct RestoreView: View {
    @ObservedObject private var restorer = Restorer.shared
    @Environment(\.dismiss) private var dismiss
    @State private var photos = true
    @State private var hidden = true
    @State private var contacts = true
    @State private var webOpened = false

    var body: some View {
        NavigationStack {
            Form {
                if !restorer.hasToken {
                    Section {
                        Text("先到网盘网页（「云盘」页）右上角菜单里点「恢复到手机」，验证后会跳回这里。")
                    }
                } else {
                    Section {
                        Toggle("相册（存回照片）", isOn: $photos)
                        Toggle("隐藏相册（存回照片的「已隐藏」）", isOn: $hidden)
                        Toggle("通讯录（导入最新的一份）", isOn: $contacts)
                    } header: {
                        Text("恢复哪些")
                    } footer: {
                        Text("手机上已经有的（文件名、大小一样）会跳过；中断了再点一次接着恢复。「相册/旅行」里的照片放回叫「旅行」的相簿。日历、提醒事项和同步的文件夹在网盘「手机数据」里，需要时在网页上下载打开。")
                    }
                    Section {
                        if restorer.running {
                            Button("停止", role: .destructive) { restorer.stop() }
                        } else {
                            Button("开始恢复") { restorer.start(photos: photos, hidden: hidden, contacts: contacts) }
                                .disabled(!photos && !hidden && !contacts)
                        }
                        if restorer.running, restorer.progress.total > 0 {
                            ProgressView(value: Double(restorer.progress.done), total: Double(restorer.progress.total))
                        }
                    }
                }
                if !restorer.status.isEmpty {
                    Section("状态") { Text(restorer.status).font(.footnote).textSelection(.enabled) }
                }
            }
            .navigationTitle("恢复到手机")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("关闭") { dismiss() } }
            }
        }
        .interactiveDismissDisabled(restorer.running)
    }
}
