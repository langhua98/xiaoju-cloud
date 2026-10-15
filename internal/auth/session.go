// Package auth 实现登录验证：先验访问令牌，再用通行密钥（Face ID / 指纹）解锁，成功后签发会话；
// 看照片、视频时再按格子口令随机抽查。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionTTL 是会话有效期，过期后需要重新输入令牌并刷脸。
const SessionTTL = 12 * time.Hour

// TicketTTL 是“刚刚刷过脸”凭证的有效期（关闭格子口令等敏感操作要用）。
const TicketTTL = 5 * time.Minute

// RevokedStore 持久化被强制退出的会话（*meta.Store 实现），服务重启后仍然有效。
type RevokedStore interface {
	RevokeSession(ctx context.Context, id string, expires time.Time) error
	ListRevokedSessions(ctx context.Context) (map[string]time.Time, error)
}

// Sessions 签发和校验会话凭证："v2.<过期时间戳>.<会话ID>.<HMAC>"。
// 凭证本身无状态：用访问令牌派生签名密钥，更换令牌后所有会话立即失效，服务重启不影响已登录的会话。
// 被强制退出的会话（如格子口令答错）按 ID 记入作废名单，直到它自然过期。
type Sessions struct {
	key   []byte
	now   func() time.Time
	store RevokedStore // 为 nil 时作废名单只在内存里

	mu      sync.Mutex
	revoked map[string]time.Time // 会话 ID → 过期时间
}

func NewSessions(adminToken string) *Sessions {
	return &Sessions{key: []byte("xiaoju session\x00" + adminToken), now: time.Now, revoked: map[string]time.Time{}}
}

// NewSessionsWithStore 同 NewSessions，并从数据库载入作废名单。
func NewSessionsWithStore(ctx context.Context, adminToken string, store RevokedStore) (*Sessions, error) {
	s := NewSessions(adminToken)
	revoked, err := store.ListRevokedSessions(ctx)
	if err != nil {
		return nil, err
	}
	s.store, s.revoked = store, revoked
	return s, nil
}

// Issue 签发一个新会话，返回凭证和过期时间。
func (s *Sessions) Issue() (string, time.Time) {
	exp := s.now().Add(SessionTTL).Truncate(time.Second)
	id, err := randomString(12)
	if err != nil {
		panic(err) // crypto/rand 不可用时无法安全签发
	}
	body := "v2." + strconv.FormatInt(exp.Unix(), 10) + "." + id
	return body + "." + s.sign(body), exp
}

// parse 校验签名和有效期，返回会话 ID 和过期时间（不检查作废名单）。
func (s *Sessions) parse(token string) (string, time.Time, bool) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || !strings.HasPrefix(token, "v2.") {
		return "", time.Time{}, false
	}
	body, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
		return "", time.Time{}, false
	}
	expStr, id, found := strings.Cut(strings.TrimPrefix(body, "v2."), ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !found || id == "" || err != nil || s.now().Unix() >= exp {
		return "", time.Time{}, false
	}
	return id, time.Unix(exp, 0), true
}

// Check 校验会话凭证，有效时返回会话 ID。
func (s *Sessions) Check(token string) (string, bool) {
	id, _, ok := s.parse(token)
	if !ok {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, revoked := s.revoked[id]; revoked {
		return "", false
	}
	return id, true
}

// Valid 报告会话凭证是否有效。
func (s *Sessions) Valid(token string) bool {
	_, ok := s.Check(token)
	return ok
}

// Revoke 强制退出一个会话：之后它不再有效（已持久化时服务重启也一样）。
func (s *Sessions) Revoke(ctx context.Context, token string) error {
	id, exp, ok := s.parse(token)
	if !ok {
		return errors.New("invalid session")
	}
	s.mu.Lock()
	now := s.now()
	for k, e := range s.revoked {
		if now.After(e) {
			delete(s.revoked, k)
		}
	}
	s.revoked[id] = exp
	s.mu.Unlock()
	if s.store != nil {
		return s.store.RevokeSession(ctx, id, exp)
	}
	return nil
}

// IssueTicket 签发“刚刚刷过脸”的凭证 "t1.<过期时间戳>.<HMAC>"。前缀不同，不能当会话使用。
func (s *Sessions) IssueTicket() string {
	body := "t1." + strconv.FormatInt(s.now().Add(TicketTTL).Unix(), 10)
	return body + "." + s.sign(body)
}

// Ticket 校验“刚刚刷过脸”的凭证。
func (s *Sessions) Ticket(token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || !strings.HasPrefix(token, "t1.") {
		return false
	}
	body, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimPrefix(body, "t1."), 10, 64)
	return err == nil && s.now().Unix() < exp
}

func (s *Sessions) sign(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}
