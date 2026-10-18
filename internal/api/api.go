// Package api 提供 HTTP 接口。MVP 阶段只有一个管理员用户，用 Bearer Token 认证。
package api

import (
	"crypto/hmac"
	"crypto/sha256"
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

	"github.com/langhua98/xiaoju-cloud/internal/auth"
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
	passkeys    *auth.Passkeys
	grid        *auth.Grid
	sessions    *auth.Sessions
	views       *auth.Views // 每个会话看了几张照片、视频（格子口令抽查用）
}

type Options struct {
	UserID uuid.UUID
	Token  string
	// CORSOrigins 允许跨域调用 /api 的来源（如 GitHub Pages 上的前端），"*" 表示任意来源。
	// 认证只走请求头、不用 Cookie，所以放开来源不会带来 CSRF 风险。
	CORSOrigins []string
	Log         *slog.Logger
	// Passkeys 为 nil 时不启用刷脸（仅凭令牌访问）。
	Passkeys *auth.Passkeys
	// Grid 为 nil 时不支持格子口令。
	Grid *auth.Grid
	// Sessions 为 nil 时用令牌新建（作废名单只在内存里）。
	Sessions *auth.Sessions
}

func New(svc *service.Service, opt Options) *Server {
	sessions := opt.Sessions
	if sessions == nil {
		sessions = auth.NewSessions(opt.Token)
	}
	return &Server{
		svc: svc, userID: opt.UserID, token: opt.Token, corsOrigins: opt.CORSOrigins, log: opt.Log,
		passkeys: opt.Passkeys, grid: opt.Grid, sessions: sessions, views: auth.NewViews(),
	}
}

// 接口按访问级别分三组：
//   - 文件接口：需要会话（令牌 + 刷脸后签发）；刷脸和格子口令都没开启时，仅凭令牌也可以（兼容旧客户端和脚本）。
//     开启格子口令后，照片、视频每看随机 3–7 张要先答一道格子题（mediaGate）
//   - 运维接口（统计、备份状态、对账）：令牌或会话都可以，不涉及文件内容，方便自动化诊断
//   - 登录接口：需要令牌（刷脸的几个接口已登录时用会话也可以）
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("xiaoju-cloud is running\n")) })

	api := http.NewServeMux()
	files := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireSession(h)) }
	ops := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireTokenOrSession(h)) }
	login := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireToken(h)) }
	// 刷脸：登录时用令牌；已登录时（给本机加设备、敏感操作前重新刷脸）用会话也可以
	passkey := func(pattern string, h http.HandlerFunc) { api.Handle(pattern, s.requireTokenOrSession(h)) }

	files("POST /api/folders", s.createFolder)
	files("GET /api/folders/{id}/children", s.listChildren)
	files("POST /api/files", s.upload)
	files("GET /api/files/{id}", s.getFile)
	files("GET /api/files/{id}/content", s.download)
	files("POST /api/files/{id}/link", s.createLink)
	files("DELETE /api/files/{id}", s.deleteFile)
	files("GET /api/auth/passkeys", s.listPasskeys)
	files("DELETE /api/auth/passkeys/{id}", s.deletePasskey)
	files("POST /api/auth/passkeys/add-code", s.newAddCode)
	files("GET /api/auth/grid", s.gridStatus)
	files("POST /api/auth/grid/setup/begin", s.gridSetupBegin)
	files("POST /api/auth/grid/setup/finish", s.gridSetupFinish)
	files("DELETE /api/auth/grid", s.gridDisable)
	files("POST /api/auth/grid/challenge", s.gridChallenge)
	files("POST /api/auth/grid/verify", s.gridVerify)

	ops("GET /api/backup/status", s.backupStatus)
	ops("GET /api/stats", s.stats)
	// 对账：GET 只出报告；POST 执行修复和残块清理
	ops("GET /api/maintenance/reconcile", s.reconcile(false))
	ops("POST /api/maintenance/reconcile", s.reconcile(true))

	login("POST /api/auth/session", s.createSession)
	passkey("POST /api/auth/passkey/login/begin", s.passkeyLoginBegin)
	passkey("POST /api/auth/passkey/login/finish", s.passkeyLoginFinish)
	passkey("POST /api/auth/passkey/register/begin", s.passkeyRegisterBegin)
	passkey("POST /api/auth/passkey/register/finish", s.passkeyRegisterFinish)
	login("POST /api/auth/recovery/passkey", s.recoveryPasskey)

	// 批量上传（快捷指令）：不需要认证，只能往“相册”里添加文件，见 ingest.go
	api.HandleFunc("POST /api/ingest", s.ingest)
	api.HandleFunc("GET /api/ingest/exists", s.ingestExists)
	api.HandleFunc("GET /api/ingest/names", s.ingestNames)
	api.HandleFunc("POST /api/ingest/plan", s.ingestPlan)

	mux.Handle("/api/", s.cors(api))
	// 签名下载链接不需要请求头认证，浏览器可以直接打开、<video> 可以直接播放。
	mux.HandleFunc("GET /dl/{id}", s.signedDownload)
	mux.HandleFunc("GET /thumb/{id}", s.thumbnail)
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
				h.Set("Access-Control-Allow-Headers", "Authorization, X-Xiaoju-Token, X-Xiaoju-Session, Content-Type, Range")
				h.Set("Access-Control-Max-Age", "86400")
			}
			w.WriteHeader(http.StatusNoContent)
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
	out, err := s.withThumbs(r.Context(), items)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
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
	// 视频播放会发很多个 Range 请求：只有从头开始的那个算“看了一张”
	if rg := r.Header.Get("Range"); rg == "" || strings.HasPrefix(rg, "bytes=0-") {
		f, err := s.svc.Store().Get(r.Context(), s.userID, id)
		if err != nil {
			s.fail(w, err)
			return
		}
		if f.Type == meta.TypeFile && !s.mediaGate(w, r, f) {
			return
		}
	}
	s.serveContent(w, r, id)
}

// mediaGate：开启了格子口令时，照片、视频（预览和下载都算）每个会话每看随机 3–7 张，
// 下一张之前要先答对一道格子题（POST /api/auth/grid/challenge、/verify）。
func (s *Server) mediaGate(w http.ResponseWriter, r *http.Request, f *meta.File) bool {
	if !s.gridRequired() || !(strings.HasPrefix(f.Mime, "image/") || strings.HasPrefix(f.Mime, "video/")) {
		return true
	}
	sid, ok := s.sessionID(r)
	if !ok { // 开启格子口令后文件接口必须带会话（requireSession），这里只是兜底
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if s.views.Allow(sid) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "grid passcode check required", "code": "grid_required"})
	return false
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
	if !s.mediaGate(w, r, f) {
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

// GET /api/backup/status 返回双备份是否启用，以及已备份的分块数和字节数。
func (s *Server) backupStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.BackupStatus(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// GET /api/stats 返回整个网盘的统计：文件数（照片、视频、其他）、总大小、已完整备份的文件数。
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Store().DriveStats(r.Context(), s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) reconcile(apply bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.svc.Reconcile(r.Context(), apply)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
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
