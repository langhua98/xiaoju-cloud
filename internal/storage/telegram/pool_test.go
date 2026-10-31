package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// fakeBot 是 Pool 测试用的 Bot：记录每个操作由谁做，broken 时什么都失败。
type fakeBot struct {
	name   string
	doc    *fakeDoc
	broken bool
	flood  int // 接下来几次请求回 FLOOD_WAIT_30

	mu     sync.Mutex
	puts   int
	chunks int
	bodies [][]byte
	forgot int
}

func (b *fakeBot) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	data, _ := io.ReadAll(io.LimitReader(req.Body, 3)) // 读一部分再失败：重做时要从头读
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts++
	if b.flood > 0 {
		b.flood--
		return storage.Location{}, fmt.Errorf("send media: %w", tgerr.New(420, "FLOOD_WAIT_30"))
	}
	if b.broken {
		return storage.Location{}, errors.New(b.name + " is broken")
	}
	rest, _ := io.ReadAll(req.Body)
	b.bodies = append(b.bodies, append(data, rest...))
	return storage.Location{ChannelID: 7, MessageID: int64(b.puts)}, nil
}

func (b *fakeBot) ReadRange(ctx context.Context, loc storage.Location, offset, length int64) (io.ReadCloser, error) {
	return newRangeReader(ctx, b.getChunk, loc.MessageID, offset, length), nil
}

func (b *fakeBot) Delete(ctx context.Context, locs []storage.Location) error { return nil }

func (b *fakeBot) List(ctx context.Context, minMessageID int64, fn func(storage.Object) error) error {
	return fn(storage.Object{Name: b.name})
}

func (b *fakeBot) getChunk(ctx context.Context, msgID, off int64) ([]byte, error) {
	b.mu.Lock()
	b.chunks++
	broken := b.broken
	b.mu.Unlock()
	if broken {
		return nil, errors.New(b.name + " is broken")
	}
	return b.doc.fetch(ctx, msgID, off)
}

func (b *fakeBot) forget(locs []storage.Location) {
	b.mu.Lock()
	b.forgot += len(locs)
	b.mu.Unlock()
}

