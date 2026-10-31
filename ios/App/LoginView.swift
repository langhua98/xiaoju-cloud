import AuthenticationServices
import SwiftUI

// 登录：App 自己的网页视图用不了 Face ID 通行密钥（要绑定域名），所以用系统的登录窗口
// （ASWebAuthenticationSession，和 Safari 共用通行密钥）打开网盘网页的 ?app-login=1，
// 照常输入令牌、刷脸，网页登录成功后跳回 xiaojubackup://login?session=…，App 把会话存进钥匙串。
struct LoginView: View {
    @ObservedObject private var cloud = Cloud.shared
    @State private var error = ""
    @State private var busy = false
    @State private var context = LoginContext()

    var body: some View {
        VStack(spacing: 20) {
            Spacer()
            Image(systemName: "cloud.fill")
                .font(.system(size: 64))
                .foregroundStyle(Color.brand)
            Text("小桔云盘").font(.largeTitle.bold())
            Text("登录后可以在 App 里看照片、管理文件。备份不用登录，照常在后台跑。")
                .font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
            Spacer()
            if !error.isEmpty {
                Text(error).font(.footnote).foregroundStyle(.red).multilineTextAlignment(.center)
            }
            Button {
                signIn()
            } label: {
                Text(busy ? "正在登录…" : "登录").font(.headline).frame(maxWidth: .infinity).padding(.vertical, 6)
            }
            .buttonStyle(.borderedProminent)
            .tint(.brand)
            .disabled(busy)
            Text("会打开网盘的登录页：输入令牌、刷脸，完成后自动回到 App。登录 12 小时有效。")
                .font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.center)
        }
        .padding(24)
    }

    private func signIn() {
        guard let web = cloud.webURL, var parts = URLComponents(url: web, resolvingAgainstBaseURL: false) else {
            error = "网页地址没配置"
            return
        }
        parts.queryItems = [URLQueryItem(name: "app-login", value: "1")]
        guard let url = parts.url else { return }
        error = ""
        busy = true
        let session = ASWebAuthenticationSession(url: url, callbackURLScheme: "xiaojubackup") { callback, err in
            Task { @MainActor in
                busy = false
                if let err {
                    if (err as? ASWebAuthenticationSessionError)?.code != .canceledLogin {
                        error = "登录没完成：\(err.localizedDescription)"
                    }
                    return
                }
                let token = callback.flatMap { URLComponents(url: $0, resolvingAgainstBaseURL: false) }?
                    .queryItems?.first { $0.name == "session" }?.value
                guard let token, !token.isEmpty else {
                    error = "网页没有把登录信息交回来，请更新网页后再试"
                    return
                }
                cloud.signIn(session: token)
            }
        }
        session.presentationContextProvider = context
        session.prefersEphemeralWebBrowserSession = false
        session.start()
    }
}

final class LoginContext: NSObject, ASWebAuthenticationPresentationContextProviding {
    func presentationAnchor(for session: ASWebAuthenticationSession) -> ASPresentationAnchor {
        let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
        return scenes.flatMap(\.windows).first { $0.isKeyWindow } ?? ASPresentationAnchor()
    }
}

// 格子口令抽查：4×4 数字格，按自己的规则心算出答案，依次点格子里的数字，点满位数自动提交。
// 限时（第一次 15 秒、第二次 20 秒），超时算用掉一次机会；两次都没答对，服务端退出登录。
struct GridGateView: View {
    @ObservedObject private var cloud = Cloud.shared
    @State private var challenge: GridChallenge?
    @State private var digits = ""
    @State private var status = ""
    @State private var busy = false
    @State private var deadline = Date()
    @State private var now = Date()
    private let ticker = Timer.publish(every: 0.2, on: .main, in: .common).autoconnect()
    private static let weekdays = ["", "周一", "周二", "周三", "周四", "周五", "周六", "周日"]

