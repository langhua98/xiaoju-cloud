package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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

// 分享链接：网页上给一个文件或文件夹生成链接 https://<网页>/share.html#<token>，知道链接的人不用登录就能看
// （文件夹连同子文件夹；能看大图、播放视频、下载），可以设有效期和密码。链接里的随机串是 128 位的，猜不到；
// 撤销后立刻失效。分享的东西在私密文件夹里也照样能看（是自己选的要分享）。
//
//	POST   /api/shares {"file_id", "expires_days": 7（0 不过期）, "password": ""} → 201 分享
//	GET    /api/shares → {"items": [分享…]}
//	DELETE /api/shares/{id}
//
// 不需要登录（按 IP 限速）：
//
//	GET  /api/s/{token}[?folder=子文件夹ID]（设了密码的要带 X-Share-Key）
//	     → {"share": {"name","type","expires_at"}, "folder", "crumbs", "items": [条目，文件带 "url"（下载）和 "preview"]}
//	     没有 / 撤销了 404，过期 410 share_expired，要密码 401 share_password
//	POST /api/s/{token}/unlock {"password"} → {"key"}（12 小时内有效，放进 X-Share-Key）；密码不对 403

const (
	shareKeyTTL        = 12 * time.Hour
	maxShareExpiryDays = 3650
)

type shareView struct {
	*meta.Share
	HasPassword bool `json:"has_password"`
	Expired     bool `json:"expired"`
	Trashed     bool `json:"trashed,omitempty"`
}