func TestPoolRotatesAndFallsBack(t *testing.T) {
	doc := newFakeDoc(8 * chunkSize)
	doc.delay = 0
	main, good, bad := &fakeBot{name: "main", doc: doc}, &fakeBot{name: "good", doc: doc}, &fakeBot{name: "bad", doc: doc, broken: true}
	p := newPool(main, slog.New(slog.DiscardHandler))
	p.add(good)
	p.add(bad)
	if p.Bots() != 3 {
		t.Fatalf("bots = %d", p.Bots())
	}

	// 下载：每 1MB 轮流交给不同的 Bot，坏掉的那个改用主 Bot，数据一个字节都不错
	r, _ := p.ReadRange(context.Background(), storage.Location{MessageID: 1}, 5, int64(len(doc.data))-5)
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, doc.data[5:]) {
		t.Fatalf("read through pool: %v (%d bytes)", err, len(got))
	}
	if good.chunks == 0 || main.chunks == 0 {
		t.Fatalf("chunks main=%d good=%d: expected both to download", main.chunks, good.chunks)
	}
	// 并发下载时停用前可能又轮到它一次
	if bad.chunks < 1 || bad.chunks > 2 {
		t.Fatalf("broken bot was tried %d times, want once or twice (then paused)", bad.chunks)
	}
	if main.chunks+good.chunks != 8 {
		t.Fatalf("chunks main=%d good=%d, want 8 in total", main.chunks, good.chunks)
	}

	// 上传：坏掉的 Bot 停用中，不再轮到它
	body := []byte("hello part")
	for range 4 {
		if _, err := p.Put(context.Background(), storage.PutRequest{Body: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}
	if bad.puts != 0 || main.puts+good.puts != 4 {
		t.Fatalf("puts main=%d good=%d bad=%d", main.puts, good.puts, bad.puts)
	}

	// 停用到期后又会轮到它：上传失败时从头重读数据交给主 Bot
	p.mu.Lock()
	p.benched[2] = p.benched[2].AddDate(-1, 0, 0)
	p.next = 2
	p.mu.Unlock()
	before := main.puts
	if _, err := p.Put(context.Background(), storage.PutRequest{Body: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if bad.puts != 1 || main.puts != before+1 || !bytes.Equal(main.bodies[len(main.bodies)-1], body) {
		t.Fatalf("fallback put: bad=%d main=%d->%d body=%q", bad.puts, before, main.puts, main.bodies[len(main.bodies)-1])
	}

	// 数据没法从头再读时不重做，报原来的错
	p.mu.Lock()
	p.benched[2] = p.benched[2].AddDate(-1, 0, 0)
	p.next = 2
	p.mu.Unlock()
	if _, err := p.Put(context.Background(), storage.PutRequest{Body: io.MultiReader(bytes.NewReader(body)), Size: int64(len(body))}); err == nil {
		t.Fatal("non-seekable body must not be retried")
	}

	// 删除、对账用主 Bot，其他 Bot 丢掉缓存
	if err := p.Delete(context.Background(), []storage.Location{{MessageID: 1}, {MessageID: 2}}); err != nil {
		t.Fatal(err)
	}
	if good.forgot != 2 || bad.forgot != 2 {
		t.Fatalf("forgot good=%d bad=%d", good.forgot, bad.forgot)
	}
	var listed []string
	p.List(context.Background(), 0, func(o storage.Object) error { listed = append(listed, o.Name); return nil })
	if len(listed) != 1 || listed[0] != "main" {
		t.Fatalf("listed %v", listed)
	}
}

func TestPoolCanceledRequestNotRetried(t *testing.T) {
	main, extra := &fakeBot{name: "main", doc: newFakeDoc(chunkSize)}, &fakeBot{name: "extra", broken: true}
	p := newPool(main, slog.New(slog.DiscardHandler))
	p.add(extra)
	p.next = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.getChunk(ctx, 1, 0); err == nil {
		t.Fatal("expected the extra bot's error")
	}
	if main.chunks != 0 {
		t.Fatal("a canceled request must not be retried on the main bot")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.benched[1].IsZero() {
		t.Fatal("a canceled request must not pause the bot")
	}
}

func TestPoolFloodHandsOff(t *testing.T) {
	main, extra := &fakeBot{name: "main", flood: 1}, &fakeBot{name: "extra"}
	p := newPool(main, slog.New(slog.DiscardHandler))
	p.add(extra)
	body := []byte("hello part")

	// 主 Bot 被限速：这一块交给额外的 Bot，从头重读数据；主 Bot 歇着，额外的 Bot 不算出错、不停用
	if _, err := p.Put(context.Background(), storage.PutRequest{Body: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if main.puts != 1 || extra.puts != 1 || !bytes.Equal(extra.bodies[0], body) {
		t.Fatalf("puts main=%d extra=%d", main.puts, extra.puts)
	}
	for range 3 {
		if _, err := p.Put(context.Background(), storage.PutRequest{Body: bytes.NewReader(body), Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}
	if main.puts != 1 || extra.puts != 4 {
		t.Fatalf("flood-limited main bot was used again: main=%d extra=%d", main.puts, extra.puts)
	}

	// 额外的 Bot 也被限速了：两个都在歇，最早的要歇 30 秒，等不起就报错（这里用很短的期限）
	extra.flood = 1
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Put(ctx, storage.PutRequest{Body: bytes.NewReader(body), Size: int64(len(body))}); err == nil {
		t.Fatal("expected an error while every bot is flood-limited")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.benched[1].IsZero() {
		t.Fatal("a flood wait must not pause the bot as broken")
	}
}

func TestPoolPicksLeastBusy(t *testing.T) {
	p := newPool(&fakeBot{name: "main"}, slog.New(slog.DiscardHandler))
	p.add(&fakeBot{name: "a"})
	p.add(&fakeBot{name: "b"})
	k0, _, _ := p.pick(storage.LaneDefault)
	k1, _, _ := p.pick(storage.LaneDefault)
	p.done(k0)
	// k0 闲下来了，k1 还忙：下一个要么是没活的第三个，要么是 k0，绝不是 k1
	k2, _, _ := p.pick(storage.LaneDefault)
	if k2 == k1 || k0 == k1 {
		t.Fatalf("picked %d, %d, %d", k0, k1, k2)
	}
	k3, _, _ := p.pick(storage.LaneDefault)
	if k3 == k1 || k3 == k2 {
		t.Fatalf("picked a busy bot: %d after %d, %d", k3, k1, k2)
	}
}

// AI 接口专用的 Bot：AI 的请求只用它（被限速就等它），别的请求不碰它；它坏了 AI 临时用普通的 Bot。
func TestPoolAgentLane(t *testing.T) {
	doc := newFakeDoc(4 * chunkSize)
	doc.delay = 0
	main, extra, ai := &fakeBot{name: "main", doc: doc}, &fakeBot{name: "extra", doc: doc}, &fakeBot{name: "ai", doc: doc}
	p := newPool(main, slog.New(slog.DiscardHandler))
	p.add(extra)
	p.addLane(ai, true)
	agentCtx := storage.WithLane(context.Background(), storage.LaneAgent)

	for range 6 {
		if _, err := p.Put(context.Background(), storage.PutRequest{Body: bytes.NewReader([]byte("abc")), Size: 3}); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		if _, err := p.Put(agentCtx, storage.PutRequest{Body: bytes.NewReader([]byte("abc")), Size: 3}); err != nil {
			t.Fatal(err)
		}
	}
	if ai.puts != 4 || main.puts+extra.puts != 6 {
		t.Fatalf("puts: main %d extra %d ai %d", main.puts, extra.puts, ai.puts)
	}

	// 专用的被限速：AI 的请求不去占普通的 Bot，而是报“都在歇”（等它）
	p.flood(2, time.Hour)
	if k, _, wait := p.pick(storage.LaneAgent); k != -1 || wait < 59*time.Minute {
		t.Fatalf("agent lane should wait for its own bot: k=%d wait=%s", k, wait)
	}
	p.mu.Lock()
	p.flooded[2] = time.Time{}
	p.mu.Unlock()

	// 专用的坏了：AI 临时用普通的
	ai.mu.Lock()
	ai.broken = true
	ai.mu.Unlock()
	rc, err := p.ReadRange(agentCtx, storage.Location{MessageID: 1}, 0, chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	before := main.chunks + extra.chunks
	rc, _ = p.ReadRange(agentCtx, storage.Location{MessageID: 1}, chunkSize, chunkSize)
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if main.chunks+extra.chunks <= before {
		t.Fatal("with its bot benched, the agent lane should use the normal bots")
	}
}

func TestPoolTimeoutBenchesBriefly(t *testing.T) {
	p := newPool(&fakeBot{name: "main"}, slog.New(slog.DiscardHandler))
	p.add(&fakeBot{name: "a"})
	p.add(&fakeBot{name: "b"})

	// Telegram 那边临时超时：只停一小会儿；别的错照旧停 10 分钟
	p.add(&fakeBot{name: "c"})
	p.bench(1, tgerr.New(-503, "Timeout")) // 日志里是 rpc error code -503: Timeout
	p.bench(2, errors.New("CHAT_ADMIN_REQUIRED"))
	p.bench(3, fmt.Errorf("get file: %w", tgerr.New(503, "Timeout")))
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range []int{1, 3} {
		if d := time.Until(p.benched[k]); d > benchBrief {
			t.Fatalf("timeout paused bot %d for %s", k, d)
		}
	}
	if d := time.Until(p.benched[2]); d < benchFor-time.Minute {
		t.Fatalf("other errors should pause for %s, got %s", benchFor, d)
	}
}
