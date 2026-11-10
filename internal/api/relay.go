package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// 即时信箱（给「小橘遥控」用）：消息只放在内存里，不写存储端。
// 以前遥控指令和回执都当文件存进云盘：每条都要加密、传 Telegram、再从 Telegram 读回来，接收端还要每 4 秒列一次目录，
// 一条指令来回要十几秒。现在发的一方 POST，收的一方用长轮询 GET（最多挂 25 秒，消息一到马上返回），来回不到一秒。
// 服务重启信箱就清空，遥控指令本来就是即时的，丢了重发就行。照片、录音可以直接 base64 放在回执里（不超过 8 MB），
// 更大的还是走云盘文件。
//
//	POST /api/agent/v1/relay?box=<信箱名>        请求体是一条 JSON 消息（最多 8 MB），要 upload 权限
//	GET  /api/agent/v1/relay?box=<信箱名>&wait=25 取走信箱里的所有消息，没有就等最多 wait 秒，要 read 权限
//	GET  /api/agent/v1/relay/ws?box=<信箱名>       常驻推送（WebSocket，见 relay_ws.go），要 read 权限
//
// POST 的回话里 "online" 表示收件的一方现在有没有挂着常驻推送（挂着就是马上送到）。
const (
	relayMaxBody    = 8 << 20
	relayMaxTotal   = 64 << 20 // 所有信箱加起来最多占多少内存
	relayMaxBox     = 200      // 信箱名最长多少字节
	relayMaxMsgs    = 100      // 一个信箱最多攒多少条，多了丢最旧的
	relayMaxBoxes   = 1000
	relayMsgTTL     = 10 * time.Minute // 没人取的消息放多久
	relayMaxWait    = 25 * time.Second
	relayIdleExpire = 30 * time.Minute
)

type relayMsg struct {
	ID   uint64          `json:"id"` // 递增编号，常驻推送用它确认收到（ack）
	At   time.Time       `json:"at"`
	Body json.RawMessage `json:"body"`
}

type relayBox struct {
	msgs      []relayMsg
	wake      chan struct{} // 有新消息时关掉，叫醒正在等的 GET
	touched   time.Time
	listeners int                // 挂着常驻推送的连接数
	stop      context.CancelFunc // 现在的常驻推送（新的连上来时断开它）
	stopGen   uint64
}

type relayHub struct {
	mu    sync.Mutex
	boxes map[string]*relayBox
	bytes int    // 所有信箱里消息的总大小
	seq   uint64 // 消息编号
	gen   uint64 // 常驻推送连接编号
}

func newRelayHub() *relayHub { return &relayHub{boxes: map[string]*relayBox{}} }

// box 取信箱，没有就建；调用方要持有锁。
func (h *relayHub) box(name string, now time.Time) *relayBox {
	b := h.boxes[name]
	if b == nil {
		if len(h.boxes) >= relayMaxBoxes {
			h.sweep(now)
		}
		b = &relayBox{wake: make(chan struct{})}
		h.boxes[name] = b
	}
	b.touched = now
	return b
}

// sweep 清掉过期消息和很久没人用的空信箱；调用方要持有锁。
func (h *relayHub) sweep(now time.Time) {
	for name, b := range h.boxes {
		h.bytes -= b.drop(now)
		if len(b.msgs) == 0 && b.listeners == 0 && now.Sub(b.touched) > relayIdleExpire {
			delete(h.boxes, name)
		}
	}
}

// drop 丢掉过期的消息，返回丢掉的字节数。
func (b *relayBox) drop(now time.Time) int {
	i, n := 0, 0
	for i < len(b.msgs) && now.Sub(b.msgs[i].At) > relayMsgTTL {
		n += len(b.msgs[i].Body)
		i++
	}
	b.msgs = b.msgs[i:]
	return n
}

