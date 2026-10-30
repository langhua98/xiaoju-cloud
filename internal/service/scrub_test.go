package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// flakyRead 读分块时报一个说不清的错（网络断了）。
type flakyRead struct{ storage.Driver }

func (d flakyRead) ReadRange(context.Context, storage.Location, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("connection reset")
}

// 定期检查：主频道的坏了从备份修，备份坏了重新复制，两份都坏了记下来；读不到（网络问题）不算坏；
// 回收站里放满 30 天的自动永久删除。
func TestScrubAndTrashExpiry(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool := isolatedDB(t, ctx, url)
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	scrubPause, scrubRate = time.Millisecond, 0
	t.Cleanup(func() { scrubPause, scrubRate = 2*time.Second, 4<<20 })
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "scrub-test")
	primaryDir, backupDir := t.TempDir(), t.TempDir()
	primary, _ := local.New(primaryDir)
	backup, _ := local.New(backupDir)
	master, _ := crypto.NewKey()
	opts := Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler), Backup: backup}
	svc := New(store, primary, opts)

	data := randBytes(2500) // 3 块，两边都有
	f, err := svc.Upload(ctx, UploadRequest{UserID: userID, Name: "a.bin", Body: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncBackupOnce(ctx); err != nil { // 上传只写主存储，备份由后台同步补上
		t.Fatal(err)
	}
	parts, _ := store.ListParts(ctx, f.ID)
	corrupt := func(dir string, msgID int64) {
		t.Helper()
		p := filepath.Join(dir, strconv.FormatInt(msgID, 10))
		b, _ := os.ReadFile(p)
		b[10] ^= 0xff
		os.WriteFile(p, b, 0o600)
	}
	due := func() { pool.Exec(ctx, `UPDATE file_parts SET verified_at = NULL`) }
	stats := func() meta.BackupStats {
		st, err := store.BackupStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// 都是好的
	if n, err := svc.ScrubOnce(ctx); err != nil || n != 3 {
		t.Fatalf("scrub: %d %v", n, err)
	}
	if st := stats(); st.Verified != 3 || st.VerifyErrors != 0 {
		t.Fatalf("all good: %+v", st)
	}
	if n, _ := svc.ScrubOnce(ctx); n != 0 {
		t.Fatalf("parts checked again too soon: %d", n)
	}

	// 主频道第 1 块坏了：从备份修好；备份第 2 块坏了：清掉备份记录，同步时重新复制；第 0 块两边都坏了：记下来
	corrupt(primaryDir, parts[1].MessageID)
	corrupt(backupDir, *parts[2].BackupMessageID)
	corrupt(primaryDir, parts[0].MessageID)
	corrupt(backupDir, *parts[0].BackupMessageID)
	due()
	if n, err := svc.ScrubOnce(ctx); err != nil || n != 3 {
		t.Fatalf("scrub damaged: %d %v", n, err)
	}
	after, _ := store.ListParts(ctx, f.ID)
	if after[1].MessageID == parts[1].MessageID {
		t.Fatal("primary part 1 was not repaired from the backup")
	}
	if after[2].HasBackup() {
		t.Fatal("damaged backup of part 2 must be cleared for recopying")
	}
	if st := stats(); st.VerifyErrors != 1 || st.Verified != 2 {
		t.Fatalf("after damage: %+v", st)
	}
	if n, err := svc.SyncBackupOnce(ctx); err != nil || n != 1 {
		t.Fatalf("recopy backup: %d %v", n, err)
	}
	// 第 1、2 块读出来是对的（第 0 块两份都坏了，修不了）
	_, rd, _ := svc.Open(ctx, userID, f.ID)
	rd.Seek(1000, io.SeekStart)
	got, _ := io.ReadAll(rd)
	if !bytes.Equal(got, data[1000:]) {
		t.Fatal("repaired parts read back wrong")
	}

	// 读不到（网络问题）不算坏：这一轮停下，不记结果
	due()
	flaky := New(store, flakyRead{primary}, opts)
	if n, err := flaky.ScrubOnce(ctx); err == nil || n != 0 {
		t.Fatalf("flaky read: %d %v", n, err)
	}
	var unchecked int
	pool.QueryRow(ctx, `SELECT count(*) FROM file_parts WHERE verified_at IS NULL`).Scan(&unchecked)
	if unchecked != 3 {
		t.Fatalf("a failed read must not mark parts as checked: %d unchecked", unchecked)
	}

	// 回收站：放满 30 天的自动永久删除，刚删的留着
	oldF, _ := svc.Upload(ctx, UploadRequest{UserID: userID, Name: "old.bin", Body: bytes.NewReader(randBytes(1500))})
	newF, _ := svc.Upload(ctx, UploadRequest{UserID: userID, Name: "new.bin", Body: bytes.NewReader(randBytes(10))})
	store.Trash(ctx, userID, oldF.ID)
	store.Trash(ctx, userID, newF.ID)
	pool.Exec(ctx, `UPDATE files SET trashed_at = now() - interval '31 days' WHERE id = $1`, oldF.ID)
	oldParts, _ := store.ListParts(ctx, oldF.ID)
	if n, err := svc.PurgeExpiredTrash(ctx); err != nil || n != 1 {
		t.Fatalf("purge expired trash: %d %v", n, err)
	}
	if _, err := store.Get(ctx, userID, oldF.ID); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("expired item still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(primaryDir, strconv.FormatInt(oldParts[0].MessageID, 10))); !os.IsNotExist(err) {
		t.Fatal("expired item's parts must be deleted")
	}
	if _, err := store.Get(ctx, userID, newF.ID); err != nil {
		t.Fatalf("recently trashed item must stay: %v", err)
	}
}
