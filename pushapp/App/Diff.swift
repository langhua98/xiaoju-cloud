import Foundation

// 按行比较两个文本文件，算出加了几行、删了几行，以及带 3 行上下文的改动片段。

struct DiffLine: Identifiable {
    enum Kind { case same, added, removed, gap }
    let id: Int
    let kind: Kind
    let text: String
    let oldNo: Int?
    let newNo: Int?
}

struct FileDiff {
    var added = 0
    var removed = 0
    var lines: [DiffLine] = []
    /// 不是文本（图片等）或者太大，只显示大小
    var note: String?

    static func make(old: Data?, new: Data) -> FileDiff {
        guard let newText = text(new) else {
            return FileDiff(note: old == nil ? "新的二进制文件（\(size(new))）" : "二进制文件，\(size(old!)) → \(size(new))")
        }
        guard let old else {
            let lines = split(newText)
            var d = FileDiff(added: lines.count)
            d.lines = lines.prefix(400).enumerated().map {
                DiffLine(id: $0.offset, kind: .added, text: $0.element, oldNo: nil, newNo: $0.offset + 1)
            }
            if lines.count > 400 { d.note = "新文件共 \(lines.count) 行，只显示前 400 行" }
            return d
        }
        guard let oldText = text(old) else {
            return FileDiff(note: "原来是二进制文件，现在是文本（\(size(new))）")
        }
        return diff(split(oldText), split(newText))
    }

    private static func text(_ d: Data) -> String? {
        if d.prefix(8000).contains(0) { return nil }
        return String(data: d, encoding: .utf8)
    }

    private static func size(_ d: Data) -> String {
        ByteCountFormatter.string(fromByteCount: Int64(d.count), countStyle: .file)
    }

    private static func split(_ s: String) -> [String] {
        if s.isEmpty { return [] }
        var lines = s.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n")
        if lines.last == "" { lines.removeLast() }
        return lines
    }

    private enum Op { case same(Int, Int), add(Int), del(Int) }

    private static func diff(_ a: [String], _ b: [String]) -> FileDiff {
        // 去掉相同的开头和结尾，中间做 LCS
        var pre = 0
        while pre < a.count, pre < b.count, a[pre] == b[pre] { pre += 1 }
        var suf = 0
        while suf < a.count - pre, suf < b.count - pre, a[a.count - 1 - suf] == b[b.count - 1 - suf] { suf += 1 }
        let x = Array(a[pre..<(a.count - suf)]), y = Array(b[pre..<(b.count - suf)])
        var ops: [Op] = (0..<pre).map { Op.same($0, $0) }
        let n = x.count, m = y.count
        if n * m > 4_000_000 {
            // 太大了：整段当成删了再加
            ops += (0..<n).map { Op.del(pre + $0) }
            ops += (0..<m).map { Op.add(pre + $0) }
        } else {
            var t = [Int32](repeating: 0, count: (n + 1) * (m + 1))
            let w = m + 1
            if n > 0 && m > 0 {
                for i in stride(from: n - 1, through: 0, by: -1) {
                    for j in stride(from: m - 1, through: 0, by: -1) {
                        t[i * w + j] = x[i] == y[j] ? t[(i + 1) * w + j + 1] + 1 : max(t[(i + 1) * w + j], t[i * w + j + 1])
                    }
                }
            }
            var i = 0, j = 0
            while i < n || j < m {
                if i < n, j < m, x[i] == y[j] {
                    ops.append(.same(pre + i, pre + j)); i += 1; j += 1
                } else if j < m, i == n || t[i * w + j + 1] >= t[(i + 1) * w + j] {
                    ops.append(.add(pre + j)); j += 1
                } else {
                    ops.append(.del(pre + i)); i += 1
                }
            }
        }
        ops += (0..<suf).map { Op.same(a.count - suf + $0, b.count - suf + $0) }

        var d = FileDiff()
        var changed = [Bool](repeating: false, count: ops.count)
        for (k, op) in ops.enumerated() {
            switch op {
            case .add: d.added += 1; changed[k] = true
            case .del: d.removed += 1; changed[k] = true
            case .same: break
            }
        }
        // 只留改动和前后 3 行，中间省略
        var keep = [Bool](repeating: false, count: ops.count)
        for k in ops.indices where changed[k] {
            for c in max(0, k - 3)...min(ops.count - 1, k + 3) { keep[c] = true }
        }
        var id = 0
        var lastKept = -1
        for (k, op) in ops.enumerated() where keep[k] {
            if k > lastKept + 1 {
                d.lines.append(DiffLine(id: id, kind: .gap, text: "⋯", oldNo: nil, newNo: nil)); id += 1
            }
            lastKept = k
            switch op {
            case let .same(o, nw): d.lines.append(DiffLine(id: id, kind: .same, text: b[nw], oldNo: o + 1, newNo: nw + 1))
            case let .add(nw): d.lines.append(DiffLine(id: id, kind: .added, text: b[nw], oldNo: nil, newNo: nw + 1))
            case let .del(o): d.lines.append(DiffLine(id: id, kind: .removed, text: a[o], oldNo: o + 1, newNo: nil))
            }
            id += 1
            if d.lines.count >= 1500 { d.note = "改动太多，只显示前 1500 行"; break }
        }
        return d
    }
}
