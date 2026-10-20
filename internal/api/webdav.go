package api

import (
	"crypto/subtle"
	"net/http"
)

// webdav 给 WebDAV 套上认证：HTTP Basic，用户名随意，密码是 XIAOJU_WEBDAV_PASSWORD。
// 不用管理员令牌：WebDAV 客户端会把密码存在本地，泄露了也只影响 WebDAV（私密文件夹看不到），
// 换一个就行。同一个 IP 密码错太多次先回 429，挡住猜密码。
func (s *Server) webdav(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := limitKey(r)
		if s.davFails.blocked(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed logins, try again later", http.StatusTooManyRequests)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.davPassword)) != 1 {
			if ok { // 没带密码的是客户端第一次试探，不算错
				s.davFails.allow(ip)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="xiaoju-cloud", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPut, "COPY": // 会往存储端写文件，部署前要等它传完
			s.tracked(next.ServeHTTP)(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
