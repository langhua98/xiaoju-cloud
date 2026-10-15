package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions("token-a")
	s.now = func() time.Time { return now }

	tok, exp := s.Issue()
	id, ok := s.Check(tok)
	if !ok || id == "" {
		t.Fatal("fresh session should be valid")
	}
	if exp.Sub(now) != SessionTTL {
		t.Fatalf("ttl = %v", exp.Sub(now))
	}
	if tok2, _ := s.Issue(); tok2 == tok {
		t.Fatal("each session needs its own ID")
	}

	// 篡改过期时间、ID、签名，旧格式，或换了令牌：都无效
	i := strings.LastIndexByte(tok, '.')
	for _, bad := range []string{
		"", "v2", "v2.123", tok + "0", tok[:i] + "9" + tok[i:], "v1" + tok[2:],
		strings.Replace(tok, "."+id+".", ".other.", 1),
	} {
		if s.Valid(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
	if NewSessions("token-b").Valid(tok) {
		t.Fatal("session must not survive an admin token change")
	}

	// 过期
	now = now.Add(SessionTTL)
	if s.Valid(tok) {
		t.Fatal("expired session should be invalid")
	}
}

type memRevoked map[string]time.Time

func (m memRevoked) RevokeSession(_ context.Context, id string, exp time.Time) error {
	m[id] = exp
	return nil
}

func (m memRevoked) ListRevokedSessions(context.Context) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

func TestRevokeSession(t *testing.T) {
	ctx := context.Background()
	db := memRevoked{}
	s, _ := NewSessionsWithStore(ctx, "tok", db)
	a, _ := s.Issue()
	b, _ := s.Issue()
	if err := s.Revoke(ctx, a); err != nil {
		t.Fatal(err)
	}
	if s.Valid(a) || !s.Valid(b) {
		t.Fatal("only the revoked session should be logged out")
	}
	// 服务重启后（重新从数据库载入）依然作废
	s2, _ := NewSessionsWithStore(ctx, "tok", db)
	if s2.Valid(a) || !s2.Valid(b) {
		t.Fatal("revocation must survive a restart")
	}
	if s.Revoke(ctx, "garbage") == nil {
		t.Fatal("revoking an invalid token should fail")
	}
}

func TestTickets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions("token-a")
	s.now = func() time.Time { return now }

	ticket := s.IssueTicket()
	if !s.Ticket(ticket) {
		t.Fatal("fresh ticket should be valid")
	}
	// 票据不能当会话，会话也不能当票据
	sess, _ := s.Issue()
	if s.Valid(ticket) || s.Ticket(sess) {
		t.Fatal("tickets and sessions must not be interchangeable")
	}
	if NewSessions("token-b").Ticket(ticket) {
		t.Fatal("ticket must not survive an admin token change")
	}
	now = now.Add(TicketTTL)
	if s.Ticket(ticket) {
		t.Fatal("expired ticket should be invalid")
	}
}
