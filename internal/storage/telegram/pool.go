package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// Pool 让几个 Bot 共用同一个频道（都要是频道管理员，能发消息、删消息）：上传的每个分块、
// 下载的每 1MB 交给手上活最少的 Bot（一样少的轮流）。Telegram 按账号限流，Bot 多了，批量上传、看视频时不容易被限速。
// 思路来自 Teldrive（github.com/tgdrive/teldrive，MIT 许可）：1.x 的 internal/tgc/bot_selector.go 轮流选 Bot，
// 2.x 的 internal/botgateway/runner.go 挑连接最少的 Bot。
//
// 第一个是主 Bot：删除、对账都用它。其他 Bot 出错时（比如没被加成管理员、文件在别的数据中心），
// 这一次改用主 Bot 再做，出错的 Bot 停用一段时间，所以额外的 Bot 配错了也不影响使用。
// 被限速（FLOOD_WAIT）不算出错：那个 Bot（主 Bot 也一样）歇到 Telegram 说的时间，活马上交给别的 Bot；
// 全都在歇时等最早歇完的那个。
//
// 可以把几个额外的 Bot 划给 AI agent 接口专用（TG_AGENT_BOTS）：标了 storage.LaneAgent 的请求只用这些 Bot，
// 被限速时等它们歇完，不去占手机备份、网页用的 Bot；其他请求不碰这些 Bot。专用的 Bot 都出错停用（或还没连上）时，
// AI 的请求临时用普通的 Bot，不至于用不了。
type Pool struct {
	log *slog.Logger

	mu      sync.Mutex
	drivers []bot
	benched []time.Time // 每个 Bot 出错停用到什么时候（主 Bot 从不停用）
	flooded []time.Time // 每个 Bot 被限速歇到什么时候
	busy    []int       // 每个 Bot 手上正在做的活
	agent   []bool      // 每个 Bot 是不是 AI 接口专用
	next    int
}

// bot 是 Pool 里的一个 Bot（*Driver；测试里换成假的）。
type bot interface {
	Put(ctx context.Context, req storage.PutRequest) (storage.Location, error)
	ReadRange(ctx context.Context, loc storage.Location, offset, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, locs []storage.Location) error
	List(ctx context.Context, minMessageID int64, fn func(storage.Object) error) error
	getChunk(ctx context.Context, msgID, alignedOffset int64) ([]byte, error)
	forget(locs []storage.Location)
}

const (
	// benchFor 是出错的额外 Bot 停用多久。
	benchFor = 10 * time.Minute
	// 被限速时等多久以上就换 Bot：小分块换个 Bot 重传很快；大分块重传要重新上传几百 MB，宁可原地多等一会儿。
	handoffSmall = 5 * time.Second
	handoffLarge = 2 * time.Minute
	largePart    = 32 << 20
	// floodTries 是一次请求最多因限速换几次 Bot；maxFloodWait 是所有 Bot 都在歇时最多等多久，
	// 再长就报错（手机那边太久没进展会断开，快捷指令、App 会过一会儿再传）。
	floodTries   = 10
	maxFloodWait = 2 * time.Minute
)

func NewPool(primary *Driver, log *slog.Logger) *Pool { return newPool(primary, log) }

func newPool(primary bot, log *slog.Logger) *Pool {
	p := &Pool{log: log}
	p.add(primary)
	return p
}

// Add 加入一个额外的 Bot（可以在服务运行中加入）。
func (p *Pool) Add(d *Driver) { p.add(d) }

// AddAgent 加入一个 AI 接口专用的 Bot（见 Pool 的说明）。
func (p *Pool) AddAgent(d *Driver) { p.addLane(d, true) }

func (p *Pool) add(d bot) { p.addLane(d, false) }

func (p *Pool) addLane(d bot, agent bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agent = append(p.agent, agent)
	p.drivers = append(p.drivers, d)
	p.benched = append(p.benched, time.Time{})
	p.flooded = append(p.flooded, time.Time{})
	p.busy = append(p.busy, 0)
}

// Bots 返回 Bot 总数（含主 Bot）。
func (p *Pool) Bots() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.drivers)
}

func (p *Pool) Name() string { return "telegram" }

func (p *Pool) primary() bot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drivers[0]
}

