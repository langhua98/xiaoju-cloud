package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/gdrive"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 谷歌云盘同步（见 internal/gdrive）：网页上连接谷歌账号、挑要同步的文件夹、看同步状态。
//
// 授权：网页先要一个谷歌授权地址（POST /api/gdrive/connect），在新窗口打开，用户点同意后谷歌跳回
// /gdrive/callback（不需要认证，靠一次性的 state 认人），服务端换到 refresh token 存好，页面提示可以关掉了；
// 网页这边隔几秒问一次状态，连上了就刷新。

const gdriveStateTTL = 15 * time.Minute

type gdriveStates struct {
	mu sync.Mutex
	m  map[string]gdriveState
}

type gdriveState struct {
	redirect string
	expires  time.Time
}

func (g *gdriveStates) add(redirect string) string {
	b := make([]byte, 24)
	rand.Read(b)
	st := hex.EncodeToString(b)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]gdriveState{}
	}
	for k, v := range g.m {
		if time.Now().After(v.expires) {
			delete(g.m, k)
		}
	}
	g.m[st] = gdriveState{redirect: redirect, expires: time.Now().Add(gdriveStateTTL)}
	return st
}

// take 取出 state 对应的回调地址，一个 state 只能用一次。
func (g *gdriveStates) take(st string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.m[st]
	delete(g.m, st)
	if !ok || time.Now().After(v.expires) {
		return "", false
	}
	return v.redirect, true
}

func (s *Server) gdriveRoutes(files func(string, http.HandlerFunc), mux *http.ServeMux) {
	files("GET /api/gdrive", s.gdriveStatus)
	files("POST /api/gdrive/connect", s.gdriveConnect)
	files("DELETE /api/gdrive", s.gdriveDisconnect)
	files("GET /api/gdrive/folders", s.gdriveFolders)
	files("POST /api/gdrive/links", s.gdriveCreateLink)
	files("DELETE /api/gdrive/links/{id}", s.gdriveDeleteLink)
	files("POST /api/gdrive/links/{id}/resume", s.gdriveResume)
	files("POST /api/gdrive/sync", s.gdriveSyncNow)
	mux.HandleFunc("GET /gdrive/callback", s.limited(s.shareUnlockLimit, s.gdriveCallback))
}

// gdriveRedirect 是谷歌授权完跳回来的地址（要原样填进 Google Cloud 的 OAuth 客户端里）。
func (s *Server) gdriveRedirect(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL + "/gdrive/callback"
	}
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.Split(p, ",")[0]
	} else if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")) {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/gdrive/callback"
}

type gdriveLinkView struct {
	*meta.GDriveLink
	FolderName string `json:"folder_name"`
	FolderPath string `json:"folder_path"`
}

func (s *Server) gdriveStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{
		"configured":   s.gdrive.Configured(),
		"connected":    false,
		"redirect_uri": s.gdriveRedirect(r),
		"links":        []gdriveLinkView{},
	}
	if !s.gdrive.Configured() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["syncing"] = s.gdrive.Running()
	if t := s.gdrive.LastCheck(); !t.IsZero() {
		out["last_check"] = t
	}
	acct, err := s.svc.Store().GetGDriveAccount(ctx, s.userID)
	if errors.Is(err, meta.ErrNotFound) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out["connected"] = true
	out["email"] = acct.Email
	out["error"] = acct.Error
	links, err := s.svc.Store().ListGDriveLinks(ctx, s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	views := []gdriveLinkView{}
	for _, l := range links {
		v := gdriveLinkView{GDriveLink: l}
		if chain, err := s.svc.Store().Ancestry(ctx, s.userID, l.FolderID); err == nil && len(chain) > 0 {
			v.FolderName = chain[0].Name
			v.FolderPath = chainPath(chain, nil)
		}
		views = append(views, v)
	}
	out["links"] = views
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) gdriveConnect(w http.ResponseWriter, r *http.Request) {
	if !s.gdrive.Configured() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": gdrive.ErrNotConfigured.Error(), "code": "not_configured"})
		return
	}
	redirect := s.gdriveRedirect(r)
	writeJSON(w, http.StatusOK, map[string]string{"url": s.gdrive.OAuth.AuthURL(redirect, s.gdriveAuth.add(redirect))})
}

