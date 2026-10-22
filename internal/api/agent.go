package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// AI agent 管理网盘（设计见 README「AI 助手」一节）。
//
// 网页上（登录、刷脸）给每个 agent 发一把令牌 xa_…，令牌上写明权限（scopes）、能碰哪个文件夹、能不能看私密文件夹、
// 什么时候过期、每天的额度。agent 带着它调 /api/agent/v1/…（agent_files.go）或 /mcp（mcp.go）。
// 每个请求依次过五道关（requireAgent → 各接口里的 need / agentItem / quota / 危险操作只能申请），都记进审计日志。
// 上传链接（drops.go）是同一张表里的另一种令牌（kind = 'link'）。

// 权限范围。
const (
	ScopeRead     = "files:read"     // 列目录、搜索、文件信息、缩略图、统计、重复文件、回收站
	ScopeDownload = "files:download" // 下载、读内容、预览、短时下载链接
	ScopeUpload   = "files:upload"   // 建文件夹、上传（同名自动改名，不覆盖）
	ScopeOrganize = "files:organize" // 改名、移动、移入回收站、恢复、批量
	ScopeShares   = "shares:manage"  // 建、撤销分享链接
	ScopeOps      = "ops:read"       // 备份状态、正在进行的上传
	ScopeDanger   = "danger:request" // 申请永久删除、清空回收站、取消私密、对账修复（人批准后才执行）
)

var allScopes = []string{ScopeRead, ScopeDownload, ScopeUpload, ScopeOrganize, ScopeShares, ScopeOps, ScopeDanger}

// agentPresets 是网页上的预设档位。
var agentPresets = map[string][]string{
	"readonly": {ScopeRead, ScopeOps},
	"upload":   {ScopeRead, ScopeDownload, ScopeUpload},
	"organize": {ScopeRead, ScopeDownload, ScopeUpload, ScopeOrganize},
	"admin":    allScopes,
}

// 每天的额度（每把令牌单独算，UTC 零点重置）。网页上创建时可以改。0 表示不限。
const (
	quotaRequests      = "requests"       // 请求数
	quotaTrash         = "trash"          // 移入回收站的文件数
	quotaDownloadBytes = "download_bytes" // 下载的字节数
	quotaMedia         = "media"          // 照片、视频原图（只在开了格子口令时算）
	quotaUploadBytes   = "upload_bytes"   // 上传的字节数
	quotaShares        = "shares"         // 新建分享链接数
)

var defaultAgentLimits = map[string]int64{
	quotaRequests:      20000,
	quotaTrash:         500,
	quotaDownloadBytes: 5 << 30,
	quotaMedia:         50,
	quotaUploadBytes:   20 << 30,
	quotaShares:        20,
}

const (
	agentTokenPrefix  = "xa_"
	agentPlanFiles    = 50               // 一次改动超过这么多文件要先预演
	agentFreezeAfter  = 50               // 连续被拒这么多次自动冻结
	agentLinkTTL      = 15 * time.Minute // agent 拿到的下载链接的有效期
	agentMaxShareDays = 7
	agentAuditKeep    = 180 * 24 * time.Hour
)

var agentExpiryDays = []int{1, 7, 30, 90}

// 每把令牌每秒最多 10 个请求，最多连着 30 个（测试里调大）。
var (
	agentRate  = rate.Limit(10)
	agentBurst = 30
)

// agentCall 是一个通过了令牌校验的请求。
type agentCall struct {
	tok      *meta.AgentToken
	root     *meta.File // 令牌限定的文件夹；nil 表示整个网盘
	rootPath string     // 根文件夹在网盘里的路径（给人看的）
	ip, ua   string
	batch    *uuid.UUID
}

type agentCtxKey struct{}

func agentFrom(r *http.Request) *agentCall {
	a, _ := r.Context().Value(agentCtxKey{}).(*agentCall)
	return a
}

func (a *agentCall) has(scope string) bool { return slices.Contains(a.tok.Scopes, scope) }

func (a *agentCall) rootID() *uuid.UUID {
	if a.root == nil {
		return nil
	}
	return &a.root.ID
}

func (a *agentCall) limit(counter string) int64 {
	if n, ok := a.tok.Limits[counter]; ok {
		return n
	}
	return defaultAgentLimits[counter]
}

