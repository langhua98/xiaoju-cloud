package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/scrypt"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// agent 接口的其余部分：分享链接、危险操作申请、自己的审计日志、运维状态。

// POST /shares {"id", "expires_days": 1–7（默认 7）, "password"}：分享一个文件或文件夹。私密文件夹里的不能分享。
func (s *Server) agentCreateShare(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeShares) {
		return
	}
	a := agentFrom(r)
	ctx := r.Context()
	var req struct {
		ID          uuid.UUID `json:"id"`
		Path        string    `json:"path"`
		ExpiresDays int       `json:"expires_days"`
		Password    string    `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		agentError(w, http.StatusBadRequest, "invalid_json", "invalid JSON", "")
		return
	}
	if req.ExpiresDays == 0 {
		req.ExpiresDays = agentMaxShareDays
	}
	if req.ExpiresDays < 1 || req.ExpiresDays > agentMaxShareDays {
		agentError(w, http.StatusBadRequest, "invalid_expiry", fmt.Sprintf("expires_days must be 1–%d", agentMaxShareDays), "Agents cannot create permanent share links.")
		return
	}
	var f *meta.File
	var path string
	var err error
	if req.ID != uuid.Nil {
		f, path, err = s.agentItem(ctx, a, req.ID, false)
	} else if f, err = s.agentPath(ctx, a, req.Path); err == nil && f == nil {
		err = meta.ErrNotFound
	} else if err == nil {
		path = "/" + joinSegs(must(splitPath(req.Path)))
	}
	if err != nil {
		s.agentFail(w, r, err)
		return
	}
	if private, err := s.svc.Store().InPrivateFolder(ctx, s.userID, &f.ID); err != nil {
		s.fail(w, err)
		return
	} else if private || f.Private {
		s.denied(ctx, a, "share", "private", f)
		agentError(w, http.StatusForbidden, "private", "items in private folders cannot be shared by an agent", "Ask the owner to share it from the web page.")
		return
	}
	if !s.quota(w, r, quotaShares, 1) {
		return
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	t := time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
	sh := &meta.Share{UserID: s.userID, FileID: f.ID, Token: base64.RawURLEncoding.EncodeToString(raw), ExpiresAt: &t, AgentTokenID: &a.tok.ID}
	if pw := strings.TrimSpace(req.Password); pw != "" {
		sh.PasswordSalt = make([]byte, 16)
		rand.Read(sh.PasswordSalt)
		if sh.PasswordHash, err = scrypt.Key([]byte(pw), sh.PasswordSalt, 1<<15, 8, 1, 32); err != nil {
			s.fail(w, err)
			return
		}
	}
	out, err := s.svc.Store().CreateShare(ctx, sh)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(ctx, a, "share", []*meta.File{f}, []string{path}, map[string]any{"share_id": out.ID, "expires_days": req.ExpiresDays, "password": sh.HasPassword()}, "ok", "", 0)
	writeJSON(w, http.StatusCreated, agentShareView(out, path))
}

func agentShareView(sh *meta.Share, path string) map[string]any {
	v := map[string]any{"id": sh.ID, "item_id": sh.FileID, "name": sh.File.Name, "expires_at": sh.ExpiresAt,
		"has_password": sh.HasPassword(), "views": sh.Views, "created_at": sh.CreatedAt,
		"web_path": "share.html#" + sh.Token, "about": "Open it at <web page address>/share.html#" + sh.Token}
	if path != "" {
		v["path"] = path
	}
	return v
}

// GET /shares：这把令牌建的分享链接。
func (s *Server) agentListShares(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeShares) {
		return
	}
	a := agentFrom(r)
	shares, err := s.svc.Store().ListAgentShares(r.Context(), s.userID, a.tok.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, len(shares))
	for i, sh := range shares {
		out[i] = agentShareView(sh, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// DELETE /shares/{id}：撤销这把令牌自己建的分享链接。
func (s *Server) agentDeleteShare(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeShares) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a := agentFrom(r)
	if err := s.svc.Store().DeleteAgentShare(r.Context(), s.userID, a.tok.ID, id); err != nil {
		s.agentFail(w, r, err)
		return
	}
	s.audit(r.Context(), a, "unshare", nil, nil, map[string]any{"share_id": id}, "ok", "", 0)
	w.WriteHeader(http.StatusNoContent)
}

// 危险操作：agent 只能申请，网页上人确认（刷脸）后才执行。
const (
	reqPurge      = "purge"       // 永久删除回收站里的这些条目
	reqEmptyTrash = "empty_trash" // 清空回收站（这把令牌看得到的部分）
	reqUnprivate  = "unprivate"   // 把这些文件夹取消私密
	reqReconcile  = "reconcile"   // 对账并修复、清理残块
)

// POST /requests {"action": "purge"|"empty_trash"|"unprivate"|"reconcile", "ids": [...], "reason": "为什么"}
func (s *Server) agentCreateRequest(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeDanger) {
		return
	}
	a := agentFrom(r)
	ctx := r.Context()
	var req struct {
		Action string      `json:"action"`
		IDs    []uuid.UUID `json:"ids"`
		Reason string      `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		agentError(w, http.StatusBadRequest, "invalid_json", "invalid JSON", "")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len([]rune(req.Reason)) > 500 {
		req.Reason = string([]rune(req.Reason)[:500])
	}
	if req.Reason == "" {
		agentError(w, http.StatusBadRequest, "reason_required", "say why in \"reason\"", "The owner sees the reason when deciding.")
		return
	}
	q := &meta.AgentRequest{TokenID: &a.tok.ID, TokenName: a.tok.Name, Action: req.Action, Reason: req.Reason}
	switch req.Action {
	case reqPurge, reqUnprivate:
		if len(req.IDs) == 0 || len(req.IDs) > maxBatchOps {
			agentError(w, http.StatusBadRequest, "invalid_request", "send between 1 and 500 ids", "")
			return
		}
		for _, id := range req.IDs {
			f, p, err := s.agentItem(ctx, a, id, req.Action == reqPurge)
			if err != nil {
				s.agentFail(w, r, err)
				return
			}
			if req.Action == reqPurge && f.TrashedAt == nil {
				agentError(w, http.StatusBadRequest, "not_in_trash", p+" is not in the trash", "Move it to the trash first (DELETE /api/agent/v1/items/{id}).")
				return
			}
			if req.Action == reqUnprivate && (f.Type != meta.TypeFolder || !f.Private) {
				agentError(w, http.StatusBadRequest, "not_private", p+" is not a private folder", "")
				return
			}
			q.FileIDs, q.Paths = append(q.FileIDs, f.ID), append(q.Paths, p)
		}
	case reqEmptyTrash, reqReconcile:
	default:
		agentError(w, http.StatusBadRequest, "invalid_action", "action must be purge, empty_trash, unprivate or reconcile", "")
		return
	}
	out, err := s.svc.Store().CreateAgentRequest(ctx, s.userID, q)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(ctx, a, "request_"+req.Action, nil, q.Paths, map[string]any{"request_id": out.ID, "reason": req.Reason}, "pending", "", 0)
	writeJSON(w, http.StatusAccepted, map[string]any{"request": out,
		"hint": "Nothing happens until the owner approves it on the web page (within 24 hours). Poll GET /api/agent/v1/requests/" + out.ID.String() + "."})
}

