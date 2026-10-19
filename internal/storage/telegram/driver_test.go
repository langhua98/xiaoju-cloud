package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestNormalizeChannelID(t *testing.T) {
	cases := map[int64]int64{
		-1001234567890: 1234567890,
		1234567890:     1234567890,
		-1234567:       1234567,
	}
	for in, want := range cases {
		if got := NormalizeChannelID(in); got != want {
			t.Errorf("NormalizeChannelID(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestAlignChunk(t *testing.T) {
	cases := []struct{ off, aligned, skip int64 }{
		{0, 0, 0},
		{1, 0, 1},
		{chunkSize - 1, 0, chunkSize - 1},
		{chunkSize, chunkSize, 0},
		{3*chunkSize + 4097, 3 * chunkSize, 4097},
	}
	for _, c := range cases {
		a, s := alignChunk(c.off)
		if a != c.aligned || s != c.skip {
			t.Errorf("alignChunk(%d) = (%d,%d), want (%d,%d)", c.off, a, s, c.aligned, c.skip)
		}
		if a%chunkSize != 0 {
			t.Errorf("aligned offset %d not 1MB aligned", a)
		}
	}
}

func TestExtractMessageID(t *testing.T) {
	upd := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 99}},
		&tg.UpdateMessageID{ID: 42, RandomID: 7},
	}}
	if id, err := extractMessageID(upd, 7); err != nil || id != 42 {
		t.Fatalf("got %d, %v; want 42", id, err)
	}
	// 没有匹配 random_id 时退回到新频道消息
	if id, err := extractMessageID(upd, 8); err != nil || id != 99 {
		t.Fatalf("got %d, %v; want 99", id, err)
	}
	if _, err := extractMessageID(&tg.Updates{}, 1); err == nil {
		t.Fatal("expected error for empty updates")
	}
}

func TestDocumentObject(t *testing.T) {
	msg := &tg.Message{ID: 42, Date: 1700000000, Media: &tg.MessageMediaDocument{Document: &tg.Document{
		Size:       1234,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "abc.part0001"}},
	}}}
	obj, ok := documentObject(7, msg)
	if !ok || obj.MessageID != 42 || obj.ChannelID != 7 || obj.Size != 1234 || obj.Name != "abc.part0001" || obj.Created.Unix() != 1700000000 {
		t.Fatalf("got %+v ok=%v", obj, ok)
	}
	if _, ok := documentObject(7, &tg.Message{ID: 1, Message: "test"}); ok {
		t.Fatal("text message must not be treated as a part")
	}
}

// fakeDoc 模拟频道里的一个文档：按 1MB 对齐返回数据，记录同时在下载的块数。
type fakeDoc struct {
	data     []byte
	delay    time.Duration
	failAt   int64 // 这个偏移的块返回错误（-1 不出错）
	mu       sync.Mutex
	active   int
	peak     int
	requests []int64
}

func (d *fakeDoc) fetch(ctx context.Context, msgID, off int64) ([]byte, error) {
	d.mu.Lock()
	d.active++
	d.peak = max(d.peak, d.active)
	d.requests = append(d.requests, off)
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.active--; d.mu.Unlock() }()
	if off%chunkSize != 0 {
		return nil, fmt.Errorf("unaligned offset %d", off)
	}
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if off == d.failAt {
		return nil, errors.New("boom")
	}
	if off >= int64(len(d.data)) {
		return nil, nil
	}
	return d.data[off:min(off+chunkSize, int64(len(d.data)))], nil
}

func newFakeDoc(size int) *fakeDoc {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + i/chunkSize)
	}
	return &fakeDoc{data: b, delay: 20 * time.Millisecond, failAt: -1}
}

func TestRangeReaderReadsInOrder(t *testing.T) {
	doc := newFakeDoc(5*chunkSize + 12345)
	for _, c := range []struct{ off, n int64 }{
		{0, int64(len(doc.data))}, {1, 10}, {chunkSize - 3, 6}, {2*chunkSize + 77, 3 * chunkSize}, {5 * chunkSize, 12345}, {0, 0},
	} {
		r := newRangeReader(context.Background(), doc.fetch, 1, c.off, c.n)
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("range %+v: %v", c, err)
		}
		if !bytes.Equal(got, doc.data[c.off:c.off+c.n]) {
			t.Fatalf("range %+v: wrong data (%d bytes)", c, len(got))
		}
	}
}

