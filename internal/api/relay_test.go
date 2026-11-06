package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRelayHubPutTake(t *testing.T) {
	h := newRelayHub()
	now := time.Now()
	if msgs, wake := h.take("a", now); msgs != nil || wake == nil {
		t.Fatalf("empty box: got %v", msgs)
	}
	h.put("a", json.RawMessage(`{"n":1}`), now)
	h.put("a", json.RawMessage(`{"n":2}`), now)
	h.put("b", json.RawMessage(`{"n":3}`), now)
	msgs, _ := h.take("a", now)
	if len(msgs) != 2 || string(msgs[0].Body) != `{"n":1}` {
		t.Fatalf("got %v", msgs)
	}
	if msgs, _ := h.take("a", now); msgs != nil {
		t.Fatalf("messages must be taken once, got %v", msgs)
	}
	if msgs, _ := h.take("b", now); len(msgs) != 1 {
		t.Fatalf("box b: got %v", msgs)
	}
}

func TestRelayHubWakesWaiter(t *testing.T) {
	h := newRelayHub()
	_, wake := h.take("a", time.Now())
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.put("a", json.RawMessage(`{}`), time.Now())
	}()
	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
	if msgs, _ := h.take("a", time.Now()); len(msgs) != 1 {
		t.Fatalf("got %v", msgs)
	}
}

func TestRelayHubExpiresAndCaps(t *testing.T) {
	h := newRelayHub()
	old := time.Now().Add(-relayMsgTTL - time.Second)
	h.put("a", json.RawMessage(`{"old":true}`), old)
	if msgs, _ := h.take("a", time.Now()); msgs != nil {
		t.Fatalf("expired message returned: %v", msgs)
	}
	now := time.Now()
	for i := 0; i < relayMaxMsgs+5; i++ {
		h.put("a", json.RawMessage(`{}`), now)
	}
	if msgs, _ := h.take("a", now); len(msgs) != relayMaxMsgs {
		t.Fatalf("cap: got %d", len(msgs))
	}
	if h.bytes != 0 {
		t.Fatalf("bytes after taking everything: %d", h.bytes)
	}
}

func TestRelayHubTotalCap(t *testing.T) {
	h := newRelayHub()
	big := json.RawMessage(make([]byte, relayMaxBody))
	now := time.Now()
	n := 0
	for h.put("box"+string(rune('a'+n)), big, now) {
		n++
	}
	if n != relayMaxTotal/relayMaxBody {
		t.Fatalf("stored %d big messages", n)
	}
	h.take("boxa", now)
	if !h.put("boxz", big, now) {
		t.Fatal("put must work again after a box is emptied")
	}
}
