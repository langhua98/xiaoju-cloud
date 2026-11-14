package api

import (
	"net/http"
	"regexp"
	"strings"
)

// 只读备用模式（XIAOJU_STANDBY=1）：家里电脑关着时，手机和网页临时连 Hugging Face 上的这台。
// 数据库是电脑每小时同步过来的副本，在这里改了也会被下次同步盖掉，所以只放行“看”的请求：
//   - GET、HEAD、OPTIONS 全部放行（列目录、看照片、下载、缩略图……）；
//   - 登录、刷脸、格子口令、拿下载链接、打开带密码的分享这几个 POST 也放行（只是验证，不改文件）；
//   - 其余的（上传、新建、改名、删除、AI 助手、MCP……）一律返回 423 和错误码 standby，客户端提示“电脑关着，只能看”。
//
// 每个响应都带 X-Xiaoju-Standby: 1，客户端据此显示提示条。存储驱动另外也包成只读的（见 storage.ReadOnly），双保险。
const standbyHeader = "X-Xiaoju-Standby"

var standbyAllowed = regexp.MustCompile(`^POST (` + strings.Join([]string{
	`/api/auth/session`,
	`/api/auth/passkey/login/(begin|finish)`,
	`/api/auth/grid/(challenge|verify)`,
	`/api/files/[^/]+/link`,
	`/api/s/[^/]+/unlock`,
	`/api/speedtest/up`,
}, "|") + `)$`)

func standbyAllows(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return !strings.HasPrefix(r.URL.Path, "/mcp")
	}
	return standbyAllowed.MatchString(r.Method + " " + r.URL.Path)
}

func (s *Server) standbyGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(standbyHeader, "1")
		if !standbyAllows(r) {
			// 跨域请求要带上 CORS 头，网页才读得到这个错误
			if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Expose-Headers", standbyHeader)
			}
			writeJSON(w, http.StatusLocked, map[string]string{
				"error": "家里电脑关着，云盘现在是只读备用模式：能看、能下载，不能上传、修改或删除。电脑开了会自动换回去。",
				"code":  "standby",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
