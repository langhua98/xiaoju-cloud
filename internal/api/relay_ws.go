package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// 即时信箱的常驻推送（给「小橘遥控」的被控端用）：被控端挂一条 WebSocket 一直不断，指令一到服务器马上推下去。
// 长轮询（GET relay?wait=25）每 25 秒就要回一次「没消息」，被控端一小时要醒来 144 次；常驻推送没指令时一个字节都不发，
// 被控端 CPU 可以一直睡，数据包到了 Wi-Fi 会把它叫醒。连接会被中间的路由器 / 代理悄悄掐掉，所以被控端自己定时发心跳，
// 间隔由它自己摸（能撑多久就拉多长）；服务端从不主动发心跳，免得把被控端叫醒。
//
//	GET /api/agent/v1/relay/ws?box=<信箱名>   要 read 权限
//
// 服务端 → 被控端（文字消息，JSON）：
//   - {"messages":[{"id","at","body"},…]}  新指令（和长轮询的格式一样，多了 id）
//   - {"pong":<原样带回>}                   回心跳
//
// 被控端 → 服务端（文字消息，JSON）：
//   - {"ack":<id>}   编号不大于 id 的指令收到了，服务端删掉。没确认的指令留在信箱里（最多 10 分钟），
//     连接断了重连上来、或者走长轮询，都会再拿到一次（被控端按指令 id 去重）。
//   - {"ping":<任意>} 心跳，服务端回 {"pong":…}。
//
// 同一个信箱只留最新一条常驻推送：新的连上来，旧的断掉（旧的多半已经是死连接）。
// 发指令的一方 POST relay 时，回话里 "online": true 表示被控端正挂着常驻推送。
const (
	relayWSIdle    = 30 * time.Minute // 这么久没收到被控端任何消息（心跳也没有），就当它掉线了
	relayWSMaxConn = 24 * time.Hour   // 一条连接最长连多久
	relayWSMaxIn   = 4 << 10          // 被控端发来的消息很小（ack、心跳）
)

type relayWSIn struct {
	Ack  *uint64         `json:"ack"`
	Ping json.RawMessage `json:"ping"`
}

// takeOver 记下这个信箱现在的常驻推送（stop 用来断开它），返回它的编号和被顶掉的旧连接的 stop。
func (h *relayHub) takeOver(name string, stop context.CancelFunc, now time.Time) (uint64, context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.box(name, now)
	old := b.stop
	h.gen++
	b.stop, b.stopGen = stop, h.gen
	b.listeners++
	return h.gen, old
}

// release 常驻推送断开了（gen 是 takeOver 给的编号）。
func (h *relayHub) release(name string, gen uint64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.box(name, now)
	b.listeners = max(0, b.listeners-1)
	if b.stopGen == gen {
		b.stop, b.stopGen = nil, 0
	}
}

func (s *Server) relayWS(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeRead) {
		return
	}
	name, ok := relayBoxName(w, r)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept 已经回了错误
	}
	defer c.CloseNow()
	c.SetReadLimit(relayWSMaxIn)

	ctx, cancel := context.WithTimeout(r.Context(), relayWSMaxConn)
	defer cancel()
	gen, old := s.relay.takeOver(name, cancel, time.Now())
	if old != nil {
		old()
	}
	defer s.relay.release(name, gen, time.Now())

	pongs := make(chan json.RawMessage, 4)
	go func() {
		defer cancel()
		for {
			rctx, rcancel := context.WithTimeout(ctx, relayWSIdle)
			typ, b, err := c.Read(rctx)
			rcancel()
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var in relayWSIn
			if json.Unmarshal(b, &in) != nil {
				continue
			}
			if in.Ack != nil {
				s.relay.ack(name, *in.Ack, time.Now())
			}
			if in.Ping != nil {
				select {
				case pongs <- in.Ping:
				default:
				}
			}
		}
	}()

	var sent uint64
	for {
		msgs, wake := s.relay.peek(name, sent, time.Now())
		if msgs != nil {
			b, _ := json.Marshal(map[string]any{"messages": msgs})
			if streamWrite(ctx, c, websocket.MessageText, b) != nil {
				return
			}
			sent = msgs[len(msgs)-1].ID
			continue
		}
		select {
		case <-wake:
		case p := <-pongs:
			b, _ := json.Marshal(map[string]json.RawMessage{"pong": p})
			if streamWrite(ctx, c, websocket.MessageText, b) != nil {
				return
			}
		case <-ctx.Done():
			c.Close(websocket.StatusGoingAway, "")
			return
		}
	}
}