// agentState 是 agent 相关的内存状态：每把令牌的限速、连续被拒次数、最近记过的 IP、批量预演、幂等上传。
type agentState struct {
	mu      sync.Mutex
	limits  map[uuid.UUID]*rate.Limiter
	denied  map[uuid.UUID]int
	touched map[uuid.UUID]touchMark
	plans   map[string]*batchPlan
	idem    map[string]idemEntry
}

type touchMark struct {
	ip string
	at time.Time
}

type idemEntry struct {
	file *meta.File
	at   time.Time
}

func newAgentState() *agentState {
	return &agentState{limits: map[uuid.UUID]*rate.Limiter{}, denied: map[uuid.UUID]int{}, touched: map[uuid.UUID]touchMark{},
		plans: map[string]*batchPlan{}, idem: map[string]idemEntry{}}
}

func (st *agentState) allow(id uuid.UUID) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	l := st.limits[id]
	if l == nil {
		l = rate.NewLimiter(agentRate, agentBurst)
		st.limits[id] = l
	}
	return l.Allow()
}

// agentFingerprint：令牌签发时的管理员令牌指纹。换了管理员令牌，所有 agent 令牌一起作废。
func (s *Server) agentFingerprint() string {
	sum := sha256.Sum256([]byte("xiaoju agent\x00" + s.token))
	return hex.EncodeToString(sum[:8])
}

func hashAgentToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

func newAgentSecret() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return agentTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// agentTokenFrom 取请求里的 agent 令牌：X-Xiaoju-Agent，或 Authorization: Bearer xa_…（不接受放在网址里）。
func agentTokenFrom(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-Xiaoju-Agent")); t != "" {
		return t
	}
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.HasPrefix(t, agentTokenPrefix) {
		return strings.TrimSpace(t)
	}
	return ""
}

// agentError 是 agent 接口统一的错误格式：hint 用一句话告诉 agent 下一步该怎么做。
func agentError(w http.ResponseWriter, status int, code, msg, hint string) {
	out := map[string]string{"error": msg, "code": code}
	if hint != "" {
		out["hint"] = hint
	}
	writeJSON(w, status, out)
}

// requireAgent 校验 agent 令牌（第一道关），再按令牌限速、数请求数，记下最近使用的时间和 IP。
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := agentTokenFrom(r)
		if raw == "" {
			agentError(w, http.StatusUnauthorized, "unauthorized", "missing agent token",
				"Send the token from the web page as 'Authorization: Bearer xa_…' or 'X-Xiaoju-Agent: xa_…'.")
			return
		}
		ctx := r.Context()
		store := s.svc.Store()
		tok, err := store.AgentTokenByHash(ctx, hashAgentToken(raw))
		now := time.Now()
		switch {
		case errors.Is(err, meta.ErrNotFound):
			agentError(w, http.StatusUnauthorized, "unauthorized", "unknown agent token", "Ask the owner for a new token.")
			return
		case err != nil:
			s.fail(w, err)
			return
		case tok.RevokedAt != nil || tok.IssuedUnder != s.agentFingerprint():
			agentError(w, http.StatusUnauthorized, "token_revoked", "this agent token was revoked", "Ask the owner for a new token.")
			return
		case tok.ExpiresAt != nil && now.After(*tok.ExpiresAt):
			agentError(w, http.StatusUnauthorized, "token_expired", "this agent token has expired", "Ask the owner to renew it on the web page.")
			return
		case tok.FrozenAt != nil:
			agentError(w, http.StatusForbidden, "token_frozen", "this agent token was frozen after too many rejected requests",
				"Ask the owner to unfreeze it on the web page.")
			return
		}
		a := &agentCall{tok: tok, ip: hostOnly(clientIP(r)), ua: r.UserAgent()}
		if len(a.ua) > 200 {
			a.ua = a.ua[:200]
		}
		if tok.RootFolderID != nil {
			chain, err := store.Ancestry(ctx, s.userID, *tok.RootFolderID)
			if err != nil {
				s.fail(w, err)
				return
			}
			for _, f := range chain {
				if f.TrashedAt != nil {
					agentError(w, http.StatusForbidden, "root_missing", "the folder this token is limited to is in the trash", "Ask the owner to restore it.")
					return
				}
			}
			a.root = chain[0]
			a.rootPath = chainPath(chain, nil)
		}
		if !s.agents.allow(tok.ID) {
			w.Header().Set("Retry-After", "1")
			agentError(w, http.StatusTooManyRequests, "rate_limited", "too many requests", "Slow down to at most 10 requests per second.")
			return
		}
		if n, err := store.AddAgentUsage(ctx, tok.ID, quotaRequests, 1); err != nil {
			s.fail(w, err)
			return
		} else if l := a.limit(quotaRequests); l > 0 && n > l {
			agentError(w, http.StatusTooManyRequests, "quota_exceeded", "daily request quota used up", quotaHint(quotaRequests))
			return
		}
		s.touchAgent(ctx, a)
		next(w, r.WithContext(storage.WithLane(context.WithValue(ctx, agentCtxKey{}, a), storage.LaneAgent)))
	}
}

