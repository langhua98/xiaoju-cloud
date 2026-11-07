package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// 直播通道（给「小橘遥控」的实时画面用）：WebSocket 长连接，帧只在内存里转一下，不写存储端（不碰 Telegram）。
// 以前实时画面走即时信箱：每一帧都是一次 HTTPS 请求，发一帧要等一个来回，服务器离得远，一秒只能传两三帧。
// 现在推流端和观看端各连一条长连接：推流端连续推，不用等回话；服务端给每个观看端只留最新一帧，
// 观看端网络慢就丢旧帧，画面永远是最新的，不会越拖越慢。
//
//	GET /api/agent/v1/stream?room=<房间名>&role=pub   推流（被控端），要 upload 权限
//	GET /api/agent/v1/stream?room=<房间名>&role=sub   观看（控制端），要 read 权限
//
// 推流端发二进制消息，一条就是一帧。服务端原样转发，不看内容（小橘遥控的帧格式：第 1 字节是类型，
// 第 2 字节是画面要转的角度/90，后面是 JPEG）。
// 服务端发给推流端的文字消息 {"subs":N}：观看人数变了就发，推流端没人看了就自己停。
// 服务端发给观看端的文字消息 {"pub":true|false}：推流端连上了 / 断开了。
// 观看端加 &ack=1：每处理完一帧回一条任意文字消息（比如 "ok"），服务端最多让 streamInFlight 帧在路上。
// 不加的话，帧会先堆在系统的网络缓冲区里，观看端慢的时候看到的是好几秒前的画面。
// 房间名由观看端每次随机生成（遥控/<设备名>/视频/<随机串>），放在发给被控端的指令里，别人猜不到。
const (
	streamMaxRoom   = 200                   // 房间名最长多少字节
	streamMaxFrame  = 1 << 20               // 一帧最大 1 MB（480p 的 JPEG 一般几十 KB）
	streamMaxRooms  = 50                    // 同时最多几个房间
	streamMaxSubs   = 4                     // 一个房间最多几个人同时看
	streamMinGap    = 33 * time.Millisecond // 最多每秒 30 帧，再快的直接丢
	streamMaxConn   = 4 * time.Hour         // 一条连接最长连多久（忘了关的直播也会断）
	streamPing      = 20 * time.Second      // 心跳，免得中间的代理把安静的连接断掉
	streamWriteWait = 10 * time.Second      // 一帧写了这么久还没写完，就当对面掉线了
	streamInFlight  = 2                     // ack=1 时最多几帧发出去了还没收到回话
)

type streamHub struct {
	mu    sync.Mutex
	rooms map[string]*streamRoom
}

type streamRoom struct {
	pub  *streamPeer
	subs map[*streamPeer]struct{}
}

// streamPeer 是一条连接。frame 只放最新一帧（缓冲 1，新帧来了旧的还没写出去就扔掉），ctrl 放文字消息。
type streamPeer struct {
	frame chan []byte
	ctrl  chan []byte
	acks  chan struct{}      // ack=1 的观看端：处理完一帧回的话
	stop  context.CancelFunc // 推流端被新的推流端顶掉时用来断开它
	last  time.Time          // 推流端：上一帧转发的时间（限帧率，只在它自己的读循环里用）
}

func newStreamPeer() *streamPeer {
	return &streamPeer{frame: make(chan []byte, 1), ctrl: make(chan []byte, 8), acks: make(chan struct{}, 16)}
}

// offer 放一帧；上一帧还没写出去就换成这一帧。只有房间的推流端会调用它（持有 hub 锁），不会同时有两个人放。
func (p *streamPeer) offer(f []byte) {
	for {
		select {
		case p.frame <- f:
			return
		default:
		}
		select {
		case <-p.frame: // 扔掉没来得及发的旧帧
		default:
		}
	}
}

// say 发一条文字消息；对面很久不读、堆满了就丢（它多半已经掉线了）。
func (p *streamPeer) say(v any) {
	b, _ := json.Marshal(v)
	select {
	case p.ctrl <- b:
	default:
	}
}

func newStreamHub() *streamHub { return &streamHub{rooms: map[string]*streamRoom{}} }

