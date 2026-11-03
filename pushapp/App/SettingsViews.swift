import SwiftUI

// 设置：云盘地址、两个令牌；以及编辑推送任务（选云盘文件夹、选仓库）。

struct SettingsView: View {
    @EnvironmentObject var store: Store
    @Environment(\.dismiss) private var dismiss
    @State private var cloudResult = ""
    @State private var githubResult = ""
    @State private var testing = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("地址", text: $store.cloudURL)
                        .textInputAutocapitalization(.never).autocorrectionDisabled().keyboardType(.URL)
                    SecureField("xa_ 开头的令牌", text: $store.cloudToken)
                } header: { Text("小橘云盘") } footer: {
                    Text("在网盘网页的「Agent 令牌」里新建，只读就够。")
                }
                Section {
                    SecureField("GitHub 令牌", text: $store.githubToken)
                } header: { Text("GitHub") } footer: {
                    Text("GitHub → Settings → Developer settings → Personal access tokens 新建，要能写目标仓库（Contents: Read and write）。令牌存在手机钥匙串里。")
                }
                Section {
                    Button(testing ? "测试中…" : "测试连接") { test() }.disabled(testing || !store.ready)
                    if !cloudResult.isEmpty { Text(cloudResult).font(.footnote) }
                    if !githubResult.isEmpty { Text(githubResult).font(.footnote) }
                }
            }
            .navigationTitle("设置")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("完成") { dismiss() } } }
        }
    }

    private func test() {
        testing = true
        cloudResult = ""
        githubResult = ""
        let e = store.engine()
        Task {
            do { cloudResult = "云盘：✓ \(try await e.checkCloud())" } catch { cloudResult = "云盘：✗ \(error.localizedDescription)" }
            do { githubResult = "GitHub：✓ \(try await e.whoami())" } catch { githubResult = "GitHub：✗ \(error.localizedDescription)" }
            testing = false
        }
    }
}

struct MappingEditView: View {
    @EnvironmentObject var store: Store
    @Environment(\.dismiss) private var dismiss
    @State var mapping: Mapping

    var body: some View {
        NavigationStack {
            Form {
                Section("云盘文件夹") {
                    TextField("/代码/my-app", text: $mapping.cloudPath)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                    NavigationLink("浏览云盘…") { FolderPicker(path: "/", selection: $mapping.cloudPath) }
                        .disabled(!store.ready)
                }
                Section {
                    TextField("owner/name", text: $mapping.repo)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                    NavigationLink("从我的仓库里选…") { RepoPicker(selection: $mapping.repo) }
                        .disabled(!store.ready)
                    TextField("分支（空 = 默认分支）", text: $mapping.branch)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                    TextField("仓库里的目录（空 = 根目录）", text: $mapping.repoPath)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                } header: { Text("推到 GitHub") }
            }
            .navigationTitle(mapping.cloudPath.isEmpty ? "新任务" : mapping.title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("保存") { store.upsert(mapping); dismiss() }
                        .disabled(mapping.cloudPath.isEmpty || !mapping.repo.contains("/"))
                }
            }
        }
    }
}

struct FolderPicker: View {
    @EnvironmentObject var store: Store
    let path: String
    @Binding var selection: String
    @State private var folders: [CloudItem] = []
    @State private var error = ""
    @State private var loading = true
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        List {
            Section {
                Button("选这个文件夹：\(path)") { selection = path; dismiss() }
            }
            Section {
                if loading { ProgressView() }
                if !error.isEmpty { Text(error).foregroundStyle(.red) }
                ForEach(folders, id: \.id) { f in
                    let sub = (path == "/" ? "" : path) + "/" + f.name
                    NavigationLink { FolderPicker(path: sub, selection: $selection) } label: {
                        Label(f.name, systemImage: "folder")
                    }
                }
                if !loading && error.isEmpty && folders.isEmpty { Text("没有子文件夹").foregroundStyle(.secondary) }
            }
        }
        .navigationTitle(path == "/" ? "云盘" : (path.split(separator: "/").last.map(String.init) ?? path))
        .task {
            do {
                folders = try await store.engine().listFolder(path).filter(\.isFolder)
            } catch { self.error = error.localizedDescription }
            loading = false
        }
    }
}

struct RepoPicker: View {
    @EnvironmentObject var store: Store
    @Binding var selection: String
    @State private var repos: [Repo] = []
    @State private var error = ""
    @State private var loading = true
    @State private var query = ""
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        List {
            if loading { ProgressView() }
            if !error.isEmpty { Text(error).foregroundStyle(.red) }
            ForEach(repos.filter { query.isEmpty || $0.full_name.localizedCaseInsensitiveContains(query) }, id: \.full_name) { r in
                Button {
                    selection = r.full_name
                    dismiss()
                } label: {
                    HStack {
                        Text(r.full_name)
                        Spacer()
                        if r.private { Image(systemName: "lock").foregroundStyle(.secondary) }
                    }
                }
            }
        }
        .searchable(text: $query)
        .navigationTitle("我的仓库")
        .task {
            do { repos = try await store.engine().repos() } catch { self.error = error.localizedDescription }
            loading = false
        }
    }
}