// touchAgent 记下最近一次使用（同一个 IP 一分钟最多写一次库）；新 IP 记一条审计。
func (s *Server) touchAgent(ctx context.Context, a *agentCall) {
	st := s.agents
	st.mu.Lock()
	m := st.touched[a.tok.ID]
	skip := m.ip == a.ip && time.Since(m.at) < time.Minute
	if !skip {
		st.touched[a.tok.ID] = touchMark{ip: a.ip, at: time.Now()}
	}
	st.mu.Unlock()
	if skip {
		return
	}
	isNew, err := s.svc.Store().TouchAgentToken(context.WithoutCancel(ctx), a.tok.ID, a.ip)
	if err != nil {
		s.log.Warn("agent: touch token", "err", err)
		return
	}
	if isNew && len(a.tok.IPs) > 0 { // 第一次用不算“新 IP”
		s.audit(ctx, a, "new_ip", nil, nil, map[string]any{"ip": a.ip, "known": a.tok.IPs}, "ok", "", 0)
	}
}

// need 检查令牌有没有这项权限（第二道关）。
func (s *Server) need(w http.ResponseWriter, r *http.Request, scope string) bool {
	a := agentFrom(r)
	if a.has(scope) {
		return true
	}
	s.denied(r.Context(), a, r.Method+" "+r.URL.Path, "missing_scope")
	agentError(w, http.StatusForbidden, "missing_scope", "this token lacks the "+scope+" permission",
		"Call GET /api/agent/v1/me to see what this token may do; ask the owner for a token with "+scope+" if needed.")
	return false
}

// denied 记一次被拒（审计 + 连续被拒计数，到 agentFreezeAfter 次冻结令牌）。
func (s *Server) denied(ctx context.Context, a *agentCall, action, code string, files ...*meta.File) {
	s.audit(ctx, a, action, files, nil, nil, "denied", code, 0)
	st := s.agents
	st.mu.Lock()
	st.denied[a.tok.ID]++
	n := st.denied[a.tok.ID]
	st.mu.Unlock()
	if n == agentFreezeAfter {
		if err := s.svc.Store().FreezeAgentToken(context.WithoutCancel(ctx), a.tok.ID); err != nil {
			s.log.Warn("agent: freeze token", "err", err)
		}
		s.audit(ctx, a, "frozen", nil, nil, map[string]any{"after_denials": n}, "ok", "", 0)
	}
}

func (s *Server) succeeded(a *agentCall) {
	st := s.agents
	st.mu.Lock()
	delete(st.denied, a.tok.ID)
	st.mu.Unlock()
}

// audit 记一行审计日志。paths 为 nil 时用 files 的名字。
func (s *Server) audit(ctx context.Context, a *agentCall, action string, files []*meta.File, paths []string, detail map[string]any, result, code string, bytes int64) int64 {
	e := &meta.AuditEntry{TokenID: &a.tok.ID, TokenName: a.tok.Name, Action: action, Paths: paths, Result: result, Code: code,
		Bytes: bytes, IP: a.ip, UserAgent: a.ua, BatchID: a.batch}
	for _, f := range files {
		if f != nil {
			e.FileIDs = append(e.FileIDs, f.ID)
			if paths == nil {
				e.Paths = append(e.Paths, f.Name)
			}
		}
	}
	if detail != nil {
		b, _ := json.Marshal(detail)
		e.Detail = b
	}
	if result == "ok" {
		s.succeeded(a)
	}
	id, err := s.svc.Store().AddAudit(context.WithoutCancel(ctx), s.userID, e)
	if err != nil {
		s.log.Warn("agent: write audit", "err", err)
	}
	return id
}

