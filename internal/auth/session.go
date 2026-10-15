// Package auth 实现两步登录：先验访问令牌，再用通行密钥（Face ID / 指纹）解锁，成功后签发会话。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// SessionTTL 是会话有效期，过期后需要重新输入令牌并刷脸。
const SessionTTL = 12 * time.Hour

// TicketTTL 是过渡票据的有效期：刷脸通过后，要在这段时间内答完格子口令。
const TicketTTL = 5 * time.Minute

// Sessions 签发和校验无状态会话凭证："v1.<过期时间戳>.<HMAC>"。
// 用访问令牌派生签名密钥：更换令牌后所有会话立即失效；服务重启不影响已登录的会话。
type Sessions struct {
	key []byte
	now func() time.Time
}

func NewSessions(adminToken string) *Sessions {
	return &Sessions{key: []byte("xiaoju session\x00" + adminToken), now: time.Now}
}

// Issue 签发一个新会话，返回凭证和过期时间。
func (s *Sessions) Issue() (string, time.Time) {
	exp := s.now().Add(SessionTTL).Truncate(time.Second)
	body := "v1." + strconv.FormatInt(exp.Unix(), 10)
	return body + "." + s.sign(body), exp
}

// Valid 校验会话凭证的签名和有效期。
func (s *Sessions) Valid(token string) bool {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || !strings.HasPrefix(token, "v1.") {
		return false
	}
	body, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimPrefix(body, "v1."), 10, 64)
	return err == nil && s.now().Unix() < exp
}

// IssueTicket 签发过渡票据 "t1.<过期时间戳>.<p|r>.<HMAC>"，证明这次登录已经过了刷脸这一步
// （r 表示是用恢复码代替的）。前缀不同，不能当会话使用；无状态，服务重启不影响。
func (s *Sessions) IssueTicket(recovered bool) string {
	flag := "p"
	if recovered {
		flag = "r"
	}
	body := "t1." + strconv.FormatInt(s.now().Add(TicketTTL).Unix(), 10) + "." + flag
	return body + "." + s.sign(body)
}

// Ticket 校验过渡票据，返回是否有效，以及刷脸这一步是否用了恢复码。
func (s *Sessions) Ticket(token string) (ok, recovered bool) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || !strings.HasPrefix(token, "t1.") {
		return false, false
	}
	body, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
		return false, false
	}
	expStr, flag, found := strings.Cut(strings.TrimPrefix(body, "t1."), ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !found || err != nil || (flag != "p" && flag != "r") || s.now().Unix() >= exp {
		return false, false
	}
	return true, flag == "r"
}

func (s *Sessions) sign(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}
