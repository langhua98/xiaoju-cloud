package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 大列表（相册一万多张照片的目录、回收站、重复文件）压缩后再发：家里电脑往外只有 2~6 Mbps，
// 不压缩时「相册」一次要发近 10 MB，App 等 60 秒超时（"The request timed out."）。
// 文件 ID、校验值这类内容压缩后约剩 1/4。浏览器和 iOS 的 URLSession 都会自动解压。
const gzipMinBytes = 8 << 10

func writeJSONGzip(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Add("Vary", "Accept-Encoding")
	if buf.Len() < gzipMinBytes || !acceptsGzip(r) {
		h.Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(status)
		w.Write(buf.Bytes())
		return
	}
	var zbuf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zbuf, gzip.BestSpeed)
	zw.Write(buf.Bytes())
	zw.Close()
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(zbuf.Len()))
	w.WriteHeader(status)
	w.Write(zbuf.Bytes())
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return strings.TrimSpace(strings.ReplaceAll(q, " ", "")) != "q=0"
		}
	}
	return false
}
