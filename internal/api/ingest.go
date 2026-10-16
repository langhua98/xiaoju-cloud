package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/langhua98/xiaoju-cloud/internal/media"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 批量上传（iPhone 快捷指令等）：把照片、视频直接传进根目录的“相册”文件夹。
// 只进不出：不需要任何认证，但只能添加文件（不会覆盖已有文件），不能列目录、下载或删除，
// 所以不影响刷脸和格子口令对查看的保护。代价是知道地址的人都能往“相册”里塞文件。

// AlbumFolder 是同步相册用的文件夹（和网页的“同步相册”是同一个）。
const AlbumFolder = "相册"

// HiddenAlbumFolder 放 iPhone“已隐藏”相簿里的照片，和其他照片分开。
const HiddenAlbumFolder = "隐藏相册"

// ingestAlbums：上传时用 X-Xiaoju-Album 选放进哪个文件夹（都在根目录）。只能选这几个，不能随意建文件夹。
var ingestAlbums = map[string]string{"": AlbumFolder, "hidden": HiddenAlbumFolder}

// POST /api/ingest[?name=IMG_0001.HEIC]
//
//	头部 X-File-Name: 文件名（也可以放在 ?name=；没有扩展名时按文件内容补上）
//	头部 X-Xiaoju-Album: hidden 时放进“隐藏相册”，不带时放进“相册”
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
	start := time.Now()
	body := bufio.NewReaderSize(r.Body, 64<<10)
	head, _ := body.Peek(64)
	ext, sniffed := media.Sniff(head)
	name, ok := ingestName(r, ext)
	if !ok {
		// 快捷指令没给文件名：以前会自动起名“照片 日期…”，结果重新运行时认不出传过、还会重复上传。现在拒绝，
		// 快捷指令弹出“没传上去”并停下
		io.Copy(io.Discard, body)
		s.log.Warn("ingest without a file name rejected", "client", clientIP(r))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file name (X-File-Name)", "code": "missing_name"})
		return
	}

	folder, ok := ingestAlbums[strings.ToLower(strings.TrimSpace(r.Header.Get("X-Xiaoju-Album")))]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown album")
		return
	}
	album, err := s.albumFolder(r, folder)
	if err != nil {
		s.fail(w, err)
		return
	}
	// 开始收文件时也记一条：大视频要传很久，只有传完才记日志的话，中间看不出在传哪个、是不是卡住了
	s.log.Info("ingest receiving", "folder", folder, "name", name, "size", r.ContentLength, "client", clientIP(r))
	// 已经传过：读完请求体（避免客户端在发送途中被断开而报错），不再写 Telegram
	if r.ContentLength > 0 {
		f, err := s.uploadedBefore(ctx, album.ID, name, r.ContentLength)
		if err != nil {
			s.fail(w, err)
			return
		}
		if f != nil {
			io.Copy(io.Discard, body)
			s.log.Info("ingest skipped: uploaded before", "folder", folder, "name", name, "took", time.Since(start).Round(100*time.Millisecond))
			writeJSON(w, http.StatusOK, map[string]any{"result": "skipped", "name": f.Name})
			return
		}
	}

	mimeType := sniffed
	if mimeType == "" {
		mimeType = media.ByExt(filepath.Ext(name))
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
		var received time.Time
		f, err := s.svc.Upload(uploadCtx, service.UploadRequest{
			UserID: s.userID, ParentID: &album.ID, Name: n, Mime: mimeType, Body: body,
			Spool: true, OnSpooled: func() { received = time.Now(); hb = startHeartbeat(w) },
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
		// receive：手机传到服务端用的时间；store：写进 Telegram（主频道和备份一起）用的时间。看慢在哪边
		s.log.Info("ingested", "folder", folder, "name", f.Name, "size", f.Size, "client", clientIP(r),
			"receive", received.Sub(start).Round(100*time.Millisecond), "store", time.Since(received).Round(100*time.Millisecond))
		resp := map[string]any{"result": "uploaded", "name": f.Name, "size": f.Size}
		if started {
			json.NewEncoder(w).Encode(resp)
		} else {
			writeJSON(w, http.StatusCreated, resp)
		}
		return
	}
}

// uploadedBefore 找文件夹里已经传过的同一个文件：文件名去掉自动加的序号后相同（不分大小写）、大小也相同。
// name 没有扩展名时只比较扩展名前面的部分（从相册直接传时快捷指令给的名字没有扩展名）。
//
// 以前没给文件名时服务端会自动起名“照片 2026-10-16 035540-982f.mp4”。同一个文件夹里有这种名字、大小又完全一样的，
// 就是同一个文件当时没带名字传上来的：改回真名，当作传过（不再重复上传）。
func (s *Server) uploadedBefore(ctx context.Context, albumID uuid.UUID, name string, size int64) (*meta.File, error) {
	same, err := s.svc.Store().ListFilesBySize(ctx, s.userID, albumID, size)
	if err != nil {
		return nil, err
	}
	want := stripCopySuffix(name)
	noExt := filepath.Ext(want) == ""
	var unnamed *meta.File
	for _, f := range same {
		got := stripCopySuffix(f.Name)
		if noExt {
			got = strings.TrimSuffix(got, filepath.Ext(got))
		}
		if strings.EqualFold(got, want) {
			return f, nil
		}
		if unnamed == nil && placeholderName.MatchString(f.Name) {
			unnamed = f
		}
	}
	if unnamed == nil {
		return nil, nil
	}
	newName := name
	if noExt {
		newName += filepath.Ext(unnamed.Name)
	}
	switch err := s.svc.Store().RenameFile(ctx, s.userID, unnamed.ID, newName); {
	case err == nil:
		s.log.Info("renamed an upload that came without a file name", "from", unnamed.Name, "to", newName)
		unnamed.Name = newName
	case errors.Is(err, meta.ErrConflict): // 真名已经被别的文件占了：保留原来的名字，照样当作传过
	default:
		return nil, err
	}
	return unnamed, nil
}

// placeholderName 是以前没给文件名时自动起的名字：“照片 2026-10-16 035540-982f.mp4”。
var placeholderName = regexp.MustCompile(`^照片 \d{4}-\d{2}-\d{2} \d{6}-[0-9a-f]{4}(\.[A-Za-z0-9]+)?$`)

// GET /api/ingest/exists（头部 X-File-Name、X-File-Size、X-Xiaoju-Album，同上传）：这个文件是不是已经传过了。
// 快捷指令先问一下，传过的就不用再把整个文件发一遍（中途停了再运行时省时间、省流量）。
// 返回 {"result":"exists"} 或 {"result":"missing"}；大小看不懂（比如带单位）时一律回答 missing，照常上传，不会漏传。
// 只透露“同名同大小的文件在不在”，不能列目录、下载。
func (s *Server) ingestExists(w http.ResponseWriter, r *http.Request) {
	album, ok := s.ingestAlbum(w, r)
	if !ok {
		return
	}
	missing := map[string]string{"result": "missing"}
	size := parseByteCount(r.Header.Get("X-File-Size"))
	name, named := ingestName(r, "")
	if album == nil || size <= 0 || !named {
		writeJSON(w, http.StatusOK, missing)
		return
	}
	f, err := s.uploadedBefore(r.Context(), album.ID, name, size)
	if err != nil {
		s.fail(w, err)
		return
	}
	if f == nil {
		writeJSON(w, http.StatusOK, missing)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "exists", "name": f.Name})
}