    var body: some View {
        VStack(spacing: 18) {
            HStack {
                Label("安全验证", systemImage: "checkmark.shield")
                    .font(.headline)
                if let challenge, challenge.weekday < Self.weekdays.count {
                    Text(Self.weekdays[challenge.weekday]).foregroundStyle(.secondary)
                }
                Spacer()
                if challenge != nil {
                    Text("\(secondsLeft)")
                        .font(.title3.bold()).monospacedDigit()
                        .foregroundStyle(secondsLeft <= 5 ? .red : .primary)
                }
            }
            LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 10), count: 4), spacing: 10) {
                ForEach(0..<16, id: \.self) { i in
                    Button {
                        tap(i)
                    } label: {
                        Text(challenge.map { String($0.grid[i]) } ?? "·")
                            .font(.title2.bold()).monospacedDigit()
                            .frame(maxWidth: .infinity, minHeight: 56)
                            .background(Color.secondary.opacity(0.15), in: RoundedRectangle(cornerRadius: 12))
                    }
                    .buttonStyle(.plain)
                    .disabled(challenge == nil || busy)
                }
            }
            HStack(spacing: 8) {
                ForEach(0..<(challenge?.digits ?? 0), id: \.self) { i in
                    Circle()
                        .fill(i < digits.count ? Color.brand : Color.secondary.opacity(0.3))
                        .frame(width: 12, height: 12)
                }
            }
            Text(status).font(.footnote).foregroundStyle(.secondary).frame(minHeight: 18)
        }
        .padding(24)
        .presentationDetents([.medium, .large])
        .interactiveDismissDisabled()
        .task { await load() }
        .onReceive(ticker) { t in
            now = t
            if challenge != nil, !busy, t >= deadline { Task { await submit(timeout: true) } }
        }
    }

    private var secondsLeft: Int { max(0, Int(deadline.timeIntervalSince(now).rounded(.up))) }

    private func load() async {
        challenge = nil
        digits = ""
        do {
            let c = try await cloud.post("/api/auth/grid/challenge", json: [:], as: GridChallenge.self)
            challenge = c
            deadline = Date().addingTimeInterval(TimeInterval(c.seconds > 0 ? c.seconds : 15))
            if c.attempt > 1 && status.isEmpty { status = "最后一次机会" }
        } catch CloudError.unauthorized {
            cloud.finishGrid(false)
        } catch {
            status = error.localizedDescription
        }
    }

    private func tap(_ i: Int) {
        guard let c = challenge, !busy, digits.count < c.digits else { return }
        digits += String(c.grid[i])
        if digits.count == c.digits { Task { await submit(timeout: false) } }
    }

    private func submit(timeout: Bool) async {
        guard let c = challenge, !busy else { return }
        busy = true
        defer { busy = false }
        var body: [String: Any] = ["id": c.id]
        if timeout { body["timeout"] = true } else { body["answer"] = Int(digits) ?? 0 }
        do {
            try await cloud.send("POST", "/api/auth/grid/verify", json: body)
            cloud.finishGrid(true)
        } catch CloudError.unauthorized {
            cloud.finishGrid(false) // 两次都没答对，已退出登录
        } catch CloudError.api(let code, let message) {
            status = message
            if ["wrong_answer", "timeout", "challenge_expired"].contains(code) {
                status = code == "timeout" ? "超时了，换一题" : "不对，换一题"
                await load()
            }
        } catch {
            status = error.localizedDescription
            deadline = Date().addingTimeInterval(3) // 网络不通：过 3 秒再交一次“超时”
        }
    }
}

struct GridChallenge: Decodable {
    let id: String
    let grid: [Int]
    let weekday: Int
    let attempt: Int
    let seconds: Int
    let digits: Int

    enum CodingKeys: String, CodingKey { case id, grid, weekday, attempt, seconds, digits }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        grid = try c.decode([Int].self, forKey: .grid)
        weekday = try c.decodeIfPresent(Int.self, forKey: .weekday) ?? 0
        attempt = try c.decodeIfPresent(Int.self, forKey: .attempt) ?? 1
        seconds = try c.decodeIfPresent(Int.self, forKey: .seconds) ?? 15
        digits = try c.decodeIfPresent(Int.self, forKey: .digits) ?? 2
    }
}
