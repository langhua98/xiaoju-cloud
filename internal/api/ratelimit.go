package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipLimiter 按客户端 IP 限速，给不需要认证的接口用（快捷指令批量上传、分享链接）：知道地址的人也没法无限地发请求。
// 正常使用远远碰不到（快捷指令一个接一个传，跳过传过的最快每秒两三个）。
type ipLimiter struct {
	every time.Duration
	burst int

	mu    sync.Mutex
	m     map[string]*ipBucket
	swept time.Time
}

type ipBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newIPLimiter(perMinute, burst int) *ipLimiter {
	return &ipLimiter{every: time.Minute / time.Duration(perMinute), burst: burst, m: map[string]*ipBucket{}}
}

func (l *ipLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.swept) > 10*time.Minute { // 很久没来的丢掉，免得越攒越多
		for k, b := range l.m {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.m, k)
			}
		}
		l.swept = now
	}
	b := l.m[key]
	if b == nil {
		b = &ipBucket{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.m[key] = b
	}
	b.seen = now
	return b.lim.Allow()
}

// blocked 报告这个地址现在是不是已经用完了次数（不消耗次数）。
func (l *ipLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	return b != nil && b.lim.Tokens() < 1
}

// limited 超过限速时回 429（快捷指令会等一会儿再试，见 backup.cherri）。
func (s *Server) limited(l *ipLimiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(limitKey(r)) {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests, try again later", "code": "rate_limited"})
			return
		}
		h(w, r)
	}
}

// limitKey 是限速按的客户端地址：X-Forwarded-For 的最后一个（前面的客户端可以随便填，最后一个是前面那层代理看到的）。
func limitKey(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if i := strings.LastIndexByte(f, ','); i >= 0 {
			f = f[i+1:]
		}
		return strings.TrimSpace(f)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}
