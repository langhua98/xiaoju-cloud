package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// fakeTelegram 假的 Bot API：记下收到的请求，getUpdates 返回 updates。
type fakeTelegram struct {
	mu      sync.Mutex
	updates string
	calls   []string // 方法名
	last    map[string]string
	photo   []byte
	fail    string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
		w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/botTOKEN/")
	f.calls = append(f.calls, method)
	f.last = map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(4 << 20)
		for k, v := range r.MultipartForm.Value {
			f.last[k] = v[0]
		}
		if fh := r.MultipartForm.File["photo"]; len(fh) > 0 {
			p, _ := fh[0].Open()
			f.photo, _ = io.ReadAll(p)
		}
	} else {
		r.ParseForm()
		for k, v := range r.PostForm {
			f.last[k] = v[0]
		}
	}
	if f.fail != "" && (method == "sendMessage" || method == "sendPhoto") {
		w.Write([]byte(`{"ok":false,"description":"` + f.fail + `"}`))
		return
	}
	switch method {
	case "getMe":
		w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"xiaoju_cloud_bot"}}`))
	case "getUpdates":
		w.Write([]byte(`{"ok":true,"result":` + f.updates + `}`))
	default:
		w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}
}

func notifyTestServer(t *testing.T, tg *fakeTelegram, chat int64) (*httptest.Server, *Server) {
	t.Helper()
	tgs := httptest.NewServer(tg)
	t.Cleanup(tgs.Close)
	s := &Server{notifier: newNotifier("TOKEN", chat)}
	if s.notifier != nil {
		s.notifier.api = tgs.URL
	}
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeRead, ScopeUpload}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/v1/notify", func(w http.ResponseWriter, r *http.Request) {
		s.notify(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, s
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

func TestNotifyFindsChatFromStartAndSendsPhoto(t *testing.T) {
	tg := &fakeTelegram{updates: `[]`}
	ts, _ := notifyTestServer(t, tg, 0)

	// 还没人给机器人发过 /start：409，提示里带机器人的名字
	code, m := postNotify(t, ts, map[string]any{"text": "有动静"})
	if code != http.StatusConflict || m["code"] != "need_start" || !strings.Contains(m["hint"].(string), "@xiaoju_cloud_bot") {
		t.Fatalf("before /start: %d %v", code, m)
	}

	// 发过 /start 了（最近的私聊是 555，群聊不算）
	tg.mu.Lock()
	tg.updates = `[{"message":{"chat":{"id":111,"type":"private"}}},{"message":{"chat":{"id":555,"type":"private"}}},{"message":{"chat":{"id":-9,"type":"group"}}}]`
	tg.mu.Unlock()
	jpg := []byte{0xff, 0xd8, 1, 2, 3}
	code, m = postNotify(t, ts, map[string]any{"text": "客厅有动静", "photo": base64.StdEncoding.EncodeToString(jpg)})
	if code != 200 || m["chat"] != float64(555) {
		t.Fatalf("send: %d %v", code, m)
	}
	tg.mu.Lock()
	if tg.calls[len(tg.calls)-1] != "sendPhoto" || tg.last["chat_id"] != "555" || tg.last["caption"] != "客厅有动静" || !bytes.Equal(tg.photo, jpg) {
		t.Fatalf("telegram got %v %v %v", tg.calls, tg.last, tg.photo)
	}
	n := len(tg.calls)
	tg.mu.Unlock()

	// 找到过一次就记住，不用每次 getUpdates
	postNotify(t, ts, map[string]any{"text": "又有动静"})
	tg.mu.Lock()
	if got := tg.calls[n:]; len(got) != 1 || got[0] != "sendMessage" || tg.last["text"] != "又有动静" {
		t.Fatalf("second send: %v %v", got, tg.last)
	}
	tg.mu.Unlock()
}

func TestNotifyChatFromRequestOrEnv(t *testing.T) {
	tg := &fakeTelegram{updates: `[]`}
	ts, _ := notifyTestServer(t, tg, 42) // TG_NOTIFY_CHAT_ID=42
	if code, m := postNotify(t, ts, map[string]any{"text": "a"}); code != 200 || m["chat"] != float64(42) {
		t.Fatalf("env chat: %d %v", code, m)
	}
	// 被控端记下的 chat 优先
	if code, m := postNotify(t, ts, map[string]any{"text": "b", "chat": 77}); code != 200 || m["chat"] != float64(77) {
		t.Fatalf("request chat: %d %v", code, m)
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for _, c := range tg.calls {
		if c == "getUpdates" {
			t.Fatal("getUpdates should not be needed when the chat is known")
		}
	}
}

func TestNotifyErrorsAndLimits(t *testing.T) {
	tg := &fakeTelegram{updates: `[]`}
	ts, s := notifyTestServer(t, tg, 42)
	if code, _ := postNotify(t, ts, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty: %d", code)
	}
	if code, _ := postNotify(t, ts, map[string]any{"photo": "not base64!!"}); code != http.StatusBadRequest {
		t.Fatalf("bad photo: %d", code)
	}
	tg.mu.Lock()
	tg.fail = "Forbidden: bot can't initiate conversation with a user"
	tg.mu.Unlock()
	code, m := postNotify(t, ts, map[string]any{"text": "x"})
	if code != http.StatusBadGateway || !strings.Contains(m["error"].(string), "Forbidden") {
		t.Fatalf("telegram error: %d %v", code, m)
	}
	if strings.Contains(m["error"].(string), "TOKEN") {
		t.Fatal("bot token leaked in error")
	}
	tg.mu.Lock()
	tg.fail = ""
	tg.mu.Unlock()
	// 限流：已经发了 1 条（失败的也算），再发 notifyBurst-1 条就满了
	for i := 0; i < notifyBurst-1; i++ {
		if code, _ := postNotify(t, ts, map[string]any{"text": "x"}); code != 200 {
			t.Fatalf("send %d: %d", i, code)
		}
	}
	if code, m := postNotify(t, ts, map[string]any{"text": "x"}); code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after %d sends, got %d %v", notifyBurst, code, m)
	}
	_ = s

	// 没配机器人：501
	s2 := &Server{notifier: newNotifier("", 0)}
	rec := httptest.NewRecorder()
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeUpload}}}
	req := httptest.NewRequest("POST", "/api/agent/v1/notify", strings.NewReader(`{"text":"x"}`))
	s2.notify(rec, req.WithContext(context.WithValue(req.Context(), agentCtxKey{}, a)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured: %d", rec.Code)
	}
}
