package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
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

// 断点续传（App 大视频、网页）也边收边写：攒满的分块交给后台写，接着收下一块，不再停下来等；内容不变。
func TestResumableWritesWhileReceiving(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "resumable-pipeline-test")
	base, _ := local.New(t.TempDir())
	driver := &slowPut{Driver: base, delay: 120 * time.Millisecond}
	master, _ := crypto.NewKey()
	svc := New(store, driver, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})

	data := randBytes(5500) // 6 块
	p, err := svc.StartUpload(ctx, ResumableRequest{UserID: userID, Name: "v.mov", Mime: "video/quicktime", Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	body := &slowBody{r: bytes.NewReader(data), step: 1000, delay: 120 * time.Millisecond}
	done, err := svc.WriteUpload(ctx, userID, p.ID, 0, false, body, func() {})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// 收满一块停下来写要 6×120 + 6×120 = 1440ms；边收边写约 6×120 + 120 = 840ms
	if took > 1150*time.Millisecond {
		t.Fatalf("upload took %s: parts were not written while receiving", took)
	}
	if done.File == nil || done.File.Size != int64(len(data)) || len(driver.puts) != 6 {
		t.Fatalf("progress %+v after %d puts", done, len(driver.puts))
	}
	_, rd, err := svc.Open(ctx, userID, done.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rd)
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs: %d bytes", len(got))
	}
	entries, _ := os.ReadDir(svc.tempDir)
	if len(entries) != 0 {
		t.Fatalf("%d temp files left", len(entries))
	}
}

// failPart 写某个分块（文件名 .part0001 这样）时失败 times 次。
type failPart struct {
	storage.Driver
	suffix string
	mu     sync.Mutex
	times  int
}

func (d *failPart) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	d.mu.Lock()
	fail := strings.HasSuffix(req.Name, d.suffix) && d.times > 0
	if fail {
		d.times--
	}
	d.mu.Unlock()
	if fail {
		io.Copy(io.Discard, req.Body)
		return storage.Location{}, errors.New("primary storage unavailable")
	}
	return d.Driver.Put(ctx, req)
}

// 后台写的分块没写成：传完时进度里又缺这一块（offset 退回它的起点）；从那里重传，传完内容不变。
func TestResumableBackgroundWriteFails(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	delays := putRetryDelays
	putRetryDelays = nil
	defer func() { putRetryDelays = delays }()
	ctx := context.Background()
	pool := isolatedDB(t, ctx, url)
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "resumable-fail-test")
	base, _ := local.New(t.TempDir())
	driver := &failPart{Driver: base, suffix: ".part0001", times: 1} // 第 2 块第一次写不进去
	master, _ := crypto.NewKey()
	svc := New(store, driver, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})

	data := randBytes(3500)
	p, err := svc.StartUpload(ctx, ResumableRequest{UserID: userID, Name: "v.mov", Mime: "video/quicktime", Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.WriteUpload(ctx, userID, p.ID, 0, false, bytes.NewReader(data), func() {})
	if err != nil || st.File != nil || st.Offset != 1000 || len(st.Missing) != 1 || st.Missing[0] != [2]int64{1000, 2000} {
		t.Fatalf("progress %+v, %v: want part 2 missing", st, err)
	}
	if st, err := svc.UploadStatus(ctx, userID, p.ID); err != nil || st.Offset != 1000 {
		t.Fatalf("status %+v, %v: want offset 1000", st, err)
	}
	done, err := svc.WriteUpload(ctx, userID, p.ID, 1000, false, bytes.NewReader(data[1000:]), func() {})
	if err != nil || done.File == nil {
		t.Fatalf("resend: %+v, %v", done, err)
	}
	_, rd, err := svc.Open(ctx, userID, done.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rd); !bytes.Equal(got, data) || !bytes.Equal(done.File.SHA256, sha(data)) {
		t.Fatalf("content or sha256 differs: %d bytes", len(got))
	}
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// 分段同时传：几条连接从后往前、乱序传不同的段，中间服务重启一次（临时文件没了，写好的分块从存储端读回来算摘要），
// 内容和整个文件的 SHA-256 都对；已经收到的位置返回 409 和下一个缺的位置。
func TestResumableParallel(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "resumable-parallel-test")
	base, _ := local.New(t.TempDir())
	driver := &slowPut{Driver: base, delay: 50 * time.Millisecond}
	master, _ := crypto.NewKey()
	opts := Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	svc := New(store, driver, opts)

	data := randBytes(4300) // 5 块
	p, err := svc.StartUpload(ctx, ResumableRequest{UserID: userID, Name: "v.mov", Mime: "video/quicktime", Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if p.Lanes < 2 || p.Part != 1000 || len(p.Missing) != 1 || p.Missing[0] != [2]int64{0, 4300} {
		t.Fatalf("start: %+v", p)
	}
	put := func(svc *Service, from, to int64) (*UploadProgress, error) {
		return svc.WriteUpload(ctx, userID, p.ID, from, true, bytes.NewReader(data[from:to]), func() {})
	}
	// 后面几块先到（第 4、5 块收齐、写好），第 1 块只收了一半
	var wg sync.WaitGroup
	for _, r := range [][2]int64{{3000, 3500}, {3500, 4300}, {4000, 4300}, {0, 400}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := put(svc, r[0], r[1]); err != nil {
				var off *OffsetError
				if !errors.As(err, &off) { // {4000,4300} 可能已经收到了
					t.Errorf("put %v: %v", r, err)
				}
			}
		}()
	}
	wg.Wait()
	if _, err := put(svc, 100, 200); !errors.As(err, new(*OffsetError)) || err.(*OffsetError).Offset != 400 {
		t.Fatalf("already received: %v, want 409 continue at 400", err)
	}
	st, _ := svc.UploadStatus(ctx, userID, p.ID)
	if st.Offset != 400 || len(st.Missing) != 1 || st.Missing[0] != [2]int64{400, 3000} {
		t.Fatalf("progress %+v", st)
	}
	// 等后台把第 4、5 块写好，然后“重启”：第 1 块收到的那一半没了
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		parts, _ := store.ListParts(ctx, p.ID)
		if len(parts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d parts stored, want 2", len(parts))
		}
	}
	opts.TempDir = t.TempDir() // 重启后临时目录是空的
	svc = New(store, driver, opts)
	st, _ = svc.UploadStatus(ctx, userID, p.ID)
	if st.Offset != 0 || len(st.Missing) != 1 || st.Missing[0] != [2]int64{0, 3000} {
		t.Fatalf("progress after restart %+v", st)
	}
	var last *UploadProgress
	var mu sync.Mutex
	for _, r := range [][2]int64{{2000, 3000}, {1000, 2000}, {0, 1000}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := put(svc, r[0], r[1])
			if err != nil {
				t.Errorf("put %v: %v", r, err)
				return
			}
			mu.Lock()
			if got.File != nil {
				last = got
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if last == nil {
		t.Fatal("no request finished the upload")
	}
	_, rd, err := svc.Open(ctx, userID, last.File.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rd); !bytes.Equal(got, data) || !bytes.Equal(last.File.SHA256, sha(data)) {
		t.Fatalf("content or sha256 differs: %d bytes", len(got))
	}
	entries, _ := os.ReadDir(opts.TempDir)
	if len(entries) != 0 {
		t.Fatalf("%d temp files left", len(entries))
	}
}