func (s *Server) gdriveCallback(w http.ResponseWriter, r *http.Request) {
	page := func(status int, title, msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		w.Write([]byte(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + html.EscapeString(title) + `</title><style>body{font:17px -apple-system,system-ui,sans-serif;margin:0;min-height:100vh;display:grid;place-items:center;
background:#0f1115;color:#e8e8ea;text-align:center;padding:24px}h1{font-size:22px}p{color:#a0a3ab;line-height:1.6}</style></head>
<body><div><h1>` + html.EscapeString(title) + `</h1><p>` + html.EscapeString(msg) + `</p></div></body></html>`))
	}
	if !s.gdrive.Configured() {
		page(http.StatusConflict, "没有配置谷歌云盘同步", "服务端还没有填 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET。")
		return
	}
	q := r.URL.Query()
	redirect, ok := s.gdriveAuth.take(q.Get("state"))
	if !ok {
		page(http.StatusBadRequest, "链接过期了", "请回到小橘云盘，重新点「连接谷歌云盘」。")
		return
	}
	if e := q.Get("error"); e != "" {
		page(http.StatusOK, "没有连接", "你在谷歌那边取消了授权（"+e+"）。可以回到小橘云盘重新连接。")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	refresh, err := s.gdrive.OAuth.Exchange(ctx, q.Get("code"), redirect)
	if err != nil {
		s.log.Warn("gdrive connect", "err", err)
		page(http.StatusBadGateway, "连接失败", "谷歌没有给授权："+err.Error())
		return
	}
	email, err := s.gdrive.OAuth.Client(refresh).Email(ctx)
	if err != nil {
		s.log.Warn("gdrive connect: read account", "err", err)
	}
	sealed, err := s.gdrive.SealRefresh(refresh)
	if err == nil {
		err = s.svc.Store().SaveGDriveAccount(ctx, s.userID, email, sealed)
	}
	if err != nil {
		s.log.Error("gdrive connect: save", "err", err)
		page(http.StatusInternalServerError, "连接失败", "保存授权时出错了，请再试一次。")
		return
	}
	s.log.Info("gdrive connected")
	s.gdrive.Kick()
	page(http.StatusOK, "✅ 谷歌云盘已连接", "可以关掉这个页面，回到小橘云盘选要同步的文件夹了。")
}

func (s *Server) gdriveDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Store().DeleteGDriveAccount(r.Context(), s.userID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gdriveFolders 列出谷歌云盘里一个文件夹（默认“我的云盘”）里的子文件夹。
func (s *Server) gdriveFolders(w http.ResponseWriter, r *http.Request) {
	c, err := s.gdriveClient(w, r)
	if err != nil {
		return
	}
	parent := r.URL.Query().Get("parent")
	if parent == "" {
		parent = "root"
	}
	ctx := r.Context()
	self, err := c.Get(ctx, parent)
	if err != nil {
		s.gdriveFail(w, err)
		return
	}
	kids, err := c.Folders(ctx, parent)
	if err != nil {
		s.gdriveFail(w, err)
		return
	}
	type folder struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	items := []folder{}
	for _, k := range kids {
		items = append(items, folder{k.ID, k.Name})
	}
	up := ""
	if parent != "root" && len(self.Parents) > 0 {
		up = self.Parents[0]
	}
	name := self.Name
	if parent == "root" {
		name = "我的云盘"
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": self.ID, "name": name, "parent": up, "root": parent == "root", "items": items})
}

func (s *Server) gdriveClient(w http.ResponseWriter, r *http.Request) (*gdrive.Client, error) {
	c, err := s.gdrive.Client(r.Context())
	switch {
	case errors.Is(err, gdrive.ErrNotConfigured):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "not_configured"})
	case errors.Is(err, meta.ErrNotFound):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "google drive is not connected", "code": "not_connected"})
	case err != nil:
		s.fail(w, err)
	}
	return c, err
}

func (s *Server) gdriveFail(w http.ResponseWriter, err error) {
	var api *gdrive.APIError
	switch {
	case errors.Is(err, gdrive.ErrRevoked):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "revoked"})
	case gdrive.IsNotFound(err):
		writeError(w, http.StatusNotFound, "google drive folder not found")
	case errors.As(err, &api):
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.fail(w, err)
	}
}

