// Package api 提供 HTTP 接口。MVP 阶段只有一个管理员用户，用 Bearer Token 认证。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

type Server struct {
	svc    *service.Service
	userID uuid.UUID
	token  string
	log    *slog.Logger
}

func New(svc *service.Service, userID uuid.UUID, token string, log *slog.Logger) *Server {
	return &Server{svc: svc, userID: userID, token: token, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	api := http.NewServeMux()
	api.HandleFunc("POST /api/folders", s.createFolder)
	api.HandleFunc("GET /api/folders/{id}/children", s.listChildren)
	api.HandleFunc("POST /api/files", s.upload)
	api.HandleFunc("GET /api/files/{id}", s.getFile)
	api.HandleFunc("GET /api/files/{id}/content", s.download)
	api.HandleFunc("DELETE /api/files/{id}", s.deleteFile)
	mux.Handle("/api/", s.auth(api))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
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
