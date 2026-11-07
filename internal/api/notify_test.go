package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

var errFakeNeedStart = errors.New("need start")

// fakeSender 假的 Telegram：记下发了什么；started 里的人才发得出去。
type fakeSender struct {
	mu      sync.Mutex
	owner   int64
	started map[int64]bool
	sent    []sentMsg
	fail    error
}

type sentMsg struct {
	to    int64
	text  string
	photo []byte
}

func (f *fakeSender) BotName(context.Context) string { return "xiaoju_cloud_bot" }
func (f *fakeSender) Owner(context.Context) (int64, error) {
	if f.owner == 0 {
		return 0, errors.New("no creator")
	}
	return f.owner, nil
}
func (f *fakeSender) Send(_ context.Context, to int64, text string, photo []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if !f.started[to] {
		return errFakeNeedStart
	}
	f.sent = append(f.sent, sentMsg{to, text, photo})
	return nil
}
func (f *fakeSender) NeedStart(err error) bool { return errors.Is(err, errFakeNeedStart) }

func notifyTestServer(t *testing.T, tg TelegramSender, chat int64) *httptest.Server {
	t.Helper()
	s := &Server{notifier: newNotifier(tg, chat)}
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeRead, ScopeUpload}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/v1/notify", func(w http.ResponseWriter, r *http.Request) {
		s.notify(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postNotify(t *testing.T, ts *httptest.Server, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/agent/v1/notify", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestNotifySendsToChannelOwnerAfterStart(t *testing.T) {
	tg := &fakeSender{owner: 555, started: map[int64]bool{}}
	ts := notifyTestServer(t, tg, 0)

	// 主人还没给 Bot 发过 /start：409，提示里带 Bot 的名字
	code, m := postNotify(t, ts, map[string]any{"text": "有动静"})
	if code != http.StatusConflict || m["code"] != "need_start" || !strings.Contains(m["hint"].(string), "@xiaoju_cloud_bot") {
		t.Fatalf("before /start: %d %v", code, m)
	}

	tg.started[555] = true
	jpg := []byte{0xff, 0xd8, 1, 2, 3}
	code, m = postNotify(t, ts, map[string]any{"text": "客厅有动静", "photo": base64.StdEncoding.EncodeToString(jpg)})
	if code != 200 || m["chat"] != float64(555) {
		t.Fatalf("send: %d %v", code, m)
	}
	if len(tg.sent) != 1 || tg.sent[0].to != 555 || tg.sent[0].text != "客厅有动静" || !bytes.Equal(tg.sent[0].photo, jpg) {
		t.Fatalf("sent: %+v", tg.sent)
	}
}

func TestNotifyChatFromRequestOrEnv(t *testing.T) {
	tg := &fakeSender{owner: 555, started: map[int64]bool{42: true, 77: true}}
	ts := notifyTestServer(t, tg, 42) // TG_NOTIFY_CHAT_ID=42
	if code, m := postNotify(t, ts, map[string]any{"text": "a"}); code != 200 || m["chat"] != float64(42) {
		t.Fatalf("env chat: %d %v", code, m)
	}
	// 被控端记下的 chat 优先
	if code, m := postNotify(t, ts, map[string]any{"text": "b", "chat": 77}); code != 200 || m["chat"] != float64(77) {
		t.Fatalf("request chat: %d %v", code, m)
	}
}

func TestNotifyErrorsAndLimits(t *testing.T) {
	tg := &fakeSender{owner: 42, started: map[int64]bool{42: true}}
	ts := notifyTestServer(t, tg, 0)
	if code, _ := postNotify(t, ts, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty: %d", code)
	}
	if code, _ := postNotify(t, ts, map[string]any{"photo": "not base64!!"}); code != http.StatusBadRequest {
		t.Fatalf("bad photo: %d", code)
	}
	tg.fail = errors.New("FLOOD_WAIT")
	if code, m := postNotify(t, ts, map[string]any{"text": "x"}); code != http.StatusBadGateway || !strings.Contains(m["error"].(string), "FLOOD") {
		t.Fatalf("telegram error: %d %v", code, m)
	}
	tg.fail = nil
	// 限流：已经发了 1 条（失败的也算），再发 notifyBurst-1 条就满了
	for i := 0; i < notifyBurst-1; i++ {
		if code, _ := postNotify(t, ts, map[string]any{"text": "x"}); code != 200 {
			t.Fatalf("send %d: %d", i, code)
		}
	}
	if code, _ := postNotify(t, ts, map[string]any{"text": "x"}); code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after %d sends, got %d", notifyBurst, code)
	}

	// 找不到频道主人：502 + 提示设 TG_NOTIFY_CHAT_ID
	ts2 := notifyTestServer(t, &fakeSender{started: map[int64]bool{}}, 0)
	if code, m := postNotify(t, ts2, map[string]any{"text": "x"}); code != http.StatusBadGateway || !strings.Contains(m["hint"].(string), "TG_NOTIFY_CHAT_ID") {
		t.Fatalf("no owner: %d %v", code, m)
	}

	// 没连 Telegram（本地存储）：501
	s2 := &Server{notifier: newNotifier(nil, 0)}
	rec := httptest.NewRecorder()
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeUpload}}}
	req := httptest.NewRequest("POST", "/api/agent/v1/notify", strings.NewReader(`{"text":"x"}`))
	s2.notify(rec, req.WithContext(context.WithValue(req.Context(), agentCtxKey{}, a)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured: %d", rec.Code)
	}
}
