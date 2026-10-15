package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 批量上传（iPhone 快捷指令等）：把照片、视频直接传进根目录的“相册”文件夹。
// 只进不出：不需要任何认证，但只能添加文件（不会覆盖已有文件），不能列目录、下载或删除，
// 所以不影响刷脸和格子口令对查看的保护。代价是知道地址的人都能往“相册”里塞文件。

// AlbumFolder 是同步相册用的文件夹（和网页的“同步相册”是同一个）。
const AlbumFolder = "相册"

// POST /api/ingest[?name=IMG_0001.HEIC]
//
//	头部 X-File-Name: 文件名（也可以放在 ?name=；没有扩展名时按文件内容补上）
//	请求体：文件原始内容
//
// 文件名去掉自动加的序号后相同、大小也相同的视为已经传过，直接跳过：
// 返回 200 {"result":"skipped"}；新上传返回 201 {"result":"uploaded"}。同名不同内容自动改名为“名字 (2)”。
//
// 大文件（视频）收完后写 Telegram 可能要好几分钟，而快捷指令等回复超过约 60 秒就算超时。所以先把整个文件
// 收完、加密存进临时文件再写；写了 ingestHeartbeat 还没写完，就先回 200，之后每隔 ingestHeartbeat 发一个
// 空格让连接保持活跃，写完再发 JSON（前面的空格不影响解析；这种情况下出错是 200 {"result":"error"}）。
// 文件收完后即使客户端断开也会写完，下次运行会当作已传过跳过。
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body := bufio.NewReaderSize(r.Body, 64<<10)
	head, _ := body.Peek(64)
	ext, sniffed := sniffMedia(head)
	name := ingestName(r, ext)

	album, err := s.albumFolder(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 已经传过：读完请求体（避免客户端在发送途中被断开而报错），不再写 Telegram
	if r.ContentLength > 0 {
		same, err := s.svc.Store().ListFilesBySize(ctx, s.userID, album.ID, r.ContentLength)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, f := range same {
			if strings.EqualFold(stripCopySuffix(f.Name), stripCopySuffix(name)) {
				io.Copy(io.Discard, body)
				writeJSON(w, http.StatusOK, map[string]any{"result": "skipped", "name": f.Name})
				return
			}
		}
	}

	mimeType := sniffed
	if mimeType == "" {
		mimeType = mimeByExt(filepath.Ext(name))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	// 同名不同内容：改名“名字 (2)”。Upload 在读请求体之前就检查重名，所以可以换个名字重试
	uploadCtx := context.WithoutCancel(ctx) // 文件收完后客户端断开也要写完
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n = withCopySuffix(name, i)
		}
		var hb *heartbeat
		f, err := s.svc.Upload(uploadCtx, service.UploadRequest{
			UserID: s.userID, ParentID: &album.ID, Name: n, Mime: mimeType, Body: body,
			Spool: true, OnSpooled: func() { hb = startHeartbeat(w) },
		})
		started := hb.end() // 之后只有这里写回复
		// 重名在读请求体之前就会发现（hb 还没创建），可以换个名字重试
		if errors.Is(err, meta.ErrConflict) && hb == nil && i < 100 {
			continue
		}
		if err != nil {
			if !started {
				s.fail(w, err)
				return
			}
			s.log.Error("ingest failed after the body was received", "name", n, "err", err)
			json.NewEncoder(w).Encode(map[string]any{"result": "error", "error": "服务端写入失败，请稍后再运行一次（传过的会自动跳过）"})
			return
		}
		s.log.Info("ingested", "name", f.Name, "size", f.Size, "client", clientIP(r))
		resp := map[string]any{"result": "uploaded", "name": f.Name, "size": f.Size}
		if started {
			json.NewEncoder(w).Encode(resp)
		} else {
			writeJSON(w, http.StatusCreated, resp)
		}
		return
	}
}

// ingestHeartbeat 是写 Telegram 期间给客户端发空格的间隔（要明显短于快捷指令约 60 秒的等待上限）。
var ingestHeartbeat = 10 * time.Second

// heartbeat：过了 ingestHeartbeat 还没写完就先回 200，然后定时发空格。
type heartbeat struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timer   *time.Timer
	mu      sync.Mutex
	started bool // 已经回了 200
	stopped bool
	done    chan struct{}
}

func startHeartbeat(w http.ResponseWriter) *heartbeat {
	hb := &heartbeat{w: w, rc: http.NewResponseController(w), done: make(chan struct{})}
	hb.timer = time.AfterFunc(ingestHeartbeat, hb.begin)
	return hb
}