// pick 选一个能用的 Bot 并记上它多了一份活（用完要 done）：出错停用中、被限速歇着的跳过，
// 剩下的挑手上活最少的，一样少的从 next 开始轮流。全都在歇时返回 k=-1 和最早歇完还要等多久。
// AI 的请求（lane）只在专用的 Bot 里挑，专用的都停用了才在普通的里挑；其他请求只在普通的里挑。
func (p *Pool) pick(lane storage.Lane) (k int, b bot, wait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.drivers)
	now := time.Now()
	agent := false
	if lane == storage.LaneAgent {
		for j := range n {
			if p.agent[j] && !now.Before(p.benched[j]) {
				agent = true // 有能用的专用 Bot（哪怕在歇着）：只等它们
				break
			}
		}
	}
	k = -1
	for i := range n {
		j := (p.next + i) % n
		if p.agent[j] != agent {
			continue
		}
		if j != 0 && now.Before(p.benched[j]) {
			continue
		}
		if now.Before(p.flooded[j]) {
			if d := p.flooded[j].Sub(now); wait == 0 || d < wait {
				wait = d
			}
			continue
		}
		if k < 0 || p.busy[j] < p.busy[k] {
			k = j
		}
	}
	if k < 0 {
		return -1, nil, wait
	}
	p.next = k + 1
	p.busy[k]++
	return k, p.drivers[k], 0
}

func (p *Pool) done(k int) {
	p.mu.Lock()
	p.busy[k]--
	p.mu.Unlock()
}

func (p *Pool) bench(k int, err error) {
	p.mu.Lock()
	p.benched[k] = time.Now().Add(benchFor)
	p.mu.Unlock()
	p.log.Warn("extra telegram bot failed; not using it for a while", "bot", k, "pause", benchFor, "err", err)
}

func (p *Pool) flood(k int, d time.Duration) {
	p.mu.Lock()
	if until := time.Now().Add(d); until.After(p.flooded[k]) {
		p.flooded[k] = until
	}
	p.mu.Unlock()
	p.log.Warn("telegram bot flood-limited; handing work to another bot", "bot", k, "wait", d)
}

// do 用选中的 Bot 执行 fn。被限速时那个 Bot 歇着，换别的 Bot 再做；额外的 Bot 出别的错（不是请求被取消）时
// 停用它，改用主 Bot 再做一次。retry 在每次重做前调用，返回 false 时不重做（比如上传的数据没法从头再读一遍）。
func (p *Pool) do(ctx context.Context, handoff time.Duration, fn func(context.Context, bot) error, retry func() bool) error {
	hctx := withHandoff(ctx, handoff)
	for try := 0; ; try++ {
		k, d, wait := p.pick(storage.LaneOf(ctx))
		if k < 0 {
			if wait > maxFloodWait {
				return fmt.Errorf("telegram: every bot is flood-limited for at least %s", wait.Round(time.Second))
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := fn(hctx, d)
		p.done(k)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if w, ok := tgerr.AsFloodWait(err); ok && try < floodTries {
			p.flood(k, w)
			if retry != nil && !retry() {
				return err
			}
			continue
		}
		if k == 0 {
			return err
		}
		p.bench(k, err)
		if retry != nil && !retry() {
			return err
		}
		return fn(ctx, p.primary())
	}
}

func (p *Pool) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	var loc storage.Location
	handoff := handoffSmall
	if req.Size > largePart {
		handoff = handoffLarge
	}
	err := p.do(ctx, handoff, func(ctx context.Context, d bot) (err error) {
		loc, err = d.Put(ctx, req)
		return err
	}, func() bool {
		s, ok := req.Body.(io.Seeker) // 分块存在临时文件里，可以从头再读
		if !ok {
			return false
		}
		_, err := s.Seek(0, io.SeekStart)
		return err == nil
	})
	return loc, err
}

func (p *Pool) ReadRange(ctx context.Context, loc storage.Location, offset, length int64) (io.ReadCloser, error) {
	if p.Bots() == 1 { // 只有主 Bot（专用的 Bot 一定是额外加的）
		return p.primary().ReadRange(ctx, loc, offset, length)
	}
	return newRangeReader(ctx, p.getChunk, loc.MessageID, offset, length), nil
}

func (p *Pool) getChunk(ctx context.Context, msgID, alignedOffset int64) ([]byte, error) {
	var data []byte
	err := p.do(ctx, handoffSmall, func(ctx context.Context, d bot) (err error) {
		data, err = d.getChunk(ctx, msgID, alignedOffset)
		return err
	}, nil)
	return data, err
}

func (p *Pool) Delete(ctx context.Context, locs []storage.Location) error {
	if err := p.primary().Delete(ctx, locs); err != nil {
		return err
	}
	p.mu.Lock()
	others := p.drivers[1:]
	p.mu.Unlock()
	for _, d := range others {
		d.forget(locs)
	}
	return nil
}

func (p *Pool) List(ctx context.Context, minMessageID int64, fn func(storage.Object) error) error {
	return p.primary().List(ctx, minMessageID, fn)
}
