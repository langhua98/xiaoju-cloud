package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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
// 推流端发二进制消息，服务端原样转发，只看第 1 个字节（类型）：
//   - 2 = 声音：一包都不丢，按顺序发给每个观看端（最多攒 streamAudioQueue 包，再多丢最旧的），不占 ack 名额；
//   - 4 = H.264 画面：[4][角度/90][采集时间 8 字节][标志，第 0 位 = 关键帧][Annex-B 数据]。
//     H.264 的普通帧要靠前面的帧才能解出来，不能像 JPEG 那样随便丢：按顺序发，观看端跟不上（排队超过
//     streamH264MaxLag）就把排着的全扔掉，等下一个关键帧再接着发（关键帧自己就能解，正好从这里追上）。
//     房间记着最近一个关键帧和它后面的帧，中途进来的观看端马上就有画面，不用等下一个关键帧。
//   - 其他 = 一帧画面（JPEG）：每个观看端只留最新一帧，慢了就丢旧帧。
//
// 小橘遥控的格式见安卓 StreamClient.kt（画面 1/3、声音 2，带采集时间戳，观看端用它对齐声音和画面）。
// 服务端发给推流端的文字消息 {"subs":N}：观看人数变了就发，推流端没人看了就自己停。
// 服务端发给观看端的文字消息 {"pub":true|false}：推流端连上了 / 断开了。第一条还带 "h264":true，表示这个服务端能转 H.264。
// 观看端加 &ack=1：每收到一帧回一条任意文字消息（比如 "ok"），服务端最多让 streamInFlight 帧在路上
// （&window=N 改成 N 帧，最多 streamMaxWindow；离得远、来回慢的时候在路上的帧多一点帧率才上得去），
// 而且在路上的字节不超过 streamInFlightBytes（大的 JPEG 帧不会因为 window 大堆在网络缓冲区里）。
// 不加的话，帧会先堆在系统的网络缓冲区里，观看端慢的时候看到的是好几秒前的画面。
// 房间名由观看端每次随机生成（遥控/<设备名>/视频/<随机串>），放在发给被控端的指令里，别人猜不到。
const (
	streamMaxRoom       = 200                    // 房间名最长多少字节
	streamMaxFrame      = 1 << 20                // 一帧最大 1 MB（480p 的 JPEG 一般几十 KB）
	streamMaxRooms      = 50                     // 同时最多几个房间
	streamMaxSubs       = 4                      // 一个房间最多几个人同时看
	streamMinGap        = 33 * time.Millisecond  // 最多每秒 30 帧，再快的直接丢
	streamMaxConn       = 4 * time.Hour          // 一条连接最长连多久（忘了关的直播也会断）
	streamPing          = 20 * time.Second       // 心跳，免得中间的代理把安静的连接断掉
	streamWriteWait     = 10 * time.Second       // 一帧写了这么久还没写完，就当对面掉线了
	streamInFlight      = 2                      // ack=1 时默认最多几帧发出去了还没收到回话
	streamMaxWindow     = 16                     // window 最大多少
	streamInFlightBytes = 128 << 10              // ack=1 时在路上的帧最多多少字节（至少放一帧）
	streamKindAudio     = 2                      // 第 1 字节是 2 的消息是声音
	streamKindH264      = 4                      // 第 1 字节是 4 的消息是 H.264 画面
	streamH264MaxLag    = 800 * time.Millisecond // H.264 帧排队超过这么久没发出去，就扔掉等下一个关键帧
	streamGOPFrames     = 150                    // 房间最多记多少帧（关键帧 + 后面的帧）给中途进来的观看端
	streamGOPBytes      = 4 << 20                // 最多记多少字节
	streamAudioQueue    = 64                     // 每个观看端最多攒多少包声音（100 ms 一包，约 6 秒）
	streamAudioGap      = 10 * time.Millisecond  // 声音包最快每 10 ms 一包
)

type streamHub struct {
	mu    sync.Mutex
	rooms map[string]*streamRoom
}

type streamRoom struct {
	pub      *streamPeer
	subs     map[*streamPeer]struct{}
	gop      [][]byte // H.264：最近一个关键帧和它后面的帧（空 = 还没有关键帧，或者攒太多了）
	gopBytes int
}

// remember 记下一帧 H.264，给中途进来的观看端用。
func (r *streamRoom) remember(f []byte, key bool) {
	switch {
	case key:
		r.gop, r.gopBytes = append(r.gop[:0:0], f), len(f)
	case len(r.gop) == 0:
	case len(r.gop) >= streamGOPFrames || r.gopBytes+len(f) > streamGOPBytes:
		r.gop, r.gopBytes = nil, 0 // 关键帧隔太久了，不记了，新观看端等下一个关键帧
	default:
		r.gop, r.gopBytes = append(r.gop, f), r.gopBytes+len(f)
	}
}

// streamItem 是排队等着发的一帧 H.264。
type streamItem struct {
	b  []byte
	at time.Time
}

// h264Key 是不是 H.264 关键帧（标志字节的第 0 位）。
func h264Key(f []byte) bool { return len(f) > 10 && f[10]&1 == 1 }