func (hb *heartbeat) begin() {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.stopped {
		return
	}
	hb.started = true
	hb.w.Header().Set("Content-Type", "application/json")
	hb.w.Header().Set("X-Accel-Buffering", "no") // 让代理别缓冲
	hb.w.WriteHeader(http.StatusOK)
	hb.w.Write([]byte(" "))
	hb.rc.Flush()
	go func() {
		t := time.NewTicker(ingestHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				hb.mu.Lock()
				if !hb.stopped {
					hb.w.Write([]byte(" "))
					hb.rc.Flush()
				}
				hb.mu.Unlock()
			case <-hb.done:
				return
			}
		}
	}()
}

// end 停止心跳，返回是否已经回了 200（这时只能接着写 JSON，不能再设状态码）。hb 为 nil 时返回 false。
func (hb *heartbeat) end() bool {
	if hb == nil {
		return false
	}
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if !hb.stopped {
		hb.stopped = true
		hb.timer.Stop()
		close(hb.done)
	}
	return hb.started
}

// clientIP 取上传者的地址（记日志用；HF Space 在代理后面，看 X-Forwarded-For）。
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		ip, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(ip)
	}
	return r.RemoteAddr
}

// albumFolder 返回根目录的“相册”文件夹，没有就新建。
func (s *Server) albumFolder(r *http.Request) (*meta.File, error) {
	store := s.svc.Store()
	f, err := store.FindFolder(r.Context(), s.userID, nil, AlbumFolder)
	if errors.Is(err, meta.ErrNotFound) {
		f, err = store.CreateFolder(r.Context(), s.userID, nil, AlbumFolder)
		if errors.Is(err, meta.ErrConflict) { // 同时有别的请求建好了
			f, err = store.FindFolder(r.Context(), s.userID, nil, AlbumFolder)
		}
	}
	return f, err
}

// ingestName 取文件名：?name= 或 X-File-Name（可以是 URL 编码的），去掉路径分隔符；
// 没有扩展名时按文件内容补上（快捷指令给的照片名称通常没有扩展名）。
func ingestName(r *http.Request, ext string) string {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.Header.Get("X-File-Name")
		if strings.Contains(name, "%") {
			if u, err := url.PathUnescape(name); err == nil {
				name = u
			}
		}
	}
	if !utf8.ValidString(name) {
		name = ""
	}
	name = strings.TrimSpace(strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c < 0x20 {
			return '_'
		}
		return c
	}, name))
	if name == "" || name == "." || name == ".." {
		name = "照片 " + time.Now().Format("2006-01-02 150405") + "-" + uuid.NewString()[:4]
	}
	if filepath.Ext(name) == "" && ext != "" {
		name += ext
	}
	if len(name) > 200 { // 留出改名“ (2)”的余地，按字符截断
		ext := filepath.Ext(name)
		stem := []rune(strings.TrimSuffix(name, ext))
		for len(string(stem))+len(ext) > 200 {
			stem = stem[:len(stem)-1]
		}
		name = string(stem) + ext
	}
	return name
}

var copySuffix = regexp.MustCompile(` \(\d+\)(\.[^.]*)?$`)

// stripCopySuffix 去掉自动加的序号：“IMG_1 (2).jpg” → “IMG_1.jpg”（和网页同步相册的规则一样）。
func stripCopySuffix(n string) string { return copySuffix.ReplaceAllString(n, "$1") }

func withCopySuffix(n string, i int) string {
	ext := filepath.Ext(n)
	return strings.TrimSuffix(n, ext) + " (" + strconv.Itoa(i) + ")" + ext
}

// sniffMedia 按文件开头的字节认出常见的照片、视频格式，返回扩展名和 MIME 类型。
func sniffMedia(b []byte) (ext, mimeType string) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return ".jpg", "image/jpeg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png", "image/png"
	case bytes.HasPrefix(b, []byte("GIF8")):
		return ".gif", "image/gif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp", "image/webp"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		switch string(b[8:12]) {
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
			return ".heic", "image/heic"
		case "avif", "avis":
			return ".avif", "image/avif"
		case "qt  ":
			return ".mov", "video/quicktime"
		default: // isom、mp41、mp42、M4V …
			return ".mp4", "video/mp4"
		}
	}
	return "", ""
}

func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".heic", ".heif":
		return "image/heic"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	}
	return mime.TypeByExtension(ext)
}
