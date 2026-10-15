package api

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 批量上传（iPhone 快捷指令等）：用“只能上传”的密钥把照片、视频直接传进根目录的“相册”文件夹。
// 密钥不能列目录、下载或删除，所以不会削弱刷脸和格子口令对查看的保护。

// AlbumFolder 是同步相册用的文件夹（和网页的“同步相册”是同一个）。
const AlbumFolder = "相册"

// POST /api/ingest[?name=IMG_0001.HEIC]
//
//	头部 X-Xiaoju-Upload-Key: xju_…（或 Authorization: Bearer xju_…）
//	头部 X-File-Name: 文件名（也可以放在 ?name=；没有扩展名时按文件内容补上）
//	请求体：文件原始内容
//
// 文件名去掉自动加的序号后相同、大小也相同的视为已经传过，直接跳过：
// 返回 200 {"result":"skipped"}；新上传返回 201 {"result":"uploaded"}。同名不同内容自动改名为“名字 (2)”。
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	key, ok := s.uploadKey(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or revoked upload key", "code": "upload_key_invalid"})
		return
	}
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
				s.touchUploadKey(r, key, false)
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
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n = withCopySuffix(name, i)
		}
		f, err := s.svc.Upload(ctx, service.UploadRequest{UserID: s.userID, ParentID: &album.ID, Name: n, Mime: mimeType, Body: body})
		if errors.Is(err, meta.ErrConflict) && i < 100 {
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		s.touchUploadKey(r, key, true)
		s.log.Info("ingested", "key", key.Name, "name", f.Name, "size", f.Size)
		writeJSON(w, http.StatusCreated, map[string]any{"result": "uploaded", "name": f.Name, "size": f.Size})
		return
	}
}

// uploadKey 校验请求带的上传密钥。
func (s *Server) uploadKey(r *http.Request) (*meta.UploadKey, bool) {
	k := r.Header.Get("X-Xiaoju-Upload-Key")
	if k == "" {
		k, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	k = strings.TrimSpace(k)
	if !strings.HasPrefix(k, auth.UploadKeyPrefix) {
		return nil, false
	}
	key, err := s.svc.Store().FindUploadKey(r.Context(), auth.HashUploadKey(k))
	if err != nil {
		if !errors.Is(err, meta.ErrNotFound) {
			s.log.Error("find upload key", "err", err)
		}
		return nil, false
	}
	return key, true
}

func (s *Server) touchUploadKey(r *http.Request, key *meta.UploadKey, uploaded bool) {
	if err := s.svc.Store().TouchUploadKey(r.Context(), key.ID, uploaded); err != nil {
		s.log.Warn("touch upload key", "err", err)
	}
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

// ---------- 管理上传密钥（需要会话） ----------

func (s *Server) listUploadKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.svc.Store().ListUploadKeys(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": keys, "endpoint": "/api/ingest"})
}

// POST /api/upload-keys {name}：新建上传密钥。密钥只在这次返回，之后只能作废重建。
func (s *Server) createUploadKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "iPhone 快捷指令"
	}
	if utf8.RuneCountInString(name) > 60 {
		name = string([]rune(name)[:60])
	}
	plain, hash, err := auth.NewUploadKey()
	if err != nil {
		s.fail(w, err)
		return
	}
	k, err := s.svc.Store().CreateUploadKey(r.Context(), name, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("upload key created", "name", name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "created_at": k.CreatedAt, "key": plain, "endpoint": "/api/ingest",
	})
}

func (s *Server) deleteUploadKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store().DeleteUploadKey(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("upload key revoked", "id", id)
	w.WriteHeader(http.StatusNoContent)
}
