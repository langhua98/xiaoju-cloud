package service

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：需要 PostgreSQL，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestReconcileIntegration(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "reconcile-test")
	primaryDir, backupDir := t.TempDir(), t.TempDir()
	primary, _ := local.New(primaryDir)
	backup, _ := local.New(backupDir)
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(store, primary, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log, Backup: backup})

	old := time.Now().Add(-48 * time.Hour)
	putRaw := func(d *local.Driver, dir, name string, age time.Time) int64 {
		t.Helper()
		loc, err := d.Put(ctx, storage.PutRequest{Name: name, Body: bytes.NewReader([]byte("junk")), Size: 4})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, strconv.FormatInt(loc.MessageID, 10))
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatal(err)
		}
		return loc.MessageID
	}
	exists := func(dir string, id int64) bool {
		_, err := os.Stat(filepath.Join(dir, strconv.FormatInt(id, 10)))
		return err == nil
	}

	// 正常文件
	plains := map[uuid.UUID][]byte{}
	var files []*meta.File
	for i, size := range []int{2500, 800, 0} {
		data := randBytes(size)
		f, err := svc.Upload(ctx, UploadRequest{UserID: userID, Name: "f" + strconv.Itoa(i), Body: bytes.NewReader(data)})
		if err != nil {
			t.Fatal(err)
		}
		plains[f.ID] = data
		files = append(files, f)
	}
	// 上传只写主存储，备份由后台同步补上
	if _, err := svc.SyncBackupOnce(ctx); err != nil {
		t.Fatal(err)
	}
	readAll := func() {
		t.Helper()
		for _, f := range files {
			_, r, err := svc.Open(ctx, userID, f.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(got, plains[f.ID]) {
				t.Fatalf("file %s unreadable after reconcile: %v", f.Name, err)
			}
		}
	}

	// 各种不该删 / 该删的分块
	orphan := putRaw(primary, primaryDir, partName(uuid.New(), 0), old)        // 残块：该删
	backupOrphan := putRaw(backup, backupDir, partName(uuid.New(), 3), old)    // 备份里的残块：该删
	recent := putRaw(primary, primaryDir, partName(uuid.New(), 0), time.Now()) // 太新：保留
	foreign := putRaw(primary, primaryDir, "hello.txt", old)                   // 不是本程序的：保留
	suspicious := putRaw(primary, primaryDir, partName(files[0].ID, 7), old)   // 指向存在的文件：保留

	// 中断的上传：7 小时前开始，传了一块
	stale, err := store.CreateUpload(ctx, &meta.File{UserID: userID, Name: "stale", DEKWrapped: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	staleLoc, _ := primary.Put(ctx, storage.PutRequest{Name: partName(stale.ID, 0), Body: bytes.NewReader([]byte("abcd")), Size: 4})
	if err := store.AddPart(ctx, &meta.Part{FileID: stale.ID, PartNo: 0, Storage: "local", MessageID: staleLoc.MessageID, Size: 4, IV: make([]byte, 16), SHA256: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `UPDATE files SET created_at = now() - interval '7 hours' WHERE id = $1`, stale.ID)

	// 1. 只出报告：什么都不动
	rep, err := svc.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StaleUploads != 1 || len(rep.Lost) != 0 || rep.Primary.Unseen != 0 ||
		len(rep.Primary.Orphans) != 1 || rep.Primary.Orphans[0].MessageID != orphan ||
		rep.Primary.SkippedRecent != 1 || rep.Primary.SkippedForeign != 1 ||
		len(rep.Primary.KeptSuspicious) != 1 || rep.Primary.KeptSuspicious[0].MessageID != suspicious ||
		len(rep.Backup.Orphans) != 1 || rep.Backup.Orphans[0].MessageID != backupOrphan {
		t.Fatalf("dry run report: %+v primary=%+v backup=%+v", rep, rep.Primary, rep.Backup)
	}
	if !exists(primaryDir, orphan) || !exists(backupDir, backupOrphan) || !exists(primaryDir, staleLoc.MessageID) {
		t.Fatal("dry run must not delete anything")
	}

	// 2. 执行：删残块、清理中断上传，其余保留，所有文件仍可读
	if rep, err = svc.Reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	if rep.StaleUploads != 1 || rep.Primary.Deleted != 1 || rep.Backup.Deleted != 1 {
		t.Fatalf("apply report: %+v primary=%+v backup=%+v", rep, rep.Primary, rep.Backup)
	}
	if exists(primaryDir, orphan) || exists(backupDir, backupOrphan) || exists(primaryDir, staleLoc.MessageID) {
		t.Fatal("orphans / stale upload parts should be deleted")
	}
	for _, id := range []int64{recent, foreign, suspicious} {
		if !exists(primaryDir, id) {
			t.Fatalf("message %d must be kept", id)
		}
	}
	if ok, _ := store.FileExists(ctx, stale.ID); ok {
		t.Fatal("stale upload row should be removed")
	}
	readAll()

	// 3. 主存储丢了一块：先报告且不删残块；执行后从备份复制回来
	parts, _ := store.ListParts(ctx, files[0].ID)
	os.Remove(filepath.Join(primaryDir, strconv.FormatInt(parts[1].MessageID, 10)))
	orphan2 := putRaw(primary, primaryDir, partName(uuid.New(), 0), old)
	if rep, err = svc.Reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(rep.Primary.Missing) != 1 || !rep.Primary.Missing[0].Repaired || rep.Primary.Deleted != 0 || rep.Primary.DeleteSkipped == "" {
		t.Fatalf("repair primary: %+v", rep.Primary)
	}
	if !exists(primaryDir, orphan2) {
		t.Fatal("orphans must not be deleted while recorded parts are missing")
	}
	readAll()
	// 修复后再跑一次，残块才删除
	if rep, err = svc.Reconcile(ctx, true); err != nil || rep.Primary.Deleted != 1 || rep.Primary.Unseen != 0 {
		t.Fatalf("second apply: %v %+v", err, rep.Primary)
	}

	// 4. 备份丢了一块：清除备份记录，同步后补齐
	parts, _ = store.ListParts(ctx, files[1].ID)
	os.Remove(filepath.Join(backupDir, strconv.FormatInt(*parts[0].BackupMessageID, 10)))
	if rep, err = svc.Reconcile(ctx, true); err != nil || len(rep.Backup.Missing) != 1 || !rep.Backup.Missing[0].Repaired {
		t.Fatalf("repair backup: %v %+v", err, rep.Backup)
	}
	if n, err := svc.SyncBackupOnce(ctx); err != nil || n != 1 {
		t.Fatalf("resync: %d %v", n, err)
	}
	if rep, err = svc.Reconcile(ctx, false); err != nil || rep.Backup.Unseen != 0 || len(rep.Backup.Missing) != 0 {
		t.Fatalf("after resync: %v %+v", err, rep.Backup)
	}

	// 5. 两边都丢：报告为丢失，不做任何删除
	parts, _ = store.ListParts(ctx, files[1].ID)
	os.Remove(filepath.Join(primaryDir, strconv.FormatInt(parts[0].MessageID, 10)))
	os.Remove(filepath.Join(backupDir, strconv.FormatInt(*parts[0].BackupMessageID, 10)))
	orphan3 := putRaw(primary, primaryDir, partName(uuid.New(), 0), old)
	if rep, err = svc.Reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(rep.Lost) != 1 || rep.Primary.Deleted != 0 || rep.Backup.Unseen != 1 || !exists(primaryDir, orphan3) {
		t.Fatalf("lost part: %+v primary=%+v backup=%+v", rep, rep.Primary, rep.Backup)
	}
}