// GET /requests/{id}：申请批了没有。
func (s *Server) agentGetRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a := agentFrom(r)
	q, err := s.svc.Store().GetAgentRequest(r.Context(), s.userID, id)
	if err == nil && (q.TokenID == nil || *q.TokenID != a.tok.ID) {
		err = meta.ErrNotFound
	}
	if err != nil {
		s.agentFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": q})
}

// GET /audit?before=&limit=：这把令牌自己做过的操作。
func (s *Server) agentOwnAudit(w http.ResponseWriter, r *http.Request) {
	a := agentFrom(r)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.svc.Store().ListAudit(r.Context(), s.userID, meta.AuditFilter{TokenID: &a.tok.ID, Before: before, Limit: limit})
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, e := range items { // 不给 agent 看 IP、浏览器
		e.IP, e.UserAgent = "", ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) agentOpsBackup(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeOps) {
		return
	}
	st, err := s.svc.BackupStatus(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) agentOpsActivity(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeOps) {
		return
	}
	s.activityStatus(w, r)
}

// runAgentRequest 执行一个批准了的申请，返回给人看的结果。
func (s *Server) runAgentRequest(ctx context.Context, q *meta.AgentRequest) (string, error) {
	store := s.svc.Store()
	switch q.Action {
	case reqPurge:
		n := 0
		for _, id := range q.FileIDs {
			f, err := store.Get(ctx, s.userID, id)
			if errors.Is(err, meta.ErrNotFound) {
				continue // 已经没了
			}
			if err != nil {
				return fmt.Sprintf("deleted %d, then failed", n), err
			}
			if f.TrashedAt == nil {
				continue // 后来被放回去了，不删
			}
			if err := s.svc.Purge(ctx, s.userID, id); err != nil {
				return fmt.Sprintf("deleted %d, then failed", n), err
			}
			n++
		}
		return fmt.Sprintf("permanently deleted %d item(s)", n), nil
	case reqEmptyTrash:
		var tok *meta.AgentToken
		if q.TokenID != nil {
			tok, _ = store.GetAgentToken(ctx, s.userID, *q.TokenID)
		}
		if tok == nil {
			return "", errors.New("the token that asked is gone")
		}
		a := &agentCall{tok: tok}
		if tok.RootFolderID != nil {
			if a.root, _ = store.Get(ctx, s.userID, *tok.RootFolderID); a.root == nil {
				return "", errors.New("the token's folder is gone")
			}
		}
		items, err := store.ListTrash(ctx, s.userID)
		if err != nil {
			return "", err
		}
		n := 0
		for _, f := range items {
			if _, _, err := s.agentItem(ctx, a, f.ID, true); err != nil {
				continue
			}
			if err := s.svc.Purge(ctx, s.userID, f.ID); err != nil && !errors.Is(err, meta.ErrNotFound) {
				return fmt.Sprintf("deleted %d, then failed", n), err
			}
			n++
		}
		return fmt.Sprintf("permanently deleted %d item(s) from the trash", n), nil
	case reqUnprivate:
		off := false
		for _, id := range q.FileIDs {
			if _, err := store.UpdateFile(ctx, s.userID, id, meta.FileUpdate{Private: &off}); err != nil && !errors.Is(err, meta.ErrNotFound) {
				return "", err
			}
		}
		return fmt.Sprintf("%d folder(s) are no longer private", len(q.FileIDs)), nil
	case reqReconcile:
		rep, err := s.svc.Reconcile(ctx, true)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(rep)
		if len(b) > 2000 {
			b = b[:2000]
		}
		return string(b), nil
	}
	return "", errBadOp
}
