package auth

import (
	"strings"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions("token-a")
	s.now = func() time.Time { return now }

	tok, exp := s.Issue()
	if !s.Valid(tok) {
		t.Fatal("fresh session should be valid")
	}
	if exp.Sub(now) != SessionTTL {
		t.Fatalf("ttl = %v", exp.Sub(now))
	}

	// 篡改过期时间、签名，或换了令牌：都无效
	i := strings.LastIndexByte(tok, '.')
	for _, bad := range []string{
		"", "v1", "v1.123", tok + "0", tok[:i] + "9" + tok[i:], "v2" + tok[2:],
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

func TestTickets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions("token-a")
	s.now = func() time.Time { return now }

	p, r := s.IssueTicket(false), s.IssueTicket(true)
	if ok, rec := s.Ticket(p); !ok || rec {
		t.Fatalf("passkey ticket: %v %v", ok, rec)
	}
	if ok, rec := s.Ticket(r); !ok || !rec {
		t.Fatalf("recovery ticket: %v %v", ok, rec)
	}
	// 票据不能当会话，会话也不能当票据
	sess, _ := s.Issue()
	if s.Valid(p) {
		t.Fatal("ticket must not be accepted as a session")
	}
	if ok, _ := s.Ticket(sess); ok {
		t.Fatal("session must not be accepted as a ticket")
	}
	// 把恢复码标记改掉（想再用一个恢复码）会破坏签名
	if ok, _ := s.Ticket(strings.Replace(r, ".r.", ".p.", 1)); ok {
		t.Fatal("tampered flag must be rejected")
	}
	if ok, _ := NewSessions("token-b").Ticket(p); ok {
		t.Fatal("ticket must not survive an admin token change")
	}
	now = now.Add(TicketTTL)
	if ok, _ := s.Ticket(p); ok {
		t.Fatal("expired ticket should be invalid")
	}
}
