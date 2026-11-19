package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 家里电脑发通行证，中转按通行证放行，画面在中转上从推流端到观看端。
func TestLiveRelayTicketEndToEnd(t *testing.T) {
	const admin = "admin-token-for-test"
	relaySrv := httptest.NewServer(LiveRelayHandler(admin))
	t.Cleanup(relaySrv.Close)

	s := &Server{stream: newStreamHub(), videoRelay: newRelayState(relaySrv.URL, admin)}
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeRead, ScopeUpload}}}
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.streamRelayTicket(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	}))
	t.Cleanup(home.Close)
	room := "遥控/测试机/视频/relay1"

	ticket := func(role string) (int, string) {
		resp, err := http.Get(home.URL + "/?role=" + role + "&room=" + url.QueryEscape(room))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m["url"]
	}

	// 中转不在线：不发通行证
	if code, _ := ticket("pub"); code != http.StatusNotFound {
		t.Fatalf("offline relay: want 404, got %d", code)
	}
	s.videoRelay.ok.Store(true)

	code, pubURL := ticket("pub")
	if code != http.StatusOK || !strings.HasPrefix(pubURL, "ws://") || !strings.Contains(pubURL, "/live?") {
		t.Fatalf("pub ticket: %d %q", code, pubURL)
	}
	_, subURL := ticket("sub")

	dial := func(u string) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, u, nil)
		if err != nil {
			t.Fatalf("dial %s: %v", u, err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	sub := dial(subURL + "&ack=1&window=8")
	if m := readText(t, sub); m["pub"] != false || m["h264"] != true {
		t.Fatalf("viewer hello: %v", m)
	}
	pub := dial(pubURL)
	if m := readText(t, pub); m["subs"] != float64(1) {
		t.Fatalf("publisher sees viewers: %v", m)
	}
	readText(t, sub) // {"pub":true}
	frame := append([]byte{1, 0}, bytes.Repeat([]byte("JPEG"), 100)...)
	send(t, pub, frame)
	if _, got := readMsg(t, sub); !bytes.Equal(got, frame) {
		t.Fatal("frame changed on the way through the relay")
	}

	// 中转和家里电脑的房间不相干
	s.stream.mu.Lock()
	n := len(s.stream.rooms)
	s.stream.mu.Unlock()
	if n != 0 {
		t.Fatalf("home server hub should be empty, has %d rooms", n)
	}

	// 通行证不能挪用：改角色、改房间、过期、乱签都不行
	bad := []string{
		strings.Replace(subURL, "role=sub", "role=pub", 1),
		strings.Replace(subURL, "relay1", "relay2", 1),
		relaySrv.URL + "/live?room=x&role=sub&exp=9999999999&sig=00",
		"ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/live?" + url.Values{
			"room": {room}, "role": {"sub"}, "exp": {"1"}, "sig": {liveSig(s.videoRelay.key, room, "sub", 1)},
		}.Encode(),
	}
	for _, u := range bad {
		u = strings.Replace(u, "http://", "ws://", 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, resp, err := websocket.Dial(ctx, u, nil)
		cancel()
		if err == nil {
			c.CloseNow()
			t.Fatalf("relay accepted a bad ticket: %s", u)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bad ticket %s: want 403, got %v", u, resp)
		}
	}
}

func TestLiveRelayTicketURL(t *testing.T) {
	s := &Server{stream: newStreamHub(), videoRelay: newRelayState("https://relay.example", "x")}
	s.videoRelay.ok.Store(true)
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeRead}}}
	call := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/?"+q, nil)
		s.streamRelayTicket(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
		return w
	}
	w := call("role=sub&room=r")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "wss://relay.example/live?") {
		t.Fatalf("sub ticket: %d %s", w.Code, w.Body.String())
	}
	if w := call("role=x&room=r"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad role: %d", w.Code)
	}
}