// streamPeer 是一条连接。frame 只放最新一帧（缓冲 1，新帧来了旧的还没写出去就扔掉），ctrl 放文字消息。
type streamPeer struct {
	frame     chan []byte
	ctrl      chan []byte
	acks      chan struct{}      // ack=1 的观看端：处理完一帧回的话
	audio     chan []byte        // 声音包，按顺序发
	vmu       sync.Mutex         // 管下面三个
	vq        []streamItem       // H.264 帧，按顺序发
	needKey   bool               // 扔过帧了，等关键帧
	vsig      chan struct{}      // 有新的 H.264 帧了
	stop      context.CancelFunc // 推流端被新的推流端顶掉时用来断开它
	last      time.Time          // 推流端：上一帧转发的时间（限帧率，只在它自己的读循环里用）
	lastAudio time.Time          // 推流端：上一包声音转发的时间
}

func newStreamPeer() *streamPeer {
	return &streamPeer{frame: make(chan []byte, 1), ctrl: make(chan []byte, 8), acks: make(chan struct{}, 16),
		audio: make(chan []byte, streamAudioQueue), vsig: make(chan struct{}, 1)}
}

// offerH264 排一帧 H.264：关键帧来了，前面没发出去的都不要了（关键帧自己能解）；
// 等关键帧时普通帧直接扔；排队太久（观看端跟不上）就全扔掉，等下一个关键帧。
func (p *streamPeer) offerH264(f []byte, now time.Time) {
	p.vmu.Lock()
	switch {
	case h264Key(f):
		clear(p.vq) // 别让扔掉的帧还占着内存
		p.vq = append(p.vq[:0], streamItem{f, now})
		p.needKey = false
	case p.needKey:
	case len(p.vq) > 0 && now.Sub(p.vq[0].at) > streamH264MaxLag:
		clear(p.vq)
		p.vq = p.vq[:0]
		p.needKey = true
	default:
		p.vq = append(p.vq, streamItem{f, now})
	}
	p.vmu.Unlock()
	select {
	case p.vsig <- struct{}{}:
	default:
	}
}

// nextH264 取下一帧要发的 H.264；没有返回 nil。
func (p *streamPeer) nextH264() []byte {
	p.vmu.Lock()
	defer p.vmu.Unlock()
	if len(p.vq) == 0 {
		return nil
	}
	f := p.vq[0].b
	p.vq[0] = streamItem{}
	p.vq = p.vq[1:]
	return f
}

// preload 新观看端进房间时，先排上房间记着的关键帧和后面的帧；没有就等关键帧。
func (p *streamPeer) preload(gop [][]byte, now time.Time) {
	p.vmu.Lock()
	defer p.vmu.Unlock()
	if len(gop) == 0 {
		p.needKey = true
		return
	}
	for _, f := range gop {
		p.vq = append(p.vq, streamItem{f, now})
	}
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

// offerAudio 放一包声音；攒满了（观看端很久没收）丢最旧的一包。
func (p *streamPeer) offerAudio(a []byte) {
	for {
		select {
		case p.audio <- a:
			return
		default:
		}
		select {
		case <-p.audio:
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
		r.gop, r.gopBytes = nil, 0
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
	p.preload(r.gop, time.Now())
	p.say(map[string]bool{"pub": r.pub != nil, "h264": true})
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
			r.gop, r.gopBytes = nil, 0
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
	kind := byte(0)
	if len(f) > 0 {
		kind = f[0]
	}
	now := time.Now()
	if kind == streamKindH264 {
		r.remember(f, h264Key(f))
	}
	for s := range r.subs {
		switch kind {
		case streamKindAudio:
			s.offerAudio(f)
		case streamKindH264:
			s.offerH264(f, now)
		default:
			s.offer(f)
		}
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
	window := streamInFlight
	if n, err := strconv.Atoi(q.Get("window")); err == nil && n >= 1 {
		window = min(n, streamMaxWindow)
	}
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

	// 写：文字消息、声音、画面，谁先到写谁。写不出去（对面掉线）就整条连接结束。
	// ack=1 时，在路上的帧满了就先不取帧（JPEG：新帧来了会把等着的旧帧换掉；H.264：排着等）。
	go func() {
		defer cancel()
		var inflight []int // ack=1：发出去还没回话的帧的大小，按顺序
		inflightBytes := 0
		canSend := func() bool {
			return !ack || len(inflight) == 0 || (len(inflight) < window && inflightBytes < streamInFlightBytes)
		}
		sent := func(n int) {
			if ack {
				inflight = append(inflight, n)
				inflightBytes += n
			}
		}
		for {
			frames, vsig := p.frame, p.vsig
			if !canSend() {
				frames, vsig = nil, nil
			}
			var err error
			select {
			case <-ctx.Done():
				return
			case b := <-p.ctrl:
				err = streamWrite(ctx, c, websocket.MessageText, b)
			case <-p.acks:
				if len(inflight) > 0 {
					inflightBytes -= inflight[0]
					inflight = inflight[1:]
				}
			case a := <-p.audio:
				err = streamWrite(ctx, c, websocket.MessageBinary, a)
			case f := <-frames:
				err = streamWrite(ctx, c, websocket.MessageBinary, f)
				sent(len(f))
			case <-vsig:
			}
			// 排着的 H.264 帧：名额够就一直发
			for err == nil && canSend() {
				f := p.nextH264()
				if f == nil {
					break
				}
				err = streamWrite(ctx, c, websocket.MessageBinary, f)
				sent(len(f))
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
		now := time.Now()
		if len(data) > 0 && data[0] == streamKindAudio {
			if now.Sub(p.lastAudio) >= streamAudioGap {
				p.lastAudio = now
				s.stream.forward(name, p, data)
			}
		} else if len(data) > 0 && data[0] == streamKindH264 {
			s.stream.forward(name, p, data) // H.264 不能限帧率丢帧（后面的帧会解不出来）
		} else if now.Sub(p.last) >= streamMinGap {
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
