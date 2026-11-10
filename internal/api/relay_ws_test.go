package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func relayDial(t *testing.T, base, box string, query ...string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(base, "http") + "/api/agent/v1/relay/ws?box=" + url.QueryEscape(box) + strings.Join(query, "")
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func relayRead(t *testing.T, c *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %q", b)
	}
	return m
}

func relaySend(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func relayPostMsg(t *testing.T, base, box, body string) map[string]any {
	if t != nil {
		t.Helper()
	}
	resp, err := http.Post(base+"/api/agent/v1/relay?box="+url.QueryEscape(box), "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		if t != nil {
			t.Fatalf("post: %v %v", err, resp)
		}
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

type pushed struct {
	ID   uint64          `json:"id"`
	Body json.RawMessage `json:"body"`
}

func relayMsgs(t *testing.T, m map[string]json.RawMessage) []pushed {
	t.Helper()
	var out []pushed
	if err := json.Unmarshal(m["messages"], &out); err != nil {
		t.Fatalf("no messages in %v", m)
	}
	return out
}

func TestRelayWSPushesAtOnceAndReportsOnline(t *testing.T) {
	ts := relayTestServer(t, ScopeRead, ScopeUpload)
	box := "遥控/测试机/收件"
	if r := relayPostMsg(t, ts.URL, "遥控/别的/收件", `{"x":1}`); r["online"] != false {
		t.Fatalf("nobody listening, online should be false: %v", r)
	}
	c := relayDial(t, ts.URL, box)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if r := relayPostMsg(t, ts.URL, box, `{"do":"info"}`); r["online"] != true {
		t.Fatalf("listener connected, online should be true: %v", r)
	}
	msgs := relayMsgs(t, relayRead(t, c))
	if len(msgs) != 1 || string(msgs[0].Body) != `{"do":"info"}` || msgs[0].ID == 0 {
		t.Fatalf("got %+v", msgs)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("push took %v", d)
	}
	// 心跳
	relaySend(t, c, `{"ping":7}`)
	if m := relayRead(t, c); string(m["pong"]) != "7" {
		t.Fatalf("pong: %v", m)
	}
}

func TestRelayWSRedeliversUnackedAfterReconnect(t *testing.T) {
	ts := relayTestServer(t, ScopeRead, ScopeUpload)
	box := "遥控/测试机/收件"
	relayPostMsg(t, ts.URL, box, `{"n":1}`)
	c := relayDial(t, ts.URL, box)
	first := relayMsgs(t, relayRead(t, c))
	if len(first) != 1 {
		t.Fatalf("got %+v", first)
	}
	c.CloseNow() // 没确认就断了（比如连接被路由器掐了）

	c2 := relayDial(t, ts.URL, box)
	again := relayMsgs(t, relayRead(t, c2))
	if len(again) != 1 || again[0].ID != first[0].ID {
		t.Fatalf("unacked message must come again, got %+v", again)
	}
	relaySend(t, c2, `{"ack":`+jsonNum(again[0].ID)+`}`)
	time.Sleep(50 * time.Millisecond)
	// 确认过的不会再来：长轮询也拿不到
	resp, err := http.Get(ts.URL + "/api/agent/v1/relay?box=" + url.QueryEscape(box))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"messages":[]`) {
		t.Fatalf("acked message still there: %s", b)
	}
}

func TestRelayWSNewConnectionReplacesOld(t *testing.T) {
	ts := relayTestServer(t, ScopeRead, ScopeUpload)
	box := "遥控/测试机/收件"
	old := relayDial(t, ts.URL, box)
	time.Sleep(50 * time.Millisecond)
	relayDial(t, ts.URL, box)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := old.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("old connection should be closed, got err=%v ctx=%v", err, ctx.Err())
	}
	time.Sleep(50 * time.Millisecond)
	if r := relayPostMsg(t, ts.URL, box, `{}`); r["online"] != true {
		t.Fatalf("new listener still online: %v", r)
	}
}

func TestRelayWSOfflineAfterDisconnect(t *testing.T) {
	ts := relayTestServer(t, ScopeRead, ScopeUpload)
	box := "遥控/测试机/收件"
	c := relayDial(t, ts.URL, box)
	time.Sleep(50 * time.Millisecond)
	c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !relayOnline(ts.URL, box) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("still online after the listener left")
}

func relayOnline(base, box string) bool {
	return relayPostMsg(nil, base, box, `{}`)["online"] == true
}

func jsonNum(n uint64) string { b, _ := json.Marshal(n); return string(b) }

// 挂着常驻推送、带 &updates=android 的被控端，「软件包」来了新 APK 也要马上收到提醒（就算很久没别的动静）。
func TestRelayWSGetsUpdateHint(t *testing.T) {
	ts, hub := relayTestServerHub(t, ScopeRead, ScopeUpload)
	box := "遥控/测试机/收件"
	c := relayDial(t, ts.URL, box, "&updates=android")
	time.Sleep(50 * time.Millisecond)
	// 连上很久了：touched 已经过期，但连接还挂着
	hub.mu.Lock()
	hub.boxes[box].touched = time.Now().Add(-2 * relayIdleExpire)
	hub.mu.Unlock()
	if n := hub.hint("android", json.RawMessage(`{"do":"update_hint","name":"a.apk"}`), time.Now()); n != 1 {
		t.Fatalf("hinted %d boxes, want 1", n)
	}
	msgs := relayMsgs(t, relayRead(t, c))
	if len(msgs) != 1 || !strings.Contains(string(msgs[0].Body), "update_hint") {
		t.Fatalf("got %+v", msgs)
	}
}