func TestRangeReaderFetchesConcurrently(t *testing.T) {
	doc := newFakeDoc(12 * chunkSize)
	r := newRangeReader(context.Background(), doc.fetch, 1, 0, int64(len(doc.data)))
	start := time.Now()
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if doc.peak < 2 || doc.peak > readAhead {
		t.Fatalf("peak concurrent fetches = %d, want 2..%d", doc.peak, readAhead)
	}
	// 一块一块下要 12×20ms；并发下载应该明显更快
	if took > 9*doc.delay {
		t.Fatalf("12 chunks took %s, looks sequential", took)
	}
	if len(doc.requests) != 12 {
		t.Fatalf("fetched %d chunks, want 12", len(doc.requests))
	}
}

func TestRangeReaderSmallReadFetchesOneChunk(t *testing.T) {
	// 视频探测、缩略图只读一小段：只下需要的那一块，不预读后面的
	doc := newFakeDoc(8 * chunkSize)
	r := newRangeReader(context.Background(), doc.fetch, 1, 3*chunkSize+5, 8*chunkSize-(3*chunkSize+5))
	buf := make([]byte, 100)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if !bytes.Equal(buf, doc.data[3*chunkSize+5:3*chunkSize+105]) {
		t.Fatal("wrong data")
	}
	time.Sleep(3 * doc.delay)
	doc.mu.Lock()
	defer doc.mu.Unlock()
	// 收到第一块后窗口变成 2，最多再多开两块
	if len(doc.requests) > 3 {
		t.Fatalf("a 100-byte read fetched %d chunks: %v", len(doc.requests), doc.requests)
	}
	if doc.active != 0 {
		t.Fatalf("%d fetches still running after Close", doc.active)
	}
}

func TestRangeReaderErrors(t *testing.T) {
	doc := newFakeDoc(6 * chunkSize)
	doc.failAt = 2 * chunkSize
	r := newRangeReader(context.Background(), doc.fetch, 1, 0, int64(len(doc.data)))
	n, err := io.Copy(io.Discard, r)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("got %v, want boom", err)
	}
	if n != 2*chunkSize {
		t.Fatalf("read %d bytes before the error, want %d", n, 2*chunkSize)
	}

	// 文档比记录的短：中间一块不满 1MB
	short := newFakeDoc(2*chunkSize + 10)
	r = newRangeReader(context.Background(), short.fetch, 1, 0, 4*chunkSize)
	if _, err := io.Copy(io.Discard, r); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short document: got %v, want ErrUnexpectedEOF", err)
	}

	// 请求取消
	ctx, cancel := context.WithCancel(context.Background())
	slow := newFakeDoc(4 * chunkSize)
	slow.delay = time.Hour
	r = newRangeReader(ctx, slow.fetch, 1, 0, 4*chunkSize)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: got %v", err)
	}
}

func TestPaceSkipsFileTransfers(t *testing.T) {
	calls := 0
	inv := pace(50*time.Millisecond, 2).Handle(telegram.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		calls++
		return nil
	}))
	ctx := context.Background()
	start := time.Now()
	for range 20 { // 分片请求不排队
		if err := inv.Invoke(ctx, &tg.UploadGetFileRequest{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > 30*time.Millisecond {
		t.Fatalf("20 getFile calls took %s; file transfers must not be paced", took)
	}
	start = time.Now()
	for range 4 { // 突发 2 个，之后每 50ms 一个
		if err := inv.Invoke(ctx, &tg.ChannelsGetMessagesRequest{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 90*time.Millisecond {
		t.Fatalf("4 getMessages calls took %s; expected pacing", took)
	}
	if calls != 24 {
		t.Fatalf("calls = %d", calls)
	}
}
