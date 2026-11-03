import SwiftUI

@main
struct XiaojuPushApp: App {
    @StateObject private var store = Store()

    var body: some Scene {
        WindowGroup {
            HomeView().environmentObject(store)
        }
    }
}

// 首页：推送任务列表。点一个任务 → 看改了什么 → 提交并推送。
struct HomeView: View {
    @EnvironmentObject var store: Store
    @State private var editing: Mapping?
    @State private var showSettings = false

    var body: some View {
        NavigationStack {
            List {
                if !store.ready {
                    Section {
                        Button { showSettings = true } label: {
                            Label("先填小橘云盘令牌和 GitHub 令牌", systemImage: "key.fill")
                        }
                    }
                }
                Section {
                    ForEach(store.mappings) { m in
                        NavigationLink(value: m) {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(m.title).font(.headline)
                                Text("云盘 \(m.cloudPath)").font(.caption).foregroundStyle(.secondary)
                                Text("→ \(m.target)").font(.caption).foregroundStyle(.secondary)
                            }
                        }
                        .swipeActions {
                            Button(role: .destructive) { store.remove(m) } label: { Label("删除", systemImage: "trash") }
                            Button { editing = m } label: { Label("编辑", systemImage: "pencil") }.tint(.orange)
                        }
                    }
                } header: {
                    Text("推送任务")
                } footer: {
                    Text(store.mappings.isEmpty
                         ? "点右上角 + 添加：选一个云盘文件夹，推到哪个 GitHub 仓库的哪个目录。"
                         : "左滑可以编辑、删除。只推改过的文件；云盘删掉的文件不会在 GitHub 上删除。")
                }
            }
            .navigationTitle("小橘推送")
            .navigationDestination(for: Mapping.self) { PushView(mapping: $0) }
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Button { showSettings = true } label: { Image(systemName: "gearshape") }
                }
                ToolbarItem(placement: .topBarTrailing) {
                    Button { editing = Mapping() } label: { Image(systemName: "plus") }
                }
            }
            .sheet(item: $editing) { m in MappingEditView(mapping: m) }
            .sheet(isPresented: $showSettings) { SettingsView() }
        }
    }
}
