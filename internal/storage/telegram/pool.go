package telegram

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// Pool 让几个 Bot 共用同一个频道（都要是频道管理员，能发消息、删消息）：上传的每个分块、
// 下载的每 1MB 轮流交给不同的 Bot。Telegram 按账号限流，Bot 多了，批量上传、看视频时不容易被限速。
// 思路来自 Teldrive 1.x（github.com/tgdrive/teldrive，MIT 许可）的 internal/tgc/bot_selector.go：轮流选 Bot。
//
// 第一个是主 Bot：删除、对账都用它。其他 Bot 出错时（比如没被加成管理员、文件在别的数据中心），
// 这一次改用主 Bot 再做，出错的 Bot 停用一段时间，所以额外的 Bot 配错了也不影响使用。
type Pool struct {
	log *slog.Logger

	mu      sync.Mutex
	drivers []bot
	benched []time.Time // 每个 Bot 停用到什么时候
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

// benchFor 是出错的额外 Bot 停用多久。
const benchFor = 10 * time.Minute

func NewPool(primary *Driver, log *slog.Logger) *Pool { return newPool(primary, log) }

func newPool(primary bot, log *slog.Logger) *Pool {
	return &Pool{log: log, drivers: []bot{primary}, benched: []time.Time{{}}}
}

// Add 加入一个额外的 Bot（可以在服务运行中加入）。
func (p *Pool) Add(d *Driver) { p.add(d) }

func (p *Pool) add(d bot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drivers = append(p.drivers, d)
	p.benched = append(p.benched, time.Time{})
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

// pick 轮流选一个 Bot，停用中的跳过（主 Bot 从不停用）。
func (p *Pool) pick() (int, bot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.drivers)
	now := time.Now()
	for range n {
		k := p.next % n
		p.next++
		if k == 0 || now.After(p.benched[k]) {
			return k, p.drivers[k]
		}
	}
	return 0, p.drivers[0]
}

func (p *Pool) bench(k int, err error) {
	p.mu.Lock()
	p.benched[k] = time.Now().Add(benchFor)
	p.mu.Unlock()
	p.log.Warn("extra telegram bot failed; not using it for a while", "bot", k, "pause", benchFor, "err", err)
}

// do 用轮到的 Bot 执行 fn。额外的 Bot 出错（不是请求被取消）时停用它，改用主 Bot 再做一次；
// retry 返回 false 时不重做（比如上传的数据没法从头再读一遍）。
func (p *Pool) do(ctx context.Context, fn func(bot) error, retry func() bool) error {
	k, d := p.pick()
	err := fn(d)
	if err == nil || k == 0 || ctx.Err() != nil {
		return err
	}
	p.bench(k, err)
	if retry != nil && !retry() {
		return err
	}
	return fn(p.primary())
}

func (p *Pool) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	var loc storage.Location
	err := p.do(ctx, func(d bot) (err error) {
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
	if p.Bots() == 1 {
		return p.primary().ReadRange(ctx, loc, offset, length)
	}
	return newRangeReader(ctx, p.getChunk, loc.MessageID, offset, length), nil
}

func (p *Pool) getChunk(ctx context.Context, msgID, alignedOffset int64) ([]byte, error) {
	var data []byte
	err := p.do(ctx, func(d bot) (err error) {
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
