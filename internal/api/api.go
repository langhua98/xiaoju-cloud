// Package api 提供 HTTP 接口。MVP 阶段只有一个管理员用户，用 Bearer Token 认证。
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// downloadLinkTTL 是签名下载链接的有效期。
const downloadLinkTTL = 6 * time.Hour

type Server struct {
	svc         *service.Service
	userID      uuid.UUID
	token       string
	corsOrigins []string
	log         *slog.Logger
}

type Options struct {
	UserID uuid.UUID
	Token  string
	// CORSOrigins 允许跨域调用 /api 的来源（如 GitHub Pages 上的前端），"*" 表示任意来源。
	// 认证只走请求头、不用 Cookie，所以放开来源不会带来 CSRF 风险。
	CORSOrigins []string
	Log         *slog.Logger
}

func New(svc *service.Service, opt Options) *Server {
	return &Server{svc: svc, userID: opt.UserID, token: opt.Token, corsOrigins: opt.CORSOrigins, log: opt.Log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("xiaoju-cloud is running\n")) })

	api := http.NewServeMux()
	api.HandleFunc("POST /api/folders", s.createFolder)
	api.HandleFunc("GET /api/folders/{id}/children", s.listChildren)
	api.HandleFunc("POST /api/files", s.upload)
	api.HandleFunc("GET /api/files/{id}", s.getFile)
	api.HandleFunc("GET /api/files/{id}/content", s.download)
	api.HandleFunc("POST /api/files/{id}/link", s.createLink)
	api.HandleFunc("DELETE /api/files/{id}", s.deleteFile)
	api.HandleFunc("GET /api/album/check", s.albumCheck)
	api.HandleFunc("POST /api/album/upload", s.albumUpload)
	mux.Handle("/api/", s.cors(s.auth(api)))
	// 签名下载链接不需要请求头认证，浏览器可以直接打开、<video> 可以直接播放。
	mux.HandleFunc("GET /dl/{id}", s.signedDownload)
	return mux
}

// cors 处理跨域请求；预检请求（OPTIONS）不带认证头，必须在 auth 之前直接放行。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && (slices.Contains(s.corsOrigins, "*") || slices.Contains(s.corsOrigins, origin))
		if allowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, Content-Range")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allowed {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, X-Xiaoju-Token, Content-Type, Range")
				h.Set("Access-Control-Max-Age", "86400")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 私有 Hugging Face Space 的 Authorization 头被 HF 自己的 token 占用，所以也接受 X-Xiaoju-Token。
		got := r.Header.Get("X-Xiaoju-Token")
		if got == "" {
			got, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		// iOS 快捷指令里粘贴的请求头常带上看不见的换行，整个请求会被系统直接丢弃，
		// 所以相册同步接口也接受网址参数 ?token=。其余接口仍只认请求头，避免令牌出现在网址里。
		album := strings.HasPrefix(r.URL.Path, "/api/album/")
		if got == "" && album {
			got = albumQuery(r).Get("token")
		}
		got = strings.TrimSpace(got)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			// 只记录长度，不记录令牌内容，便于排查复制时多带了空格等问题
			attrs := []any{"method", r.Method, "path", r.URL.Path, "token_len", len(got), "want_len", len(s.token)}
			if album {
				attrs = append(attrs, "query", redactQuery(r.URL.RawQuery))
			}
			s.log.Warn("unauthorized", attrs...)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createFolderReq struct {
	ParentID *uuid.UUID `json:"parent_id"`
	Name     string     `json:"name"`
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req createFolderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := validName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := s.svc.Store().CreateFolder(r.Context(), s.userID, req.ParentID, req.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// GET /api/folders/root/children 列出根目录。
func (s *Server) listChildren(w http.ResponseWriter, r *http.Request) {
	var parent *uuid.UUID
	if id := r.PathValue("id"); id != "root" {
		u, err := uuid.Parse(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid folder id")
			return
		}
		parent = &u
	}
	items, err := s.svc.Store().ListChildren(r.Context(), s.userID, parent)
	if err != nil {
		s.fail(w, err)
		return
	}
	if items == nil {
		items = []*meta.File{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// POST /api/files?name=xxx[&parent_id=uuid]，请求体是文件原始内容（流式，不走 multipart）。
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if err := validName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var parent *uuid.UUID
	if p := q.Get("parent_id"); p != "" {
		u, err := uuid.Parse(p)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid parent_id")
			return
		}
		parent = &u
	}
	ct := r.Header.Get("Content-Type")
	switch ct {
	case "", "application/octet-stream", "application/x-www-form-urlencoded": // curl -d 默认值，不可信
		ct = "application/octet-stream"
		if guess := mime.TypeByExtension(filepath.Ext(name)); guess != "" {
			ct = guess
		}
	}
	f, err := s.svc.Upload(r.Context(), service.UploadRequest{
		UserID: s.userID, ParentID: parent, Name: name, Mime: ct, Body: r.Body,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := s.svc.Store().Get(r.Context(), s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// 下载支持 Range（断点续传、视频拖动），由 http.ServeContent 处理。
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	s.serveContent(w, r, id)
}

// POST /api/files/{id}/link 生成限时有效的下载链接 /dl/{id}?exp=..&sig=..（相对路径），
// 加 &inline=1 可在浏览器内预览。
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := s.svc.Store().Get(r.Context(), s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	exp := time.Now().Add(downloadLinkTTL).Unix()
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        fmt.Sprintf("/dl/%s?exp=%d&sig=%s", id, exp, s.linkSig(id, exp)),
		"expires_at": time.Unix(exp, 0).UTC(),
	})
}

// GET /dl/{id}?exp=..&sig=..[&inline=1]
func (s *Server) signedDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp ||
		!hmac.Equal([]byte(q.Get("sig")), []byte(s.linkSig(id, exp))) {
		writeError(w, http.StatusForbidden, "invalid or expired link")
		return
	}
	s.serveContent(w, r, id)
}

// linkSig 用管理员令牌签名，令牌更换后旧链接全部失效。
func (s *Server) linkSig(id uuid.UUID, exp int64) string {
	m := hmac.New(sha256.New, []byte("xiaoju download link\x00"+s.token))
	fmt.Fprintf(m, "%s\n%d", id, exp)
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) serveContent(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	f, rd, err := s.svc.Open(r.Context(), s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rd.Close()
	if f.Mime != "" {
		w.Header().Set("Content-Type", f.Mime)
	}
	disposition := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
	http.ServeContent(w, r, f.Name, f.UpdatedAt, rd)
}

// DELETE /api/files/{id} 移入回收站；?permanent=1 永久删除（同时删除 Telegram 消息，仅限文件）。
func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var err error
	if permanent, _ := strconv.ParseBool(r.URL.Query().Get("permanent")); permanent {
		err = s.svc.Purge(r.Context(), s.userID, id)
	} else {
		err = s.svc.Store().Trash(r.Context(), s.userID, id)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, meta.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrFolderPurge):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len(name) > 255 {
		return errors.New("invalid name")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
