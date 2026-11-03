package api

import (
	"crypto/rand"
	"io"
	"net/http"
	"strconv"
	"time"
)

// 网速测试（App「备份」页右上角的小卡片用）：测手机到服务器的延迟、下载、上传速度。
// 不碰任何文件，所以不要求登录（没登录也能备份），只按 IP 限速，单次最多 25MB。
//   - GET  /api/speedtest/ping：马上回 204，客户端自己算来回时间
//   - GET  /api/speedtest/down?bytes=N：发 N 个随机字节（随机的压缩不了，测的是真网速）
//   - POST /api/speedtest/up：收下请求体扔掉，回 {"bytes": 收到多少, "ms": 服务端读了多久}
const speedTestMax = 25 << 20

// 1MB 随机数据，下载测速时反复发
var speedTestBlock = func() []byte {
	b := make([]byte, 1<<20)
	rand.Read(b)
	return b
}()

func (s *Server) speedTestRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/speedtest/ping", s.limited(s.speedLimit, speedPing))
	api.HandleFunc("GET /api/speedtest/down", s.limited(s.speedLimit, speedDown))
	api.HandleFunc("POST /api/speedtest/up", s.limited(s.speedLimit, speedUp))
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func speedPing(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.WriteHeader(http.StatusNoContent)
}

func speedDown(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("bytes"))
	if err != nil || n <= 0 {
		n = 8 << 20
	}
	n = min(n, speedTestMax)
	noStore(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(n))
	for left := n; left > 0; {
		chunk := speedTestBlock[:min(left, len(speedTestBlock))]
		if _, err := w.Write(chunk); err != nil {
			return
		}
		left -= len(chunk)
	}
}

func speedUp(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	n, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, speedTestMax))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too big, at most 25MB")
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]int64{"bytes": n, "ms": time.Since(start).Milliseconds()})
}
