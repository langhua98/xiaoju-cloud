package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

func TestStreamPeerKeepsOnlyNewestFrame(t *testing.T) {
	p := newStreamPeer()
	p.offer([]byte("1"))
	p.offer([]byte("2"))
	p.offer([]byte("3"))
	if got := string(<-p.frame); got != "3" {
		t.Fatalf("want newest frame 3, got %q", got)
	}
	select {
	case f := <-p.frame:
		t.Fatalf("only one frame may wait, got another %q", f)
	default:
	}
}

func ctrlMsg(t *testing.T, p *streamPeer) map[string]any {
	t.Helper()
	select {
	case b := <-p.ctrl:
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no control message")
		return nil
	}
}

func TestStreamHubJoinLeave(t *testing.T) {
	h := newStreamHub()
	pub, sub := newStreamPeer(), newStreamPeer()
	if _, ok := h.join("r", pub, true); !ok {
		t.Fatal("pub join")
	}
	if m := ctrlMsg(t, pub); m["subs"] != float64(0) {
		t.Fatalf("pub first message: %v", m)
	}
	h.join("r", sub, false)
	if m := ctrlMsg(t, sub); m["pub"] != true {
		t.Fatalf("sub first message: %v", m)
	}
	if m := ctrlMsg(t, pub); m["subs"] != float64(1) {
		t.Fatalf("pub after sub joined: %v", m)
	}
	h.forward("r", pub, []byte("frame"))
	if f := <-sub.frame; string(f) != "frame" {
		t.Fatalf("forward: %q", f)
	}
	h.forward("r", sub, []byte("not a publisher")) // 只有推流端能发
	select {
	case f := <-sub.frame:
		t.Fatalf("frame from a viewer was forwarded: %q", f)
	default:
	}
	h.leave("r", sub, false)
	if m := ctrlMsg(t, pub); m["subs"] != float64(0) {
		t.Fatalf("pub after sub left: %v", m)
	}
	h.leave("r", pub, true)
	if len(h.rooms) != 0 {
		t.Fatalf("empty room not removed: %v", h.rooms)
	}
}

func TestStreamHubReplacedPublisher(t *testing.T) {
	h := newStreamHub()
	old, nw, sub := newStreamPeer(), newStreamPeer(), newStreamPeer()
	h.join("r", old, true)
	h.join("r", sub, false)
	if got, _ := h.join("r", nw, true); got != old {
		t.Fatal("new publisher must hand back the old one")
	}
	h.leave("r", old, true) // 被顶掉的离开，不能把新的也弄没
	h.forward("r", nw, []byte("new"))
	if f := <-sub.frame; string(f) != "new" {
		t.Fatalf("got %q", f)
	}
	h.forward("r", old, []byte("old"))
	select {
	case f := <-sub.frame:
		t.Fatalf("replaced publisher still forwards: %q", f)
	default:
	}
}

func TestStreamHubLimits(t *testing.T) {
	h := newStreamHub()
	for i := 0; i < streamMaxSubs; i++ {
		if _, ok := h.join("r", newStreamPeer(), false); !ok {
			t.Fatalf("viewer %d rejected", i)
		}
	}
	if _, ok := h.join("r", newStreamPeer(), false); ok {
		t.Fatal("too many viewers accepted")
	}
	for i := 1; i < streamMaxRooms; i++ {
		h.join("room-"+strconv.Itoa(i), newStreamPeer(), true)
	}
	if _, ok := h.join("one too many", newStreamPeer(), true); ok {
		t.Fatal("too many rooms accepted")
	}
}

