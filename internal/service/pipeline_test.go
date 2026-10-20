package service

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// slowBody 每 step 字节停 delay（手机传得慢）。
type slowBody struct {
	r     io.Reader
	step  int
	delay time.Duration
	left  int
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		time.Sleep(b.delay)
		b.left = b.step
	}
	n, err := b.r.Read(p[:min(len(p), b.left)])
	b.left -= n
	return n, err
}

// slowPut 写一块要 delay，记下写的时间。
type slowPut struct {
	storage.Driver
	delay time.Duration
	mu    sync.Mutex
	puts  []time.Time
}

func (d *slowPut) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	time.Sleep(d.delay)
	d.mu.Lock()
	d.puts = append(d.puts, time.Now())
	d.mu.Unlock()
	return d.Driver.Put(ctx, req)
}

// 边收边写：快捷指令上传（Spool）时前面的分块一收好就写，总时间约等于“收的时间 + 写最后一块”，不再是两者相加；
// 心跳（OnSpooled）在整个文件收完后、最后一块写之前开始；内容不变。
func TestSpoolWritesWhileReceiving(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool := isolatedDB(t, ctx, url)
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "pipeline-test")
	base, _ := local.New(t.TempDir())
	driver := &slowPut{Driver: base, delay: 120 * time.Millisecond}
	master, _ := crypto.NewKey()
	svc := New(store, driver, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})

	data := randBytes(5500) // 6 块
	var spooled time.Time
	start := time.Now()
	f, err := svc.Upload(ctx, UploadRequest{
		UserID: userID, Name: "v.mov", Body: &slowBody{r: bytes.NewReader(data), step: 1000, delay: 120 * time.Millisecond},
		Spool: true, OnSpooled: func() { spooled = time.Now() },
	})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// 先收再写要 6×120 + 6×120 = 1440ms；边收边写约 6×120 + 120 = 840ms
	if took > 1150*time.Millisecond {
		t.Fatalf("upload took %s: parts were not written while receiving", took)
	}
	if len(driver.puts) != 6 || !driver.puts[0].Before(spooled) || driver.puts[5].Before(spooled) {
		t.Fatalf("puts %v, spooled at %v", driver.puts, spooled)
	}
	_, rd, err := svc.Open(ctx, userID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rd)
	if !bytes.Equal(got, data) || f.Size != int64(len(data)) {
		t.Fatalf("content differs: %d bytes", len(got))
	}
	entries, _ := os.ReadDir(svc.tempDir)
	if len(entries) != 0 {
		t.Fatalf("%d temp files left", len(entries))
	}
}

// 分块写主存储偶尔失败（Telegram 连接断了一下）：再试一次就好，不让整个文件失败；一直失败才返回错误。
func TestPutPartRetries(t *testing.T) {
	delays := putRetryDelays
	putRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { putRetryDelays = delays }()
	base, _ := local.New(t.TempDir())
	data := []byte("encrypted part")
	req := func() storage.PutRequest {
		return storage.PutRequest{Name: "p", Body: bytes.NewReader(data), Size: int64(len(data))}
	}
	ctx := context.Background()

	flaky := &failingDriver{Driver: base, failAt: 1, failFor: 2}
	svc := &Service{driver: flaky, log: slog.New(slog.DiscardHandler)}
	loc, err := svc.putPart(ctx, [16]byte{}, &sealedPart{}, req)
	if err != nil || flaky.puts != 3 {
		t.Fatalf("flaky storage: err %v after %d puts, want success on the 3rd", err, flaky.puts)
	}
	rc, _ := base.ReadRange(ctx, loc, 0, int64(len(data)))
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, data) {
		t.Fatalf("stored %q, want %q", got, data)
	}

	broken := &failingDriver{Driver: base, failAt: 1}
	svc.driver = broken
	if _, err := svc.putPart(ctx, [16]byte{}, &sealedPart{}, req); err == nil || broken.puts != 3 {
		t.Fatalf("broken storage: err %v after %d puts, want failure after 3", err, broken.puts)
	}

	// 请求取消了就不再试
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	broken.puts = 0
	if _, err := svc.putPart(cancelled, [16]byte{}, &sealedPart{}, req); err == nil || broken.puts != 1 {
		t.Fatalf("cancelled: err %v after %d puts, want failure after 1", err, broken.puts)
	}
}