// gdriveCreateLink 新建一对同步：{"gdrive_id"} 加上网盘这边的 {"folder_id"}（用已有的文件夹），
// 或者 {"parent_id"}（在这里新建一个和谷歌文件夹同名的文件夹，不填是根目录）。
func (s *Server) gdriveCreateLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GDriveID string     `json:"gdrive_id"`
		FolderID *uuid.UUID `json:"folder_id"`
		ParentID *uuid.UUID `json:"parent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GDriveID == "" {
		writeError(w, http.StatusBadRequest, "gdrive_id is required")
		return
	}
	c, err := s.gdriveClient(w, r)
	if err != nil {
		return
	}
	ctx := r.Context()
	g, err := c.Get(ctx, req.GDriveID)
	if err == nil && (!g.IsFolder() || g.Trashed) {
		err = &gdrive.APIError{Status: http.StatusNotFound, Message: "not a folder"}
	}
	if err != nil {
		s.gdriveFail(w, err)
		return
	}
	if req.GDriveID == "root" || g.ID == "" {
		writeError(w, http.StatusBadRequest, "pick a folder inside My Drive, not My Drive itself")
		return
	}
	store := s.svc.Store()
	links, err := store.ListGDriveLinks(ctx, s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var folder *meta.File
	if req.FolderID != nil {
		if folder, err = store.Get(ctx, s.userID, *req.FolderID); err == nil && (folder.Type != meta.TypeFolder || folder.TrashedAt != nil) {
			err = meta.ErrNotFound
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	// 不能和已有的同步重叠（一个文件夹在另一个同步的里面），否则同一个文件会被同步两遍
	for _, l := range links {
		if l.GDriveID == g.ID {
			writeError(w, http.StatusConflict, "this google drive folder is already synced")
			return
		}
		if folder != nil {
			if overlap, err := s.foldersOverlap(ctx, folder.ID, l.FolderID); err != nil {
				s.fail(w, err)
				return
			} else if overlap {
				writeError(w, http.StatusConflict, "this folder overlaps a folder that is already synced")
				return
			}
		}
	}
	if folder == nil {
		used := map[string]bool{}
		kids, err := store.ListChildren(ctx, s.userID, req.ParentID)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, k := range kids {
			used[k.Name] = true
		}
		name := gdrive.LocalName(g.Name)
		for i := 2; used[name]; i++ {
			name = gdrive.LocalName(g.Name) + " (" + strconv.Itoa(i) + ")"
		}
		if folder, err = store.CreateFolder(ctx, s.userID, req.ParentID, name); err != nil {
			s.fail(w, err)
			return
		}
		for _, l := range links {
			if overlap, err := s.foldersOverlap(ctx, folder.ID, l.FolderID); err == nil && overlap {
				_ = store.DeleteRow(ctx, folder.ID)
				writeError(w, http.StatusConflict, "this folder overlaps a folder that is already synced")
				return
			}
		}
	}
	l, err := store.CreateGDriveLink(ctx, s.userID, g.ID, g.Name, folder.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.gdrive.Kick()
	writeJSON(w, http.StatusCreated, gdriveLinkView{GDriveLink: l, FolderName: folder.Name})
}

// foldersOverlap 报告两个文件夹是不是同一个、或者一个在另一个里面。
func (s *Server) foldersOverlap(ctx context.Context, a, b uuid.UUID) (bool, error) {
	for _, pair := range [][2]uuid.UUID{{a, b}, {b, a}} {
		chain, err := s.svc.Store().Ancestry(ctx, s.userID, pair[0])
		if errors.Is(err, meta.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, f := range chain {
			if f.ID == pair[1] {
				return true, nil
			}
		}
	}
	return false, nil
}

// gdriveDeleteLink 停止同步一对文件夹（两边的文件都不动）。
func (s *Server) gdriveDeleteLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store().DeleteGDriveLink(r.Context(), s.userID, id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gdriveResume 继续暂停了的同步；{"allow_deletes": true} 表示确认过“一次删很多”。
func (s *Server) gdriveResume(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		AllowDeletes bool `json:"allow_deletes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ctx := r.Context()
	if err := s.svc.Store().SetGDriveLinkPaused(ctx, s.userID, id, false, ""); err != nil {
		s.fail(w, err)
		return
	}
	if req.AllowDeletes {
		s.gdrive.AllowDeletes(id)
	}
	_ = s.svc.Store().ResetGDriveFingerprint(ctx, id)
	s.gdrive.Kick()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) gdriveSyncNow(w http.ResponseWriter, r *http.Request) {
	if !s.gdrive.Configured() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": gdrive.ErrNotConfigured.Error(), "code": "not_configured"})
		return
	}
	s.gdrive.Kick()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
