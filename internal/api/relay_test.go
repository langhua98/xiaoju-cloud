package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
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

// relayTestServer 只挂即时信箱两个接口，跳过令牌校验，直接塞一个有读写权限的 agent。
func relayTestServer(t *testing.T, scopes ...string) *httptest.Server {
	ts, _ := relayTestServerHub(t, scopes...)
	return ts
}

func relayTestServerHub(t *testing.T, scopes ...string) (*httptest.Server, *relayHub) {
	s := &Server{relay: newRelayHub()}
	a := &agentCall{tok: &meta.AgentToken{Scopes: scopes}}
	with := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/v1/relay", with(s.relayPost))
	mux.HandleFunc("GET /api/agent/v1/relay", with(s.relayGet))
	mux.HandleFunc("GET /api/agent/v1/relay/ws", with(s.relayWS))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, s.relay
}

func TestRelayHTTPLongPoll(t *testing.T) {
	ts := relayTestServer(t, ScopeRead, ScopeUpload)
	box := url.QueryEscape("遥控/测试机/收件")
	type result struct {
		body string
		took time.Duration
	}
	got := make(chan result, 1)
	go func() {
		start := time.Now()
		resp, err := http.Get(ts.URL + "/api/agent/v1/relay?wait=10&box=" + box)
		if err != nil {
			got <- result{err.Error(), 0}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- result{string(b), time.Since(start)}
	}()
	time.Sleep(100 * time.Millisecond)
	resp, err := http.Post(ts.URL+"/api/agent/v1/relay?box="+box, "application/json", strings.NewReader(`{"do":"info"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", err, resp)
	}
	r := <-got
	if !strings.Contains(r.body, `"do":"info"`) || r.took > 2*time.Second {
		t.Fatalf("long poll: %q after %v", r.body, r.took)
	}

	resp, _ = http.Post(ts.URL+"/api/agent/v1/relay?box="+box, "application/json", strings.NewReader(`not json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: %d", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/api/agent/v1/relay?wait=0&box=" + box)
	b, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(b)) != `{"messages":[]}` {
		t.Fatalf("empty box: %s", b)
	}
}