func viewShare(sh *meta.Share) shareView {
	return shareView{Share: sh, HasPassword: sh.HasPassword(), Expired: sh.Expired(time.Now()), Trashed: sh.File.TrashedAt != nil}
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileID      uuid.UUID `json:"file_id"`
		ExpiresDays int       `json:"expires_days"`
		Password    string    `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.ExpiresDays < 0 || req.ExpiresDays > maxShareExpiryDays {
		writeError(w, http.StatusBadRequest, "expires_days out of range")
		return
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	sh := &meta.Share{UserID: s.userID, FileID: req.FileID, Token: base64.RawURLEncoding.EncodeToString(raw)}
	if req.ExpiresDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		sh.ExpiresAt = &t
	}
	if pw := strings.TrimSpace(req.Password); pw != "" {
		sh.PasswordSalt = make([]byte, 16)
		rand.Read(sh.PasswordSalt)
		sh.PasswordHash = sharePasswordHash(pw, sh.PasswordSalt)
	}
	out, err := s.svc.Store().CreateShare(r.Context(), sh)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewShare(out))
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	shares, err := s.svc.Store().ListShares(r.Context(), s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	items := make([]shareView, len(shares))
	for i, sh := range shares {
		items[i] = viewShare(sh)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store().DeleteShare(r.Context(), s.userID, id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sharePasswordHash(password string, salt []byte) []byte {
	h, _ := scrypt.Key([]byte(password), salt, 1<<15, 8, 1, 32)
	return h
}

// shareKey 证明“输对过这个分享的密码”：exp.签名，用管理员令牌签名（令牌换了全部作废）。
func (s *Server) shareKey(token string, exp int64) string {
	m := hmac.New(sha256.New, []byte("xiaoju share key\x00"+s.token))
	fmt.Fprintf(m, "%s\n%d", token, exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) validShareKey(token, key string) bool {
	e, _, ok := strings.Cut(key, ".")
	exp, err := strconv.ParseInt(e, 10, 64)
	return ok && err == nil && time.Now().Unix() <= exp && hmac.Equal([]byte(key), []byte(s.shareKey(token, exp)))
}

// shareFor 找到链接对应的分享，过期、文件进了回收站、要密码时直接回复错误并返回 nil。
func (s *Server) shareFor(w http.ResponseWriter, r *http.Request, needKey bool) *meta.Share {
	sh, err := s.svc.Store().ShareByToken(r.Context(), r.PathValue("token"))
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this link does not exist or was revoked", "code": "share_missing"})
		return nil
	case err != nil:
		s.fail(w, err)
		return nil
	case sh.Expired(time.Now()):
		writeJSON(w, http.StatusGone, map[string]string{"error": "this link has expired", "code": "share_expired"})
		return nil
	case sh.File.TrashedAt != nil || sh.File.Status != meta.StatusActive:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the shared item was deleted", "code": "share_missing"})
		return nil
	case needKey && sh.HasPassword() && !s.validShareKey(sh.Token, r.Header.Get("X-Share-Key")):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this link needs a password", "code": "share_password"})
		return nil
	}
	return sh
}

func (s *Server) unlockShare(w http.ResponseWriter, r *http.Request) {
	sh := s.shareFor(w, r, false)
	if sh == nil {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	if sh.HasPassword() && subtle.ConstantTimeCompare(sharePasswordHash(strings.TrimSpace(req.Password), sh.PasswordSalt), sh.PasswordHash) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong password", "code": "share_wrong_password"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": s.shareKey(sh.Token, time.Now().Add(shareKeyTTL).Unix())})
}

// shareItem 是分享页里的一个条目：文件带签名的下载地址和大图预览地址（6 小时有效）。
type shareItem struct {
	listItem
	URL     string `json:"url,omitempty"`
	Preview string `json:"preview,omitempty"`
}

func (s *Server) openShare(w http.ResponseWriter, r *http.Request) {
	sh := s.shareFor(w, r, true)
	if sh == nil {
		return
	}
	ctx := r.Context()
	store := s.svc.Store()
	resp := map[string]any{"share": map[string]any{"name": sh.File.Name, "type": sh.File.Type, "expires_at": sh.ExpiresAt}}
	files := []*meta.File{sh.File}
	if sh.File.Type == meta.TypeFolder {
		folder := sh.File
		if q := r.URL.Query().Get("folder"); q != "" && q != sh.FileID.String() {
			id, err := uuid.Parse(q)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid folder")
				return
			}
			inside, err := store.IsInside(ctx, id, sh.FileID)
			if err != nil {
				s.fail(w, err)
				return
			}
			if f, err := store.Get(ctx, s.userID, id); inside && err == nil && f.Type == meta.TypeFolder && f.TrashedAt == nil {
				folder = f
			} else {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
		}
		crumbs, err := s.shareCrumbs(ctx, sh, folder)
		if err != nil {
			s.fail(w, err)
			return
		}
		if files, err = store.ListChildren(ctx, s.userID, &folder.ID); err != nil {
			s.fail(w, err)
			return
		}
		resp["folder"] = map[string]any{"id": folder.ID, "name": folder.Name}
		resp["crumbs"] = crumbs
	}
	listed, err := s.withThumbs(ctx, files)
	if err == nil {
		err = s.withCounts(ctx, listed)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	items := make([]shareItem, len(listed))
	exp := time.Now().Add(downloadLinkTTL).Unix()
	for i, it := range listed {
		items[i].listItem = it
		if it.Type != meta.TypeFile {
			continue
		}
		sig := s.linkSig(it.ID, exp)
		items[i].URL = fmt.Sprintf("/dl/%s?exp=%d&sig=%s", it.ID, exp, sig)
		if s.svc.CanPreview(it.File) {
			items[i].Preview = fmt.Sprintf("/pv/%s?exp=%d&sig=%s", it.ID, exp, sig)
		}
	}
	resp["items"] = items
	if r.URL.Query().Get("folder") == "" {
		if err := store.CountShareView(ctx, sh.ID); err != nil {
			s.log.Warn("share: count view", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// shareCrumbs 是从分享的文件夹到 folder 的路径（分享页顶部的“文件夹 / 子文件夹”）。
func (s *Server) shareCrumbs(ctx context.Context, sh *meta.Share, folder *meta.File) ([]map[string]any, error) {
	var chain []map[string]any
	for f := folder; ; {
		chain = append([]map[string]any{{"id": f.ID, "name": f.Name}}, chain...)
		if f.ID == sh.FileID || f.ParentID == nil || len(chain) > 64 {
			return chain, nil
		}
		parent, err := s.svc.Store().Get(ctx, s.userID, *f.ParentID)
		if err != nil {
			return nil, err
		}
		f = parent
	}
}