// join 进房间。新的推流端会顶掉原来的推流端（返回被顶掉的那个，由调用方断开它）。房间满了返回 ok=false。
func (h *streamHub) join(name string, p *streamPeer, pub bool) (old *streamPeer, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[name]
	if r == nil {
		if len(h.rooms) >= streamMaxRooms {
			return nil, false
		}
		r = &streamRoom{subs: map[*streamPeer]struct{}{}}
		h.rooms[name] = r
	}
	if pub {
		old = r.pub
		r.pub = p
		p.say(map[string]int{"subs": len(r.subs)})
		for s := range r.subs {
			s.say(map[string]bool{"pub": true})
		}
		return old, true
	}
	if len(r.subs) >= streamMaxSubs {
		return nil, false
	}
	r.subs[p] = struct{}{}
	p.say(map[string]bool{"pub": r.pub != nil})
	if r.pub != nil {
		r.pub.say(map[string]int{"subs": len(r.subs)})
	}
	return nil, true
}

// leave 离开房间；房间空了就删掉。
func (h *streamHub) leave(name string, p *streamPeer, pub bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[name]
	if r == nil {
		return
	}
	if pub {
		if r.pub == p { // 被顶掉的旧推流端离开时，不影响新的
			r.pub = nil
			for s := range r.subs {
				s.say(map[string]bool{"pub": false})
			}
		}
	} else if _, in := r.subs[p]; in {
		delete(r.subs, p)
		if r.pub != nil {
			r.pub.say(map[string]int{"subs": len(r.subs)})
		}
	}
	if r.pub == nil && len(r.subs) == 0 {
		delete(h.rooms, name)
	}
}

// forward 把推流端的一帧交给房间里每个观看端。p 已经不是这个房间的推流端（被顶掉了）就不转。
func (h *streamHub) forward(name string, p *streamPeer, f []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[name]
	if r == nil || r.pub != p {
		return
	}
	for s := range r.subs {
		s.offer(f)
	}
}

func streamWrite(ctx context.Context, c *websocket.Conn, typ websocket.MessageType, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, streamWriteWait)
	defer cancel()
	return c.Write(wctx, typ, b)
}

func (s *Server) streamWS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, role := q.Get("room"), q.Get("role")
	if name == "" || len(name) > streamMaxRoom {
		agentError(w, http.StatusBadRequest, "invalid_room", "room is required (at most 200 bytes)", "Use ?room=遥控/<设备名>/视频/<随机串>.")
		return
	}
	pub := role == "pub"
	ack := !pub && q.Get("ack") == "1"
	if !pub && role != "sub" {
		agentError(w, http.StatusBadRequest, "invalid_role", "role must be pub or sub", "")
		return
	}
	scope := ScopeRead
	if pub {
		scope = ScopeUpload
	}
	if !s.need(w, r, scope) {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept 已经回了错误
	}
	defer c.CloseNow()
	c.SetReadLimit(streamMaxFrame)

	ctx, cancel := context.WithTimeout(r.Context(), streamMaxConn)
	defer cancel()
	p := newStreamPeer()
	p.stop = cancel
	old, ok := s.stream.join(name, p, pub)
	if !ok {
		c.Close(websocket.StatusTryAgainLater, "too many streams or viewers")
		return
	}
	if old != nil {
		old.stop()
	}
	defer s.stream.leave(name, p, pub)

	// 写：文字消息和最新一帧，谁先到写谁。写不出去（对面掉线）就整条连接结束。
	// ack=1 时，在路上的帧满了就先不取帧（新帧来了会把等着的旧帧换掉）。
	go func() {
		defer cancel()
		credits := streamInFlight
		for {
			frames := p.frame
			if ack && credits <= 0 {
				frames = nil
			}
			var err error
			select {
			case <-ctx.Done():
				return
			case b := <-p.ctrl:
				err = streamWrite(ctx, c, websocket.MessageText, b)
			case <-p.acks:
				credits = min(credits+1, streamInFlight)
			case f := <-frames:
				err = streamWrite(ctx, c, websocket.MessageBinary, f)
				credits--
			}
			if err != nil {
				return
			}
		}
	}()

	if ack {
		// 观看端回的每条文字消息都算「处理完一帧」
		go streamPinger(ctx, c, cancel)
		for {
			typ, _, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				select {
				case p.acks <- struct{}{}:
				default:
				}
			}
		}
	}
	if !pub {
		// 观看端只收不发：CloseRead 负责读心跳回应和关闭帧，连接断了 done 就关。
		done := c.CloseRead(ctx)
		go streamPinger(done, c, cancel)
		<-done.Done()
		return
	}
	go streamPinger(ctx, c, cancel)
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if now := time.Now(); now.Sub(p.last) >= streamMinGap {
			p.last = now
			s.stream.forward(name, p, data)
		}
	}
}

// streamPinger 定时发心跳；对面不回就断开。
func streamPinger(ctx context.Context, c *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(streamPing)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, streamWriteWait)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