// streamTestServer 只挂直播接口，跳过令牌校验，直接塞一个有读写权限的 agent。
func streamTestServer(t *testing.T) *httptest.Server {
	s := &Server{stream: newStreamHub()}
	a := &agentCall{tok: &meta.AgentToken{Scopes: []string{ScopeRead, ScopeUpload}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agent/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		s.streamWS(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func dialStream(t *testing.T, ts *httptest.Server, room, role string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/agent/v1/stream?role=" + role + "&room=" + url.QueryEscape(room)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", role, err)
	}
	c.SetReadLimit(streamMaxFrame + 1024)
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, b
}

func readText(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	typ, b := readMsg(t, c)
	if typ != websocket.MessageText {
		t.Fatalf("want a text message, got %d bytes of binary", len(b))
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func send(t *testing.T, c *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestStreamWebSocketEndToEnd(t *testing.T) {
	ts := streamTestServer(t)
	room := "遥控/测试机/视频/abc123"

	sub := dialStream(t, ts, room, "sub")
	if m := readText(t, sub); m["pub"] != false {
		t.Fatalf("viewer before publisher: %v", m)
	}
	pub := dialStream(t, ts, room, "pub")
	if m := readText(t, pub); m["subs"] != float64(1) {
		t.Fatalf("publisher sees viewers: %v", m)
	}
	if m := readText(t, sub); m["pub"] != true {
		t.Fatalf("viewer told publisher is here: %v", m)
	}

	// 一帧原样到达
	frame := append([]byte{1, 1}, bytes.Repeat([]byte("JPEG"), 1000)...)
	send(t, pub, frame)
	if typ, got := readMsg(t, sub); typ != websocket.MessageBinary || !bytes.Equal(got, frame) {
		t.Fatalf("frame changed on the way (%d bytes)", len(got))
	}

	// 连续推 20 帧（间隔超过限帧率）：观看端按顺序收到，最后一帧一定到
	for i := 0; i < 20; i++ {
		send(t, pub, []byte{1, 0, byte(i)})
		time.Sleep(40 * time.Millisecond)
	}
	last := -1
	for last != 19 {
		_, b := readMsg(t, sub)
		if int(b[2]) <= last {
			t.Fatalf("frames out of order: %d after %d", b[2], last)
		}
		last = int(b[2])
	}

	// 推得太快（超过每秒 30 帧）的要丢掉
	for i := 0; i < 10; i++ {
		send(t, pub, []byte{1, 0, byte(100 + i)})
	}
	time.Sleep(200 * time.Millisecond)
	send(t, pub, []byte{1, 0, 200}) // 隔了 200ms，这一帧一定转
	got := 0
	for {
		_, b := readMsg(t, sub)
		got++
		if b[2] == 200 {
			break
		}
	}
	if got > 5 {
		t.Fatalf("frames faster than the limit were forwarded: %d", got)
	}

	// 观看端走了 → 推流端收到 subs:0
	sub.Close(websocket.StatusNormalClosure, "")
	if m := readText(t, pub); m["subs"] != float64(0) {
		t.Fatalf("publisher after viewer left: %v", m)
	}
}

func TestStreamWebSocketPublisherGone(t *testing.T) {
	ts := streamTestServer(t)
	room := "遥控/测试机/视频/gone"
	pub := dialStream(t, ts, room, "pub")
	readText(t, pub)
	sub := dialStream(t, ts, room, "sub")
	readText(t, sub) // pub:true
	pub.Close(websocket.StatusNormalClosure, "")
	if m := readText(t, sub); m["pub"] != false {
		t.Fatalf("viewer after publisher left: %v", m)
	}
}

func TestStreamWebSocketReplaceAndTooBig(t *testing.T) {
	ts := streamTestServer(t)
	room := "遥控/测试机/视频/replace"
	first := dialStream(t, ts, room, "pub")
	readText(t, first)
	second := dialStream(t, ts, room, "pub")
	readText(t, second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := first.Read(ctx); err == nil {
		t.Fatal("replaced publisher must be disconnected")
	}

	big := make([]byte, streamMaxFrame+1)
	second.Write(ctx, websocket.MessageBinary, big)
	if _, _, err := second.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("too big frame: %v", err)
	}
}

func TestStreamBadRequest(t *testing.T) {
	ts := streamTestServer(t)
	for _, q := range []string{"role=pub", "room=x&role=boss"} {
		resp, err := http.Get(ts.URL + "/api/agent/v1/stream?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, resp.StatusCode)
		}
	}
	// 不是 WebSocket 的普通请求：说明要升级协议（客户端可以用它判断服务端有没有直播通道）
	resp, err := http.Get(ts.URL + "/api/agent/v1/stream?room=x&role=sub")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET: %d", resp.StatusCode)
	}
}

func TestStreamWebSocketAckLimitsFramesInFlight(t *testing.T) {
	ts := streamTestServer(t)
	room := "遥控/测试机/视频/ack"
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/agent/v1/stream?role=sub&ack=1&room=" + url.QueryEscape(room)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.CloseNow()
	readText(t, sub) // pub:false
	pub := dialStream(t, ts, room, "pub")
	readText(t, pub)
	readText(t, sub) // pub:true

	// 观看端一直不回话：推 10 帧，只能收到 2 帧（streamInFlight）
	for i := 0; i < 10; i++ {
		send(t, pub, []byte{1, 0, byte(i)})
		time.Sleep(40 * time.Millisecond)
	}
	for i := 0; i < streamInFlight; i++ {
		readMsg(t, sub)
	}
	short, scancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if _, b, err := sub.Read(short); err == nil {
		t.Fatalf("got a third frame without acking: %v", b)
	}
	scancel()
	// Read 超时会关掉连接（coder/websocket 的规矩），换一个观看端接着验证「回话后拿到的是最新一帧」
	sub2, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub2.CloseNow()
	readText(t, sub2) // pub:true
	for i := 20; i < 30; i++ {
		send(t, pub, []byte{1, 0, byte(i)})
		time.Sleep(40 * time.Millisecond)
	}
	readMsg(t, sub2)
	readMsg(t, sub2) // 两帧在路上
	sub2.Write(ctx, websocket.MessageText, []byte("ok"))
	if _, b := readMsg(t, sub2); b[2] != 29 {
		t.Fatalf("after ack want newest frame 29, got %d", b[2])
	}
}
