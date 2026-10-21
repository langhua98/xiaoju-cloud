import Photos
import SwiftUI

// 「备份」→「备份哪些照片」：全部照片总是备份；手机上的相册打开开关后，在云盘「相册」下面建同名文件夹，
// 之后加进这个相册的新照片也跟着备份（自动备份每一轮都会补传）。隐藏相册放进「隐藏相册」，云盘里要解锁才能看。
struct BackupAlbumsView: View {
    @State private var albums: [PhoneAlbum] = []
    @State private var selected = Set(UserDefaults.standard.stringArray(forKey: AutoBackup.selectedAlbums) ?? [])
    @State private var tallies: [String: AutoBackup.Tally] = [:] // 相册 id → 已备份几张
    @State private var all: AutoBackup.Tally?
    @AppStorage(AutoBackup.backupHidden) private var hidden = true

    var body: some View {
        List {
            Section {
                HStack {
                    VStack(alignment: .leading) {
                        Text("全部照片")
                        if let all {
                            Text("已备份 \(all.done) / \(all.total) → 云盘「相册」").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    Spacer()
                    Text("总是备份").font(.footnote).foregroundStyle(.secondary)
                }
            }
            let hiddenAlbums = albums.filter(\.hidden)
            if !hiddenAlbums.isEmpty {
                Section {
                    ForEach(hiddenAlbums) { album in
                        Toggle(isOn: $hidden) {
                            row(album, on: hidden, icon: "lock.fill")
                        }
                    }
                } header: {
                    Text("私密")
                } footer: {
                    Text("放进云盘「隐藏相册」，在云盘里要解锁才能看。")
                }
            }
            Section {
                ForEach(albums.filter { !$0.hidden }) { album in
                    Toggle(isOn: Binding(get: { selected.contains(album.id) }, set: { set(album, $0) })) {
                        row(album, on: selected.contains(album.id), icon: nil)
                    }
                }
            } header: {
                Text("单独存成相册")
            } footer: {
                Text("打开的相册会在云盘「相册」下面建同名文件夹（照片在「全部照片」里也有一份）。之后加进这个相册的新照片，下次备份时一起传。")
            }
        }
        .navigationTitle("备份哪些照片")
        .refreshable { await load() }
        .task { await load() }
    }

    private func row(_ album: PhoneAlbum, on: Bool, icon: String?) -> some View {
        HStack {
            if let icon { Image(systemName: icon).foregroundStyle(.secondary) }
            VStack(alignment: .leading) {
                Text(album.title)
                if on, let t = tallies[album.id] {
                    Text("已备份 \(t.done) / \(t.total)").font(.caption).foregroundStyle(.secondary)
                } else {
                    Text("\(album.count) 项").font(.caption).foregroundStyle(.secondary)
                }
            }
        }
    }

    private func set(_ album: PhoneAlbum, _ on: Bool) {
        if on { selected.insert(album.id) } else { selected.remove(album.id) }
        UserDefaults.standard.set(Array(selected), forKey: AutoBackup.selectedAlbums)
    }

    private func load() async {
        guard PHPhotoLibrary.authorizationStatus(for: .readWrite) == .authorized else { return }
        let (list, counts, whole) = await Task.detached(priority: .userInitiated) {
            await AutoBackup.shared.reconcileAll()
            let list = PhoneAlbum.all()
            var counts: [String: AutoBackup.Tally] = [:]
            for album in list {
                counts[album.id] = AutoBackup.shared.tally(target: album.backupKey, assets: album.assets())
            }
            return (list, counts, AutoBackup.shared.tally(target: "all", assets: AutoBackup.allPhotos()))
        }.value
        albums = list
        tallies = counts
        all = whole
    }
}

struct PhoneAlbum: Identifiable {
    let collection: PHAssetCollection
    let title: String
    let hidden: Bool
    let count: Int

    var id: String { collection.localIdentifier }
    // 自动备份记录传过的照片时用的名字（AutoBackup.backupPhotos）
    var backupKey: String { hidden ? "hidden" : "album:\(title)" }

    func assets() -> PHFetchResult<PHAsset> {
        let options = PHFetchOptions()
        options.includeHiddenAssets = hidden
        options.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: true)]
        return PHAsset.fetchAssets(in: collection, options: options)
    }

    // 系统的「隐藏」相册
    static func hiddenAlbum() -> PhoneAlbum? {
        guard let c = PHAssetCollection.fetchAssetCollections(with: .smartAlbum, subtype: .smartAlbumAllHidden, options: nil).firstObject
        else { return nil }
        let options = PHFetchOptions()
        options.includeHiddenAssets = true
        return PhoneAlbum(collection: c, title: c.localizedTitle ?? "隐藏", hidden: true,
                          count: PHAsset.fetchAssets(in: c, options: options).count)
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