// put 放一条消息；所有信箱加起来超过 relayMaxTotal 时返回 false。
func (h *relayHub) put(name string, body json.RawMessage, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bytes+len(body) > relayMaxTotal {
		h.sweep(now)
		if h.bytes+len(body) > relayMaxTotal {
			return false
		}
	}
	b := h.box(name, now)
	h.bytes -= b.drop(now)
	h.seq++
	b.msgs = append(b.msgs, relayMsg{ID: h.seq, At: now, Body: body})
	h.bytes += len(body)
	if extra := len(b.msgs) - relayMaxMsgs; extra > 0 {
		for _, m := range b.msgs[:extra] {
			h.bytes -= len(m.Body)
		}
		b.msgs = b.msgs[extra:]
	}
	close(b.wake)
	b.wake = make(chan struct{})
	return true
}

// online 收件的一方现在有没有挂着常驻推送。
func (h *relayHub) online(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.boxes[name]
	return b != nil && b.listeners > 0
}

// peek 看编号大于 after 的消息（不取走，等对面确认收到再删）；没有的话返回一个通道，有新消息时会被关掉。
func (h *relayHub) peek(name string, after uint64, now time.Time) ([]relayMsg, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.box(name, now)
	h.bytes -= b.drop(now)
	for i, m := range b.msgs {
		if m.ID > after {
			return append([]relayMsg(nil), b.msgs[i:]...), nil
		}
	}
	return nil, b.wake
}

// ack 对面确认收到了：删掉编号不大于 upTo 的消息。
func (h *relayHub) ack(name string, upTo uint64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.box(name, now)
	i := 0
	for i < len(b.msgs) && b.msgs[i].ID <= upTo {
		h.bytes -= len(b.msgs[i].Body)
		i++
	}
	b.msgs = b.msgs[i:]
}

// take 取走所有消息；没有的话返回一个通道，有新消息时会被关掉。
func (h *relayHub) take(name string, now time.Time) ([]relayMsg, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.box(name, now)
	h.bytes -= b.drop(now)
	if len(b.msgs) > 0 {
		out := b.msgs
		for _, m := range out {
			h.bytes -= len(m.Body)
		}
		b.msgs = nil
		return out, nil
	}
	return nil, b.wake
}

func relayBoxName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.URL.Query().Get("box")
	if name == "" || len(name) > relayMaxBox {
		agentError(w, http.StatusBadRequest, "invalid_box", "box is required (at most 200 bytes)", "Use ?box=遥控/<设备名>/收件.")
		return "", false
	}
	return name, true
}

func (s *Server) relayPost(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeUpload) {
		return
	}
	name, ok := relayBoxName(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, relayMaxBody+1))
	if err != nil {
		agentError(w, http.StatusBadRequest, "bad_body", "could not read the message", "")
		return
	}
	if len(body) > relayMaxBody {
		agentError(w, http.StatusRequestEntityTooLarge, "too_large", "a relay message is at most 8 MB", "Put big files on the drive with PUT /api/agent/v1/files and send their path.")
		return
	}
	if !json.Valid(body) {
		agentError(w, http.StatusBadRequest, "bad_json", "the message must be JSON", "")
		return
	}
	if !s.relay.put(name, json.RawMessage(body), time.Now()) {
		agentError(w, http.StatusServiceUnavailable, "relay_full", "relay boxes are full right now", "Wait for the other side to pick up its messages, or send big files through the drive.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "online": s.relay.online(name)})
}

func (s *Server) relayGet(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeRead) {
		return
	}
	name, ok := relayBoxName(w, r)
	if !ok {
		return
	}
	wait := time.Duration(0)
	if n, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && n > 0 {
		wait = min(time.Duration(n)*time.Second, relayMaxWait)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		msgs, wake := s.relay.take(name, time.Now())
		if msgs != nil || wait == 0 {
			if msgs == nil {
				msgs = []relayMsg{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			wait = 0
		case <-r.Context().Done():
			return
		}
	}
}