// quota 给今天的某项用量加 n（第四道关），超过额度时回复 429 并返回 false（加上去的不退回：额度只是防失控）。
func (s *Server) quota(w http.ResponseWriter, r *http.Request, counter string, n int64) bool {
	a := agentFrom(r)
	l := a.limit(counter)
	if l <= 0 || n <= 0 {
		return true
	}
	total, err := s.svc.Store().AddAgentUsage(r.Context(), a.tok.ID, counter, n)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if total > l {
		s.audit(r.Context(), a, r.Method+" "+r.URL.Path, nil, nil, map[string]any{"counter": counter, "limit": l}, "denied", "quota_exceeded", 0)
		agentError(w, http.StatusTooManyRequests, "quota_exceeded", "daily "+counter+" quota used up", quotaHint(counter))
		return false
	}
	return true
}

func quotaHint(counter string) string {
	return "The " + counter + " quota resets at 00:00 UTC; the owner can raise it on the web page. GET /api/agent/v1/me shows today's usage."
}

// errOutOfScope：条目在令牌的范围外（别的文件夹、私密文件夹）。对 agent 一律说成“不存在”，不泄露名字。
var errOutOfScope = errors.New("out of scope")

// agentItem 找到令牌能看到的一个条目（第三道关），返回它和它从令牌根文件夹算起的路径。
// allowTrashed 为 false 时，自己或上级在回收站里的都当作不存在。
func (s *Server) agentItem(ctx context.Context, a *agentCall, id uuid.UUID, allowTrashed bool) (*meta.File, string, error) {
	chain, err := s.svc.Store().Ancestry(ctx, s.userID, id)
	if err != nil {
		return nil, "", err
	}
	end := len(chain) // chain[:end] 是令牌范围里的那一段（包括根文件夹）
	if a.root != nil {
		end = slices.IndexFunc(chain, func(f *meta.File) bool { return f.ID == a.root.ID })
		if end < 0 {
			return nil, "", errOutOfScope
		}
		end++
	}
	for _, f := range chain[:end] {
		if f.Private && !a.tok.AllowPrivate {
			return nil, "", errOutOfScope
		}
		if f.TrashedAt != nil && !allowTrashed {
			return nil, "", meta.ErrNotFound
		}
	}
	return chain[0], chainPath(chain[:end], a.root), nil
}

// chainPath 把 Ancestry 的结果拼成路径，从 root 算起（root 本身是 "/"）；root 为 nil 时从网盘根目录算起。
func chainPath(chain []*meta.File, root *meta.File) string {
	var names []string
	for i := len(chain) - 1; i >= 0; i-- {
		if root != nil && chain[i].ID == root.ID {
			continue
		}
		names = append(names, chain[i].Name)
	}
	return "/" + strings.Join(names, "/")
}

// splitPath 把 "/相册/2025 三亚" 拆成段；不允许 "." ".."。
func splitPath(p string) ([]string, error) {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." || validName(seg) != nil {
			return nil, errors.New("invalid path")
		}
		out = append(out, seg)
	}
	return out, nil
}

// agentPath 按路径（从令牌根文件夹算起）找条目。返回 nil 表示网盘根目录（只有不限文件夹的令牌能走到）。
func (s *Server) agentPath(ctx context.Context, a *agentCall, p string) (*meta.File, error) {
	segs, err := splitPath(p)
	if err != nil {
		return nil, err
	}
	cur := a.root
	for _, seg := range segs {
		if cur != nil && cur.Type != meta.TypeFolder {
			return nil, meta.ErrNotFound
		}
		f, err := s.svc.Store().ChildByName(ctx, s.userID, idOf(cur), seg)
		if err != nil {
			return nil, err
		}
		if f.Private && !a.tok.AllowPrivate {
			return nil, meta.ErrNotFound
		}
		cur = f
	}
	return cur, nil
}

