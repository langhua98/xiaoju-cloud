package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 缩略图（相册网格）：列目录时给每张照片、视频附上签名的缩略图地址 /thumb/{id}?exp=..&sig=..，
// <img> 直接加载、浏览器可以缓存。缩略图只有 320×320，按约定不计入格子口令的“看了几张”，
// 点开看原图（/link、/content）才算。签名和下载链接分开，缩略图的签名不能用来下载原文件。

// thumbLinkBucket：缩略图链接的过期时间按 6 小时取整，同一时段里列目录得到的地址不变，浏览器缓存才用得上。
const thumbLinkBucket = 6 * time.Hour

// listItem 是列目录返回的条目：文件信息加上缩略图地址和视频时长（秒）；文件夹带上里面（连同子文件夹）
// 有几张照片、几个视频、几个其他文件。
type listItem struct {
	*meta.File
	Thumb    string             `json:"thumb,omitempty"`
	Duration float64            `json:"duration,omitempty"`
	Counts   *meta.FolderCounts `json:"counts,omitempty"`
}

func (s *Server) withThumbs(ctx context.Context, files []*meta.File) ([]listItem, error) {
	items := make([]listItem, len(files))
	var ids []uuid.UUID
	for i, f := range files {
		items[i].File = f
		if f.Type == meta.TypeFile {
			ids = append(ids, f.ID)
		}
	}
	durations, err := s.svc.ThumbnailDurations(ctx, ids)
	if err != nil {
		return nil, err
	}
	b := int64(thumbLinkBucket / time.Second)
	exp := (time.Now().Unix()/b + 2) * b // 6–12 小时后过期
	for i := range items {
		f := items[i].File
		d, ready := durations[f.ID]
		if f.Type != meta.TypeFile || !(ready || s.svc.CanThumbnail(f)) {
			continue
		}
		items[i].Thumb = fmt.Sprintf("/thumb/%s?exp=%d&sig=%s", f.ID, exp, s.thumbSig(f.ID, exp))
		items[i].Duration = d.Seconds()
	}
	return items, nil
}

// thumbSig 用管理员令牌签名（和下载链接用不同的前缀，两种签名不能互换）。
func (s *Server) thumbSig(id uuid.UUID, exp int64) string {
	m := hmac.New(sha256.New, []byte("xiaoju thumbnail link\x00"+s.token))
	fmt.Fprintf(m, "%s\n%d", id, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// GET /thumb/{id}?exp=..&sig=..：320×320 的 JPEG。还没生成时当场生成（可能要几秒）；做不了的返回 404。
func (s *Server) thumbnail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp ||
		!hmac.Equal([]byte(q.Get("sig")), []byte(s.thumbSig(id, exp))) {
		writeError(w, http.StatusForbidden, "invalid or expired link")
		return
	}
	jpeg, err := s.svc.Thumbnail(r.Context(), s.userID, id)
	switch {
	case errors.Is(err, service.ErrNoThumbnail):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no thumbnail for this file", "code": "no_thumbnail"})
		return
	case errors.Is(err, context.Canceled):
		return // 浏览器已经不要了（划走了）
	case err != nil:
		s.fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Length", strconv.Itoa(len(jpeg)))
	h.Set("Cache-Control", fmt.Sprintf("private, max-age=%d, immutable", max(0, exp-time.Now().Unix())))
	w.Write(jpeg)
}
