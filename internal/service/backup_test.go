package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：需要 PostgreSQL，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestBackupIntegration(t *testing.T) {
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
	userID, err := store.EnsureUser(ctx, "backup-test")
	if err != nil {
		t.Fatal(err)
	}
	primaryDir, backupDir := t.TempDir(), t.TempDir()
	primary, _ := local.New(primaryDir)
	backup, _ := local.New(backupDir)
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log}

	upload := func(svc *Service, name string, data []byte) *meta.File {
		t.Helper()
		f, err := svc.Upload(ctx, UploadRequest{UserID: userID, Name: name, Body: bytes.NewReader(data)})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	read := func(svc *Service, id meta.File) []byte {
		t.Helper()
		_, r, err := svc.Open(ctx, userID, id.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	status := func(svc *Service) BackupStatus {
		t.Helper()
		st, err := svc.BackupStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// 1. 启用备份之前上传的旧文件：没有备份
	plainOld := randBytes(2500)
	noBackup := New(store, primary, opts)
	old := upload(noBackup, "old.bin", plainOld)
	if st := status(noBackup); st.Enabled || st.Parts != 3 || st.BackedUp != 0 {
		t.Fatalf("before backup: %+v", st)
	}

	// 2. 启用备份后上传：只等主存储写完，叫醒后台同步去备份
	optsB := opts
	optsB.Backup = backup
	svc := New(store, primary, optsB)
	plainNew := randBytes(1500)
	fresh := upload(svc, "new.bin", plainNew)
	parts, _ := store.ListParts(ctx, fresh.ID)
	for _, p := range parts {
		if p.HasBackup() {
			t.Fatalf("new upload part %d was backed up inline", p.PartNo)
		}
	}
	select {
	case <-svc.backupKick:
	default:
		t.Fatal("upload did not wake the backup sync")
	}

	// 3. 后台同步补齐旧文件和新文件
	if n, err := svc.SyncBackupOnce(ctx); err != nil || n != 5 {
		t.Fatalf("sync: copied %d, err %v", n, err)
	}
	if st := status(svc); !st.Enabled || st.Parts != 5 || st.BackedUp != 5 || st.BackedBytes != 4000 {
		t.Fatalf("after sync: %+v", st)
	}
	if n, _ := svc.SyncBackupOnce(ctx); n != 0 {
		t.Fatalf("second sync copied %d", n)
	}

	// 4. 主存储的数据全部丢失后，从备份读取
	removeAll(t, primaryDir)
	if got := read(svc, *old); !bytes.Equal(got, plainOld) {
		t.Fatal("old file not readable from backup")
	}
	if got := read(svc, *fresh); !bytes.Equal(got, plainNew) {
		t.Fatal("new file not readable from backup")
	}

	// 5. 主存储数据被篡改时，同步拒绝写入备份
	tampered := upload(noBackup, "tampered.bin", randBytes(500))
	tp, _ := store.ListParts(ctx, tampered.ID)
	path := filepath.Join(primaryDir, strconv.FormatInt(tp[0].MessageID, 10))
	b, _ := os.ReadFile(path)
	b[0] ^= 0xff
	os.WriteFile(path, b, 0o600)
	if n, err := svc.SyncBackupOnce(ctx); err == nil || n != 0 {
		t.Fatalf("tampered sync: copied %d, err %v", n, err)
	}
	if tp, _ = store.ListParts(ctx, tampered.ID); tp[0].HasBackup() {
		t.Fatal("tampered part must not be marked as backed up")
	}

	// 6. 永久删除同时删除备份
	before := countFiles(t, backupDir)
	if err := svc.Purge(ctx, userID, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if after := countFiles(t, backupDir); after != before-len(parts) {
		t.Fatalf("backup files after purge: %d, want %d", after, before-len(parts))
	}

	// 7. 主存储和备份一起写；主存储写到一半失败：上传失败，两边已经写进去的分块都删掉
	before = countFiles(t, backupDir)
	beforePrimary := countFiles(t, primaryDir)
	// 写失败会再试几次（putRetryDelays）：这里主存储一直坏着，不用等
	delays := putRetryDelays
	putRetryDelays = nil
	defer func() { putRetryDelays = delays }()
	flaky := New(store, &failingDriver{Driver: primary, failAt: 2}, optsB)
	if _, err := flaky.Upload(ctx, UploadRequest{UserID: userID, Name: "flaky.bin", Body: bytes.NewReader(randBytes(2500))}); err == nil {
		t.Fatal("upload must fail when the primary storage fails")
	}
	if got := countFiles(t, backupDir); got != before {
		t.Fatalf("backup parts left after a failed upload: %d, want %d", got, before)
	}
	if got := countFiles(t, primaryDir); got != beforePrimary {
		t.Fatalf("primary parts left after a failed upload: %d, want %d", got, beforePrimary)
	}
}

// failingDriver 从第 failAt 次写入起都失败（failFor 大于 0 时只失败这么多次，之后恢复）。
type failingDriver struct {
	storage.Driver
	puts, failAt, failFor int
}

func (d *failingDriver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	if d.puts++; d.puts >= d.failAt && (d.failFor == 0 || d.puts < d.failAt+d.failFor) {
		io.Copy(io.Discard, req.Body)
		return storage.Location{}, errors.New("primary storage unavailable")
	}
	return d.Driver.Put(ctx, req)
}

// isolatedDB 在独立的 schema 里运行测试：备份同步会扫描全库缺少备份的分块，
// 不能和其他并行运行的集成测试共用表。
func isolatedDB(t *testing.T, ctx context.Context, url string) *pgxpool.Pool {
	t.Helper()
	admin, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "test_" + hex.EncodeToString(randBytes(6))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := db.Connect(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func removeAll(t *testing.T, dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// countFiles 统计本地驱动目录里的分块文件数（不含 .name 旁路文件）
func countFiles(t *testing.T, dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.ParseInt(e.Name(), 10, 64); err == nil {
			n++
		}
	}
	return n
}

func TestDriveStatsIntegration(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "stats-test")
	primary, _ := local.New(t.TempDir())
	backup, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	noBackup := New(store, primary, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	withBackup := New(store, primary, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log, Backup: backup})

	put := func(svc *Service, parent *meta.File, name, mime string, size int) {
		t.Helper()
		req := UploadRequest{UserID: userID, Name: name, Mime: mime, Body: bytes.NewReader(randBytes(size))}
		if parent != nil {
			req.ParentID = &parent.ID
		}
		if _, err := svc.Upload(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	album, _ := store.CreateFolder(ctx, userID, nil, "相册")
	trash, _ := store.CreateFolder(ctx, userID, nil, "旧东西")

	put(withBackup, album, "a.jpg", "image/jpeg", 1500)       // 照片（MIME），2 块，已备份
	put(withBackup, album, "IMG_0001.HEIC", "", 300)          // 照片（扩展名）
	if _, err := withBackup.SyncBackupOnce(ctx); err != nil { // 上传只写主存储，后台同步补上备份
		t.Fatal(err)
	}
	put(noBackup, album, "clip.MOV", "application/octet-stream", 2000) // 视频（扩展名），未备份
	put(noBackup, nil, "notes.txt", "text/plain", 10)                  // 其他，未备份
	put(withBackup, nil, "empty.bin", "", 0)                           // 其他，空文件算已备份
	put(withBackup, trash, "gone.jpg", "image/jpeg", 100)              // 在回收站文件夹里，不计入
	if err := store.Trash(ctx, userID, trash.ID); err != nil {
		t.Fatal(err)
	}

	st, err := store.DriveStats(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	want := meta.DriveStats{
		Folders: 1, Files: 5, Photos: 2, Videos: 1, Others: 2,
		Bytes: 1500 + 300 + 2000 + 10, PhotoBytes: 1800, VideoBytes: 2000, BackedUpFiles: 3,
	}
	if st != want {
		t.Fatalf("stats = %+v\nwant    %+v", st, want)
	}
}
