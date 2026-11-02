package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/dbdump"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// pinDriver 是能置顶的本地存储（假装是频道）。
type pinDriver struct {
	storage.Driver
	mu      sync.Mutex
	pinned  map[int64]bool
	caption map[int64]string
}

func (p *pinDriver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	loc, err := p.Driver.Put(ctx, req)
	p.mu.Lock()
	p.caption[loc.MessageID] = req.Caption
	p.mu.Unlock()
	return loc, err
}

func (p *pinDriver) Pin(_ context.Context, loc storage.Location, unpin bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if unpin {
		delete(p.pinned, loc.MessageID)
	} else {
		p.pinned[loc.MessageID] = true
	}
	return nil
}

func (p *pinDriver) Pinned(context.Context) (storage.Object, string, error) {
	return storage.Object{}, "", storage.ErrNotFound
}

func (p *pinDriver) pins() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int64
	for id := range p.pinned {
		out = append(out, id)
	}
	return out
}

func newPinDriver(t *testing.T) *pinDriver {
	d, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &pinDriver{Driver: d, pinned: map[int64]bool{}, caption: map[int64]string{}}
}

func TestDBBackupIntegration(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "dbbackup-test")
	primary, backup := newPinDriver(t), newPinDriver(t)
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(store, primary, Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log, Backup: backup})
	svc.EnableDBBackup(DBBackupOptions{UserID: userID, KeepFiles: 2, KeepMessages: 1})

	first, err := svc.BackupDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.FileID == nil || first.MainMessage == nil || first.BackupMessage == nil {
		t.Fatalf("not stored everywhere: %+v", first)
	}
	if c, ok := ParseDBBackupCaption(primary.caption[*first.MainMessage]); !ok || c.Size != first.Size {
		t.Fatalf("caption: %q", primary.caption[*first.MainMessage])
	}
	// 网盘里那份能下载、能解开
	f, rd, err := svc.Open(ctx, userID, *first.FileID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := dbdump.VerifyBackup(rd, master)
	rd.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.Rows["users"] != 1 || f.ParentID == nil {
		t.Fatalf("drive copy: %+v %+v", info, f)
	}
	parent, _ := store.Get(ctx, userID, *f.ParentID)
	if parent.Name != DBBackupFolder || parent.ParentID != nil {
		t.Fatalf("folder: %+v", parent)
	}
	// 频道里那份也能解开（不经过数据库）
	ch, err := primary.ReadRange(ctx, storage.Location{MessageID: *first.MainMessage}, 0, first.Size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbdump.VerifyBackup(ch, master); err != nil {
		t.Fatal("channel copy:", err)
	}
	ch.Close()

	second, err := svc.BackupDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	third, err := svc.BackupDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 只有最新的一条置顶
	for name, d := range map[string]*pinDriver{"main": primary, "backup": backup} {
		want := *third.MainMessage
		if name == "backup" {
			want = *third.BackupMessage
		}
		if p := d.pins(); len(p) != 1 || p[0] != want {
			t.Fatalf("%s pins: %v, want [%d]", name, p, want)
		}
	}
	// 网盘留 2 份、频道留 1 份
	list, _ := store.ListDBBackups(ctx, 10)
	if len(list) != 3 {
		t.Fatalf("records: %d", len(list))
	}
	if list[2].FileID != nil || list[1].FileID == nil || list[1].MainMessage != nil || list[1].BackupMessage != nil {
		t.Fatalf("prune: %+v", list)
	}
	if _, err := store.Get(ctx, userID, *first.FileID); err == nil {
		t.Fatal("oldest drive copy not deleted")
	}
	if r, err := primary.ReadRange(ctx, storage.Location{MessageID: *second.MainMessage}, 0, 1); err == nil {
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("old channel message not deleted")
		}
	}
	st, err := svc.BackupStatus(ctx)
	if err != nil || st.DB == nil || st.DB.Last == nil || st.DB.Last.ID != third.ID {
		t.Fatalf("status: %+v %v", st.DB, err)
	}
}
