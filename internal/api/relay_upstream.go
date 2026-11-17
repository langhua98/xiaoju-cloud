package api

import (
	"context"
	"crypto/hmac"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/relay"
)

// 视频中转（见 internal/relay）的家里电脑这一头：
//   - 拿视频链接时另给一个 stream：走中转的地址（中转在机房，直接从 Telegram 取视频，不经过家里电脑的上传带宽）；
//   - GET /relay/files/{id}?exp&sig：中转来要索引（分块位置、包裹过的文件密钥），要带 X-Relay-Key。
//
// 每 5 分钟看一次中转在不在（顺便让 Hugging Face 的 Space 别睡着）；不在时不给 stream，客户端照旧直接播。

type relayState struct {
	url string
	key string
	ok  atomic.Bool
}

func newRelayState(url, token string) *relayState {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return nil
	}
	return &relayState{url: url, key: relay.Key(token)}
}

// RunRelayCheck 定期检查中转是否在线。没配置中转时立即返回。
func (s *Server) RunRelayCheck(ctx context.Context, every time.Duration) {
	r := s.videoRelay
	if r == nil {
		return
	}
	client := &http.Client{Timeout: 90 * time.Second} // Space 睡着了要唤醒，第一次会久一点
	check := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url+"/healthz", nil)
		if err != nil {
			return
		}
		ok := false
		if resp, err := client.Do(req); err == nil {
			ok = resp.StatusCode == http.StatusOK
			resp.Body.Close()
		}
		if ok != r.ok.Swap(ok) && s.log != nil {
			s.log.Info("video relay", "url", r.url, "online", ok)
		}
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// streamLink：视频走中转的播放地址；没配置中转、中转不在线或者不是视频时返回空。
func (s *Server) streamLink(f *meta.File, query string) string {
	r := s.videoRelay
	if r == nil || !r.ok.Load() || !strings.HasPrefix(f.Mime, "video/") {
		return ""
	}
	return r.url + "/rl/" + f.ID.String() + "?" + query
}

// GET /relay/files/{id}?exp=..&sig=..：给中转的索引。链接签名和下载链接一样（中转原样转过来），另外要 X-Relay-Key。
func (s *Server) relayIndex(w http.ResponseWriter, r *http.Request) {
	rs := s.videoRelay
	if rs == nil || !hmac.Equal([]byte(r.Header.Get(relay.KeyHeader)), []byte(rs.key)) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp || !hmac.Equal([]byte(q.Get("sig")), []byte(s.linkSig(id, exp))) {
		writeError(w, http.StatusForbidden, "invalid or expired link")
		return
	}
	ctx := r.Context()
	f, err := s.svc.Store().Get(ctx, s.userID, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	parts, err := s.svc.Store().ListParts(ctx, f.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, relay.Meta{
		Name: f.Name, Mime: f.Mime, Size: f.Size, UpdatedAt: f.UpdatedAt, DEKWrapped: f.DEKWrapped, Parts: parts,
	})
}