// GET /api/ingest/names（头部 X-Xiaoju-Album）：相册里每个文件“名字|字节数”的 SHA-256（小写十六进制，一行一个；
// 名字带自动加的“ (2)”时，去掉序号的也给一行）。快捷指令开始时取一次，之后每个文件取名字和大小、算 SHA-256
// 在手机上比对，传过的直接跳过，不用每个文件都问服务端（像断点续传一样快）。比对带上大小，换过手机后编号重复的
// 不同照片不会被当成传过（对不上的照常上传，由上传接口按名字 + 大小判断，同名不同内容自动改名）。
// 只给摘要：知道名字和大小的人能确认文件在不在（和 exists 一样），但列不出相册里有哪些文件。
func (s *Server) ingestNames(w http.ResponseWriter, r *http.Request) {
	album, ok := s.ingestAlbum(w, r)
	if !ok {
		return
	}
	var files []meta.NameSize
	if album != nil {
		var err error
		if files, err = s.svc.Store().ListNameSizes(r.Context(), s.userID, album.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	// 快捷指令每次运行开始时取一次：看得出它开始跑了
	s.log.Info("ingest names requested", "album", r.Header.Get("X-Xiaoju-Album"), "files", len(files), "client", clientIP(r))
	var b strings.Builder
	for _, f := range files {
		b.WriteString(nameSizeHash(f.Name, f.Size))
		b.WriteByte('\n')
		if base := stripCopySuffix(f.Name); base != f.Name {
			b.WriteString(nameSizeHash(base, f.Size))
			b.WriteByte('\n')
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, b.String())
}

// nameSizeHash 是快捷指令比对用的摘要：SHA-256("名字|字节数")，小写十六进制。
func nameSizeHash(name string, size int64) string {
	sum := sha256.Sum256([]byte(name + "|" + strconv.FormatInt(size, 10)))
	return hex.EncodeToString(sum[:])
}

// ingestAlbum 找 X-Xiaoju-Album 指的文件夹（不新建；还没有时返回 nil）。相册名不对时回 400，ok 为 false。
func (s *Server) ingestAlbum(w http.ResponseWriter, r *http.Request) (album *meta.File, ok bool) {
	folder, ok := ingestAlbums[strings.ToLower(strings.TrimSpace(r.Header.Get("X-Xiaoju-Album")))]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown album")
		return nil, false
	}
	album, err := s.svc.Store().FindFolder(r.Context(), s.userID, nil, folder)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, true
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return album, true
}

// parseByteCount 读字节数：只允许数字和千位分隔符（“12,345”“12 345”），其他（“1.2 MB”）返回 0。
func parseByteCount(v string) int64 {
	v = strings.TrimSpace(v)
	digits := strings.Map(func(c rune) rune {
		switch {
		case c >= '0' && c <= '9':
			return c
		case c == ',' || c == ' ' || c == '\'' || c == '\u00a0' || c == '\u202f':
			return -1
		}
		return 'x'
	}, v)
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
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

// albumFolder 返回根目录下名为 name 的文件夹（“相册”或“隐藏相册”），没有就新建。
func (s *Server) albumFolder(r *http.Request, name string) (*meta.File, error) {
	store := s.svc.Store()
	f, err := store.FindFolder(r.Context(), s.userID, nil, name)
	if errors.Is(err, meta.ErrNotFound) {
		f, err = store.CreateFolder(r.Context(), s.userID, nil, name)
		if errors.Is(err, meta.ErrConflict) { // 同时有别的请求建好了
			f, err = store.FindFolder(r.Context(), s.userID, nil, name)
		}
	}
	return f, err
}

// ingestName 取文件名：?name= 或 X-File-Name（可以是 URL 编码的），去掉路径分隔符；
// 没有扩展名时按文件内容补上（快捷指令给的照片名称通常没有扩展名）。没给名字（或只有“.”）时 ok 为 false。
func ingestName(r *http.Request, ext string) (name string, ok bool) {
	name = r.URL.Query().Get("name")
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
	if strings.Trim(name, ".") == "" {
		return "", false
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
	return name, true
}

var copySuffix = regexp.MustCompile(` \(\d+\)(\.[^.]*)?$`)

// stripCopySuffix 去掉自动加的序号：“IMG_1 (2).jpg” → “IMG_1.jpg”（和网页同步相册的规则一样）。
func stripCopySuffix(n string) string { return copySuffix.ReplaceAllString(n, "$1") }

func withCopySuffix(n string, i int) string {
	ext := filepath.Ext(n)
	return strings.TrimSuffix(n, ext) + " (" + strconv.Itoa(i) + ")" + ext
}