func idOf(f *meta.File) *uuid.UUID {
	if f == nil {
		return nil
	}
	return &f.ID
}

// joinPath 拼路径："/" + "a" → "/a"。
func joinPath(dir, name string) string {
	if dir == "/" || dir == "" {
		return "/" + name
	}
	return dir + "/" + name
}

// agentFail 把错误转成 agent 能看懂的回复。
func (s *Server) agentFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errOutOfScope):
		if a := agentFrom(r); a != nil {
			s.denied(r.Context(), a, r.Method+" "+r.URL.Path, "out_of_scope")
		}
		agentError(w, http.StatusNotFound, "not_found", "not found", "List a folder or search to find valid ids and paths.")
	case errors.Is(err, meta.ErrNotFound):
		agentError(w, http.StatusNotFound, "not_found", "not found", "List a folder or search to find valid ids and paths.")
	case errors.Is(err, meta.ErrConflict):
		agentError(w, http.StatusConflict, "name_taken", "an item with that name already exists in the target folder", "Pick another name.")
	case errors.Is(err, meta.ErrMoveIntoSelf):
		agentError(w, http.StatusBadRequest, "move_into_self", err.Error(), "")
	case errors.Is(err, service.ErrEmptyUpload):
		agentError(w, http.StatusBadRequest, "empty_file", "empty file", "Send the file bytes as the request body.")
	case err != nil && err.Error() == "invalid path":
		agentError(w, http.StatusBadRequest, "invalid_path", "invalid path", "Use paths like /folder/sub/name.ext without . or .. segments.")
	default:
		s.fail(w, err)
	}
}

// agentView 是 agent 看到的一个条目。
type agentView struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Path      string     `json:"path"`
	ParentID  *uuid.UUID `json:"parent_id"`
	Size      int64      `json:"size,omitempty"`
	Mime      string     `json:"mime,omitempty"`
	SHA256    string     `json:"sha256,omitempty"`
	TakenAt   *time.Time `json:"taken_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Etag      string     `json:"etag"` // 改名、移动时放进 If-Match，文件中间被改过就不会覆盖
	TrashedAt *time.Time `json:"trashed_at,omitempty"`
	Private   bool       `json:"private,omitempty"`
	Thumb     bool       `json:"has_thumbnail,omitempty"`
}

func etagOf(f *meta.File) string { return f.UpdatedAt.UTC().Format(time.RFC3339Nano) }

func (s *Server) viewItem(f *meta.File, path string) agentView {
	v := agentView{ID: f.ID, Name: f.Name, Type: f.Type, Path: path, ParentID: f.ParentID, TakenAt: f.TakenAt,
		CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt, Etag: etagOf(f), TrashedAt: f.TrashedAt, Private: f.Private}
	if f.Type == meta.TypeFile {
		v.Size, v.Mime = f.Size, f.Mime
		if len(f.SHA256) > 0 {
			v.SHA256 = hex.EncodeToString(f.SHA256)
		}
		v.Thumb = s.svc.CanThumbnail(f)
	}
	return v
}

// rootView 是令牌根文件夹（或整个网盘的根目录）的样子。
func (s *Server) rootView(a *agentCall) agentView {
	if a.root != nil {
		return s.viewItem(a.root, "/")
	}
	return agentView{Name: "", Type: meta.TypeFolder, Path: "/"}
}

// isMedia：照片、视频（格子口令只抽查这两种）。
func isMedia(f *meta.File) bool {
	return strings.HasPrefix(f.Mime, "image/") || strings.HasPrefix(f.Mime, "video/")
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

// RunAgentMaintenance 定期删掉 180 天前的审计日志、用量和早就决定了的申请。
func (s *Server) RunAgentMaintenance(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.svc.Store().PurgeOldAgentData(ctx, time.Now().Add(-agentAuditKeep)); err != nil && ctx.Err() == nil {
			s.log.Warn("agent: purge old audit", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hostOnly 去掉端口（直连时 RemoteAddr 带端口，每个连接都不一样，“新 IP”提醒会乱报）。
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// agentLane 把请求标成 AI 的（存储驱动用 AI 专用的 Bot，见 telegram.Pool）。上传链接用。
func agentLane(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(storage.WithLane(r.Context(), storage.LaneAgent)))
	}
}
