import Foundation

// 按行比较两个文本文件，算出加了几行、删了几行，以及带 3 行上下文的改动片段。

struct DiffLine: Identifiable {
    enum Kind { case same, added, removed, gap }
    let id: Int
    let kind: Kind
    let text: String
    let oldNo: Int?
    let newNo: Int?

    /// 压缩过的代码一行可能有几万个字符，显示时截断，不然手机会卡住
    static let maxChars = 300

    init(id: Int, kind: Kind, text: String, oldNo: Int?, newNo: Int?) {
        self.id = id
        self.kind = kind
        let utf16 = text.utf16.count
        self.text = utf16 > Self.maxChars ? String(text.prefix(Self.maxChars)) + " …（这行共 \(utf16) 个字符）" : text
        self.oldNo = oldNo
        self.newNo = newNo
    }
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

    /// Myers 差分（和 git diff 一样的算法），只存每一步用到的那段，内存约 D² 个数。
    /// 改动超过 maxD 行时返回 nil。
    private static func myers(_ a: [String], _ b: [String], maxD: Int = 4000) -> [Op]? {
        var ids: [String: Int] = [:]
        let x = a.map { s -> Int in if let i = ids[s] { return i }; ids[s] = ids.count; return ids.count - 1 }
        let y = b.map { s -> Int in if let i = ids[s] { return i }; ids[s] = ids.count; return ids.count - 1 }
        let n = x.count, m = y.count
        let off = n + m + 1
        var v = [Int](repeating: 0, count: 2 * (n + m) + 3)
        var trace: [(lo: Int, v: [Int])] = []
        var found = -1
        search: for d in 0...(n + m) {
            if d > maxD { return nil }
            let lo = max(0, off - d - 1)
            trace.append((lo, Array(v[lo...min(v.count - 1, off + d + 1)])))
            for k in stride(from: -d, through: d, by: 2) {
                var px: Int
                if k == -d || (k != d && v[off + k - 1] < v[off + k + 1]) { px = v[off + k + 1] } else { px = v[off + k - 1] + 1 }
                var py = px - k
                while px < n, py < m, x[px] == y[py] { px += 1; py += 1 }
                v[off + k] = px
                if px >= n && py >= m { found = d; break search }
            }
        }
        guard found >= 0 else { return nil }
        var ops: [Op] = []
        var i = n, j = m
        for d in stride(from: found, through: 0, by: -1) {
            let t = trace[d]
            func at(_ k: Int) -> Int { t.v[off + k - t.lo] }
            let k = i - j
            let pk = (k == -d || (k != d && at(k - 1) < at(k + 1))) ? k + 1 : k - 1
            let pi = at(pk), pj = pi - pk
            while i > pi && j > pj { i -= 1; j -= 1; ops.append(.same(i, j)) }
            if d > 0 {
                if i == pi { ops.append(.add(j - 1)) } else { ops.append(.del(i - 1)) }
            }
            i = pi; j = pj
        }
        return ops.reversed()
    }

    private static func diff(_ a: [String], _ b: [String]) -> FileDiff {
        // 去掉相同的开头和结尾，中间用 Myers 差分
        var pre = 0
        while pre < a.count, pre < b.count, a[pre] == b[pre] { pre += 1 }
        var suf = 0
        while suf < a.count - pre, suf < b.count - pre, a[a.count - 1 - suf] == b[b.count - 1 - suf] { suf += 1 }
        let x = Array(a[pre..<(a.count - suf)]), y = Array(b[pre..<(b.count - suf)])
        var ops: [Op] = (0..<pre).map { Op.same($0, $0) }
        if let mid = myers(x, y) {
            for op in mid {
                switch op {
                case let .same(i, j): ops.append(.same(pre + i, pre + j))
                case let .add(j): ops.append(.add(pre + j))
                case let .del(i): ops.append(.del(pre + i))
                }
            }
        } else {
            // 改得太多：整段当成删了再加
            ops += (0..<x.count).map { Op.del(pre + $0) }
            ops += (0..<y.count).map { Op.add(pre + $0) }
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
