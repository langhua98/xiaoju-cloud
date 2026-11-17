// Package relay 是「视频中转」：家里电脑往外送数据只有几 Mbps，手机拍的视频要几十 Mbps，原画播放一直转圈。
//
// 视频本来就存在 Telegram 频道里（加密的分块），不在电脑上。所以让机房里的一台服务器（Hugging Face Space）
// 直接从 Telegram 取分块、解密、发给手机；家里电脑只回答“这个文件在频道的哪几条消息里、包裹过的文件密钥是什么”，几 KB。
//
//	手机 ──/rl/{id}?exp&sig──▶ 中转（机房）──GET /relay/files/{id}?exp&sig──▶ 家里电脑（只给索引）
//	                             └───────────── Telegram 频道（视频分块）◀──────┘
//
// 中转不连数据库、不写 Telegram、不跑任何后台任务，只读。链接还是家里电脑签的那个（6 小时有效），
// 中转原样转给电脑核对，所以没有电脑签发的链接，中转什么也拿不到。中转向电脑要索引时另外带 X-Relay-Key
// （用管理员令牌算的，两边都有），别人拿到链接也没法直接向电脑要索引。
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// KeyHeader 是中转向家里电脑要索引时带的认证头。
const KeyHeader = "X-Relay-Key"

// Key 由管理员令牌算出中转的认证值（令牌换了，旧的自动失效）。
func Key(adminToken string) string {
	m := hmac.New(sha256.New, []byte("xiaoju relay key\x00"+adminToken))
	return hex.EncodeToString(m.Sum(nil))
}

// Meta 是家里电脑给中转的索引：文件名、类型、大小，包裹过的文件密钥（要主密钥才解得开），和每个分块在频道里的位置。
type Meta struct {
	Name       string      `json:"name"`
	Mime       string      `json:"mime"`
	Size       int64       `json:"size"`
	UpdatedAt  time.Time   `json:"updated_at"`
	DEKWrapped []byte      `json:"dek_wrapped"`
	Parts      []meta.Part `json:"parts"`
}

// metaTTL：索引在中转上缓存多久（拖动进度条会发很多请求，不用每次都问电脑）。
const metaTTL = 10 * time.Minute

type Options struct {
	Upstream   string // 家里电脑的地址，如 https://node.tail823dbd.ts.net
	AdminToken string
	MasterKey  []byte
	Driver     storage.Driver
	Backup     storage.Driver // 可选：主频道读不到时读备份频道
	Log        *slog.Logger
	Client     *http.Client // 测试用；nil 用默认
}

type Server struct {
	opt    Options
	key    string
	client *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	meta    *Meta
	dek     []byte
	expires time.Time
}

func New(opt Options) *Server {
	c := opt.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &Server{opt: opt, key: Key(opt.AdminToken), client: c, cache: map[string]cached{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("xiaoju-cloud video relay (read-only)\n"))
	})
	mux.HandleFunc("GET /rl/{id}", s.serve)
	return mux
}

// GET /rl/{id}?exp=..&sig=..[&inline=1]：播放 / 下载，支持 Range。
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp || q.Get("sig") == "" {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}
	m, dek, status, err := s.lookup(r.Context(), id, q.Get("exp"), q.Get("sig"))
	if err != nil {
		if s.opt.Log != nil && status >= 500 {
			s.opt.Log.Warn("relay: index lookup failed", "file", id, "err", err)
		}
		http.Error(w, err.Error(), status)
		return
	}
	rd := service.NewFileReaderWithBackup(r.Context(), s.opt.Driver, s.opt.Backup, dek, m.Parts, s.opt.Log)
	defer rd.Close()
	h := w.Header()
	if m.Mime != "" {
		h.Set("Content-Type", m.Mime)
	}
	disposition := "attachment"
	if q.Get("inline") == "1" {
		disposition = "inline"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": m.Name}))
	h.Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, m.Name, m.UpdatedAt, rd)
}

var errNotFound = errors.New("not found")

// lookup 向家里电脑要索引（缓存 metaTTL）。返回出错时该回的 HTTP 状态码。
func (s *Server) lookup(ctx context.Context, id uuid.UUID, exp, sig string) (*Meta, []byte, int, error) {
	ck := id.String() + "|" + exp + "|" + sig
	s.mu.Lock()
	c, ok := s.cache[ck]
	if ok && time.Now().Before(c.expires) {
		s.mu.Unlock()
		return c.meta, c.dek, 0, nil
	}
	s.mu.Unlock()

	u := s.opt.Upstream + "/relay/files/" + id.String() + "?exp=" + url.QueryEscape(exp) + "&sig=" + url.QueryEscape(sig)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	req.Header.Set(KeyHeader, s.key)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, http.StatusBadGateway, fmt.Errorf("家里电脑连不上：%w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, nil, http.StatusForbidden, errors.New("invalid or expired link")
	case http.StatusNotFound:
		return nil, nil, http.StatusNotFound, errNotFound
	default:
		return nil, nil, http.StatusBadGateway, fmt.Errorf("家里电脑回答 %s", resp.Status)
	}
	var m Meta
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, nil, http.StatusBadGateway, fmt.Errorf("索引解析失败：%w", err)
	}
	dek, err := crypto.UnwrapKey(s.opt.MasterKey, m.DEKWrapped)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, fmt.Errorf("解不开文件密钥（两边的主密钥不一样？）：%w", err)
	}
	s.mu.Lock()
	now := time.Now()
	for k, v := range s.cache { // 顺手清掉过期的
		if now.After(v.expires) {
			delete(s.cache, k)
		}
	}
	s.cache[ck] = cached{meta: &m, dek: dek, expires: now.Add(metaTTL)}
	s.mu.Unlock()
	return &m, dek, 0, nil
}
