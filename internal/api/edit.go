package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// PATCH /api/files/{id}：重命名、移动，或者把文件夹设成 / 取消私密。请求体里只放要改的：
//
//	{"name": "新名字"}  {"parent_id": "uuid"}（null 是根目录）  {"private": true}
//
// 目标文件夹里重名返回 409；文件夹不能移进它自己里面，不是文件夹不能设私密，都返回 400。
func (s *Server) patchFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var u meta.FileUpdate
	if raw, ok := body["name"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil || validName(name) != nil {
			writeError(w, http.StatusBadRequest, "invalid name")
			return
		}
		u.Name = &name
	}
	if raw, ok := body["parent_id"]; ok {
		var parent *uuid.UUID
		if err := json.Unmarshal(raw, &parent); err != nil {
			writeError(w, http.StatusBadRequest, "invalid parent_id")
			return
		}
		u.Move, u.ParentID = true, parent
	}
	if raw, ok := body["private"]; ok {
		var private bool
		if err := json.Unmarshal(raw, &private); err != nil {
			writeError(w, http.StatusBadRequest, "invalid private")
			return
		}
		u.Private = &private
	}
	f, err := s.svc.Store().UpdateFile(r.Context(), s.userID, id, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// POST /api/files/{id}/restore：从回收站放回原处（原来的文件夹也删了时放回根目录）。原处有同名的返回 409。
func (s *Server) restoreFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := s.svc.Store().Restore(r.Context(), s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// GET /api/trash：回收站里的条目（最近删的在前）。永久删除用 DELETE /api/files/{id}?permanent=1。
func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Store().ListTrash(r.Context(), s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if items == nil {
		items = []*meta.File{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /api/backup/phone：手机上的照片备份到哪一步了。每个相册文件夹（“相册”“隐藏相册”）：网盘里有几个文件、
// 最近一次上传的时间，以及快捷指令最近一次清单（手机上有几个、当时已有几个、之后又传了几个）。
func (s *Server) phoneBackup(w http.ResponseWriter, r *http.Request) {
	albums, err := s.svc.Store().PhoneBackup(r.Context(), s.userID, []string{AlbumFolder, HiddenAlbumFolder})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albums})
}
