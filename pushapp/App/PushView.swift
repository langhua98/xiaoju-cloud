import SwiftUI

// 一个任务：检查改动 → 列出改了哪些文件、改了几行 → 点文件看具体代码改动 → 写提交说明 → 提交并推送。
struct PushView: View {
    @EnvironmentObject var store: Store
    let mapping: Mapping

    @State private var plan: Plan?
    @State private var status = ""
    @State private var error = ""
    @State private var working = false
    @State private var message = ""
    @State private var result: (sha: String, url: String)?

    var body: some View {
        List {
            Section {
                LabeledContent("云盘", value: mapping.cloudPath)
                LabeledContent("推到", value: mapping.target)
            }
            if working {
                Section { HStack { ProgressView(); Text(status).font(.footnote).foregroundStyle(.secondary) } }
            }
            if !error.isEmpty {
                Section { Text(error).foregroundStyle(.red) }
            }
            if let result {
                Section("已推送") {
                    if let url = URL(string: result.url) {
                        Link("在 GitHub 上看这次提交（\(result.sha.prefix(7))）", destination: url)
                    }
                }
            }
            if let plan, result == nil {
                summary(plan)
                if !plan.changes.isEmpty {
                    Section {
                        TextEditor(text: $message).frame(minHeight: 120).font(.system(.footnote, design: .monospaced))
                        Button {
                            push(plan)
                        } label: {
                            Label("提交并推送 \(plan.changes.count) 个文件", systemImage: "arrow.up.circle.fill")
                        }
                        .disabled(working || message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    } header: { Text("提交说明") } footer: {
                        Text("推到 \(plan.repo) 的 \(plan.branch) 分支。")
                    }
                }
            }
        }
        .navigationTitle(mapping.title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button("重新检查") { check() }.disabled(working)
            }
        }
        .refreshable { check() }
        .task { if plan == nil && !working { check() } }
    }

    @ViewBuilder
    private func summary(_ plan: Plan) -> some View {
        let added = plan.changes.reduce(0) { $0 + $1.diff.added }
        let removed = plan.changes.reduce(0) { $0 + $1.diff.removed }
        Section {
            if plan.changes.isEmpty {
                Text("没有改动，和 GitHub 上一样（\(plan.unchanged) 个文件）。").foregroundStyle(.secondary)
            }
            ForEach(plan.changes) { c in
                NavigationLink { DiffView(change: c) } label: { ChangeRow(change: c) }
            }
            ForEach(plan.skipped, id: \.self) { Text("跳过 \($0)").font(.footnote).foregroundStyle(.orange) }
        } header: {
            if !plan.changes.isEmpty {
                Text("改了 \(plan.changes.count) 个文件  +\(added) −\(removed)")
            }
        } footer: {
            if !plan.changes.isEmpty { Text("另有 \(plan.unchanged) 个文件没变。点文件看具体改了哪几行。") }
        }
    }

    private func check() {
        guard store.ready else { error = "先在设置里填好两个令牌"; return }
        working = true
        error = ""
        result = nil
        status = "检查改动…"
        let engine = store.engine()
        let pushed = store.pushed
        Task {
            do {
                let p = try await engine.plan(mapping, pushed: pushed) { s in Task { @MainActor in status = s } }
                store.record(p.confirmed)
                plan = p
                message = Self.defaultMessage(p)
            } catch {
                self.error = error.localizedDescription
            }
            working = false
        }
    }

    private func push(_ plan: Plan) {
        working = true
        error = ""
        let engine = store.engine()
        let msg = message
        Task {
            do {
                let r = try await engine.commit(plan, message: msg) { s in Task { @MainActor in status = s } }
                var rec: [String: Pushed] = [:]
                for c in plan.changes { rec[plan.recordKey(c.repoPath)] = Pushed(sha256: c.sha256, blob: c.blob) }
                store.record(rec)
                result = r
            } catch {
                self.error = error.localizedDescription
            }
            working = false
        }
    }

    /// 默认提交说明：第一行是摘要，下面列出每个文件和加减行数。
    static func defaultMessage(_ p: Plan) -> String {
        guard !p.changes.isEmpty else { return "" }
        let names = p.changes.prefix(3).map { ($0.repoPath as NSString).lastPathComponent }.joined(separator: "、")
        var title = "从小橘云盘更新 \(names)"
        if p.changes.count > 3 { title += " 等 \(p.changes.count) 个文件" }
        let lines = p.changes.map { c -> String in
            let kind = c.isNew ? "新增" : "修改"
            let stat = c.diff.note == nil || c.diff.added + c.diff.removed > 0
                ? "（+\(c.diff.added) −\(c.diff.removed)）" : ""
            return "- \(kind) \(c.repoPath)\(stat)"
        }
        return title + "\n\n" + lines.joined(separator: "\n")
    }
}

struct ChangeRow: View {
    let change: Change

    var body: some View {
        HStack {
            Text(change.isNew ? "新" : "改")
                .font(.caption.bold()).foregroundStyle(.white)
                .frame(width: 22, height: 22)
                .background(change.isNew ? Color.green : Color.orange, in: RoundedRectangle(cornerRadius: 5))
            VStack(alignment: .leading, spacing: 2) {
                Text((change.repoPath as NSString).lastPathComponent).lineLimit(1)
                Text(change.repoPath).font(.caption2).foregroundStyle(.secondary).lineLimit(1).truncationMode(.head)
            }
            Spacer()
            if change.diff.added + change.diff.removed > 0 {
                Text("+\(change.diff.added)").font(.caption.monospacedDigit()).foregroundStyle(.green)
                Text("−\(change.diff.removed)").font(.caption.monospacedDigit()).foregroundStyle(.red)
            }
        }
    }
}

// 一个文件改了哪几行：绿色是加的，红色是删的，⋯ 是省略的没改的部分。
struct DiffView: View {
    let change: Change

    var body: some View {
        ScrollView([.vertical, .horizontal]) {
            VStack(alignment: .leading, spacing: 0) {
                if let note = change.diff.note {
                    Text(note).font(.footnote).foregroundStyle(.secondary).padding()
                }
                ForEach(change.diff.lines) { l in
                    HStack(alignment: .top, spacing: 6) {
                        Text(l.oldNo.map(String.init) ?? "").frame(width: 34, alignment: .trailing)
                        Text(l.newNo.map(String.init) ?? "").frame(width: 34, alignment: .trailing)
                        Text(Self.sign(l.kind))
                        Text(l.text.isEmpty ? " " : l.text).fixedSize()
                    }
                    .font(.system(size: 12, design: .monospaced))
                    .foregroundStyle(l.kind == .gap ? Color.secondary : Color.primary)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 1)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Self.color(l.kind))
                }
            }
        }
        .navigationTitle((change.repoPath as NSString).lastPathComponent)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .principal) {
                VStack {
                    Text((change.repoPath as NSString).lastPathComponent).font(.headline)
                    Text("+\(change.diff.added) −\(change.diff.removed)").font(.caption2).foregroundStyle(.secondary)
                }
            }
        }
    }

    static func sign(_ k: DiffLine.Kind) -> String {
        switch k {
        case .added: return "+"
        case .removed: return "−"
        case .same, .gap: return " "
        }
    }

    static func color(_ k: DiffLine.Kind) -> Color {
        switch k {
        case .added: return Color.green.opacity(0.18)
        case .removed: return Color.red.opacity(0.18)
        case .gap: return Color.gray.opacity(0.12)
        case .same: return Color.clear
        }
    }
}
