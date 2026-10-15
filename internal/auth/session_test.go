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
