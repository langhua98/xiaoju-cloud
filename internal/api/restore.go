package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 恢复到手机：换新手机或误删后，手机 App 把网盘里的「相册」「隐藏相册」「手机数据」下载回来，照片存回相册、
// 通讯录导回通讯录。
//
// App 自己不能登录（刷脸要关联域名，自签名的 App 没有），所以在网页上（已登录）点「恢复到手机」：
// 重新刷脸（注册了刷脸设备时），开了格子口令再答对一道题，换一个 24 小时有效的恢复凭证，网页跳到
// xiaojubackup://restore?server=…&token=… 交给 App。凭证只能列出、下载这三个文件夹里的文件。

// restoreRoots：恢复凭证能看的根目录文件夹。
var restoreRoots = []string{AlbumFolder, HiddenAlbumFolder, PhoneDataFolder}

// restoreMaxFiles：一次列出的文件上限（防止意外的超大目录拖垮服务）。
const restoreMaxFiles = 200000

// POST /api/restore/grant {ticket, id, answer | timeout}：签发恢复凭证 {token, expires_at}。
func (s *Server) restoreGrant(w http.ResponseWriter, r *http.Request) {
	var req gridAnswerReq
	if !decodeBody(w, r, &req) {
		return
	}
	if s.passkeyRequired() && !s.sessions.Ticket(req.Ticket) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "face verification required first", "code": "passkey_required"})
		return
	}
	if s.gridRequired() && !s.checkAnswer(w, r, req) {
		return
	}
	tok, exp := s.sessions.IssueRestore()
	s.log.Warn("restore token issued", "client", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp})
}

func (s *Server) requireRestore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.Restore(r.Header.Get("X-Xiaoju-Restore")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

type restoreItem struct {
	ID   uuid.UUID `json:"id"`
	Path string    `json:"path"` // 从根目录文件夹算起，如 "相册/旅行/IMG_0001.HEIC"
	Size int64     `json:"size"`
	Mime string    `json:"mime"`
}

// GET /api/restore/list?folder=相册|隐藏相册|手机数据：列出文件夹里（含子文件夹）的所有文件。
// 文件夹还没有时返回空列表。
func (s *Server) restoreList(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if !isRestoreRoot(folder) {
		writeError(w, http.StatusBadRequest, "unknown folder")
		return
	}
	store := s.svc.Store()
	root, err := store.FindFolder(r.Context(), s.userID, nil, folder)
	if errors.Is(err, meta.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []restoreItem{}})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	items := []restoreItem{}
	var walk func(ctx context.Context, id uuid.UUID, prefix string) error
	walk = func(ctx context.Context, id uuid.UUID, prefix string) error {
		children, err := store.ListChildren(ctx, s.userID, &id)
		if err != nil {
			return err
		}
		for _, c := range children {
			path := prefix + "/" + c.Name
			if c.Type == meta.TypeFolder {
				if err := walk(ctx, c.ID, path); err != nil {
					return err
				}
				continue
			}
			if len(items) >= restoreMaxFiles {
				return errors.New("too many files")
			}
			items = append(items, restoreItem{ID: c.ID, Path: path, Size: c.Size, Mime: c.Mime})
		}
		return nil
	}
	if err := walk(r.Context(), root.ID, folder); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GET /api/restore/files/{id}：下载一个文件（只能是那三个文件夹里的）。
func (s *Server) restoreDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, err := s.underRestoreRoot(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !in {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.serveContent(w, r, id)
}

// underRestoreRoot 沿着上级文件夹往上找，看文件是不是在 restoreRoots 里。
func (s *Server) underRestoreRoot(ctx context.Context, id uuid.UUID) (bool, error) {
	store := s.svc.Store()
	f, err := store.Get(ctx, s.userID, id)
	if err != nil {
		return false, err
	}
	if f.Type != meta.TypeFile {
		return false, nil
	}
	for depth := 0; f.ParentID != nil && depth < 64; depth++ {
		if f, err = store.Get(ctx, s.userID, *f.ParentID); err != nil {
			return false, err
		}
	}
	return f.ParentID == nil && f.Type == meta.TypeFolder && isRestoreRoot(f.Name), nil
}

func isRestoreRoot(name string) bool {
	for _, n := range restoreRoots {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	return false
}
