package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

type syncEnv struct {
	t      *testing.T
	ctx    context.Context
	d      *fakeDrive
	s      *Syncer
	svc    *service.Service
	store  *meta.Store
	user   uuid.UUID
	folder uuid.UUID // 网盘这边同步的文件夹
	groot  string    // 谷歌那边同步的文件夹
	link   *meta.GDriveLink
}

func newSyncEnv(t *testing.T) *syncEnv {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	driver, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	user, err := store.EnsureUser(ctx, "gd-"+t.Name()+"-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	d := newFakeDrive()
	t.Cleanup(d.srv.Close)
	s := New(&OAuth{ClientID: "cid", ClientSecret: "sec", Endpoints: d.endpoints()}, svc, user, master, log)
	sealed, _ := s.SealRefresh("rt")
	if err := store.SaveGDriveAccount(ctx, user, "me@example.com", sealed); err != nil {
		t.Fatal(err)
	}
	e := &syncEnv{t: t, ctx: ctx, d: d, s: s, svc: svc, store: store, user: user}
	e.groot = d.mkdir("root", "Shared")
	f, err := store.CreateFolder(ctx, user, nil, "Shared")
	if err != nil {
		t.Fatal(err)
	}
	e.folder = f.ID
	if e.link, err = store.CreateGDriveLink(ctx, user, e.groot, "Shared", f.ID); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *syncEnv) sync() Result {
	e.t.Helper()
	e.s.Kick() // 每次都整个对一遍
	if err := e.s.SyncOnce(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	l, err := e.store.GetGDriveLink(e.ctx, e.user, e.link.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if l.LastError != "" && !l.Paused {
		e.t.Fatalf("sync error: %s", l.LastError)
	}
	e.link = l
	var r Result
	_ = json.Unmarshal(l.LastResult, &r)
	return r
}

// x 按路径找网盘这边的条目（从同步的文件夹起），找不到返回 nil
func (e *syncEnv) x(path string) *meta.File {
	cur := &e.folder
	var f *meta.File
	for _, seg := range strings.Split(path, "/") {
		kids, err := e.store.ListChildren(e.ctx, e.user, cur)
		if err != nil {
			return nil
		}
		f = nil
		for _, k := range kids {
			if k.Name == seg {
				f = k
			}
		}
		if f == nil {
			return nil
		}
		cur = &f.ID
	}
	return f
}

func (e *syncEnv) xData(path string) string {
	f := e.x(path)
	if f == nil {
		e.t.Fatalf("%s missing in xiaoju", path)
	}
	_, rd, err := e.svc.Open(e.ctx, e.user, f.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rd.Close()
	b, _ := io.ReadAll(rd)
	return string(b)
}

func (e *syncEnv) xPut(parent *uuid.UUID, name, data string) *meta.File {
	if parent == nil {
		parent = &e.folder
	}
	f, err := e.svc.Upload(e.ctx, service.UploadRequest{UserID: e.user, ParentID: parent, Name: name, Mime: "text/plain", Body: strings.NewReader(data)})
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *syncEnv) gData(path string) string {
	f := e.d.find(e.groot, path)
	if f == nil {
		e.t.Fatalf("%s missing in google drive", path)
	}
	return string(f.data)
}

func TestSyncBothWays(t *testing.T) {
	e := newSyncEnv(t)
	// 谷歌那边已有的、网盘这边已有的、两边都有的同名文件
	sub := e.d.mkdir(e.groot, "Photos")
	e.d.put(sub, "a.txt", "from google")
	e.d.put(e.groot, "same.txt", "same content")
	e.d.put(e.groot, "diff.txt", "google version")
	doc := e.d.put(e.groot, "Doc", "")
	e.d.edit(doc, func(f *fakeFile) { f.MimeType = "application/vnd.google-apps.document" })
	e.xPut(nil, "local.txt", "from xiaoju")
	e.xPut(nil, "same.txt", "same content")
	e.xPut(nil, "diff.txt", "xiaoju version")

	r := e.sync()
	if got := e.xData("Photos/a.txt"); got != "from google" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := e.gData("local.txt"); got != "from xiaoju" {
		t.Fatalf("local.txt = %q", got)
	}
	if e.x("same (谷歌).txt") != nil || e.d.find(e.groot, "same (小橘).txt") != nil {
		t.Fatal("same content should be paired, not copied")
	}
	if e.xData("diff.txt") != "xiaoju version" || e.xData("diff (谷歌).txt") != "google version" {
		t.Fatal("diff.txt conflict in xiaoju")
	}
	if e.gData("diff.txt") != "google version" || e.gData("diff (小橘).txt") != "xiaoju version" {
		t.Fatal("diff.txt conflict in google")
	}
	if r.Conflicts != 1 || r.Down != 2 || r.Up != 2 || r.Skipped != 1 || e.x("Doc") != nil {
		t.Fatalf("result %+v", r)
	}
	// 第二次什么都不用做
	if r := e.sync(); r.Down+r.Up+r.Folders+r.Renamed+r.Trashed+r.Conflicts != 0 {
		t.Fatalf("second sync did something: %+v", r)
	}

	// 谷歌那边改内容、改名 → 网盘跟着变
	a := e.d.find(sub, "a.txt")
	e.d.write(a.ID, "edited in google")
	e.d.edit(a.ID, func(f *fakeFile) { f.Name = "a2.txt" })
	// 网盘这边改内容（换成新文件）、移动 → 谷歌跟着变
	local := e.x("local.txt")
	photos := e.x("Photos")
	if _, err := e.store.UpdateFile(e.ctx, e.user, local.ID, meta.FileUpdate{Move: true, ParentID: &photos.ID}); err != nil {
		t.Fatal(err)
	}
	r = e.sync()
	if e.xData("Photos/a2.txt") != "edited in google" || e.x("Photos/a.txt") != nil {
		t.Fatal("google rename / edit not applied")
	}
	if e.gData("Photos/local.txt") != "from xiaoju" || e.d.find(e.groot, "local.txt") != nil {
		t.Fatal("xiaoju move not applied")
	}

	// 网盘这边新文件夹里新文件 → 谷歌
	nf, err := e.store.CreateFolder(e.ctx, e.user, &e.folder, "New")
	if err != nil {
		t.Fatal(err)
	}
	e.xPut(&nf.ID, "n.txt", "nested")
	e.sync()
	if e.gData("New/n.txt") != "nested" {
		t.Fatal("new nested folder not uploaded")
	}

	// 网盘里“改内容”是旧的进回收站、传一个同名的新文件：谷歌那边换成新的（旧的进谷歌回收站），不加“(小橘)”
	if err := e.store.Trash(e.ctx, e.user, e.x("same.txt").ID); err != nil {
		t.Fatal(err)
	}
	e.xPut(nil, "same.txt", "xiaoju edit")
	e.sync()
	if e.gData("same.txt") != "xiaoju edit" || e.d.find(e.groot, "same (小橘).txt") != nil {
		t.Fatalf("replaced file not mirrored: %v", e.d.names(e.groot))
	}
	// 两边都改了：两份都留
	e.d.write(e.d.find(e.groot, "same.txt").ID, "google edit")
	if err := e.store.Trash(e.ctx, e.user, e.x("same.txt").ID); err != nil {
		t.Fatal(err)
	}
	e.xPut(nil, "same.txt", "xiaoju edit 2")
	e.sync()
	if e.xData("same.txt") != "xiaoju edit 2" || e.xData("same (谷歌).txt") != "google edit" {
		t.Fatal("both-edited conflict in xiaoju")
	}
	if e.gData("same.txt") != "google edit" || e.gData("same (小橘).txt") != "xiaoju edit 2" {
		t.Fatalf("both-edited conflict in google: %v", e.d.names(e.groot))
	}

	// 删除：谷歌删 → 网盘进回收站；网盘删文件夹 → 谷歌进回收站
	e.d.edit(e.d.find(e.groot, "diff.txt").ID, func(f *fakeFile) { f.Trashed = true })
	if err := e.store.Trash(e.ctx, e.user, nf.ID); err != nil {
		t.Fatal(err)
	}
	r = e.sync()
	if e.x("diff (谷歌).txt") != nil || e.x("diff.txt") == nil {
		t.Fatal("google diff.txt is paired with xiaoju diff (谷歌).txt: only that one should be trashed")
	}
	if e.d.find(e.groot, "New") != nil {
		t.Fatal("New should be trashed in google")
	}
	if r.Trashed != 2 {
		t.Fatalf("trashed = %d, want 2 (folder counts once)", r.Trashed)
	}
	if r := e.sync(); r.Down+r.Up+r.Folders+r.Renamed+r.Trashed+r.Conflicts != 0 {
		t.Fatalf("sync after deletes did something: %+v", r)
	}
}

func TestSyncGoogleEditKeepsOldVersionInTrash(t *testing.T) {
	e := newSyncEnv(t)
	id := e.d.put(e.groot, "notes.txt", "v1")
	e.sync()
	e.d.write(id, "v2")
	e.sync()
	if e.xData("notes.txt") != "v2" {
		t.Fatal("v2 not downloaded")
	}
	trash, _ := e.store.ListTrash(e.ctx, e.user)
	if !slices.ContainsFunc(trash, func(f *meta.File) bool { return f.Name == "notes.txt" }) {
		t.Fatal("old version should be in xiaoju trash")
	}
	if e.x("notes (同步中).txt") != nil {
		t.Fatal("temporary name left behind")
	}
}

func TestSyncMassDeletePauses(t *testing.T) {
	e := newSyncEnv(t)
	for i := 0; i < 30; i++ {
		e.d.put(e.groot, "f"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt", "x")
	}
	e.sync()
	for _, n := range e.d.names(e.groot) {
		e.d.edit(e.d.find(e.groot, n).ID, func(f *fakeFile) { f.Trashed = true })
	}
	e.sync()
	if !e.link.Paused || !strings.Contains(e.link.LastError, "太多") {
		t.Fatalf("expected pause, got paused=%v err=%q", e.link.Paused, e.link.LastError)
	}
	kids, _ := e.store.ListChildren(e.ctx, e.user, &e.folder)
	if len(kids) != 30 {
		t.Fatalf("nothing should be deleted while paused, have %d", len(kids))
	}
	// 网页上确认后继续
	if err := e.store.SetGDriveLinkPaused(e.ctx, e.user, e.link.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	e.s.AllowDeletes(e.link.ID)
	e.sync()
	kids, _ = e.store.ListChildren(e.ctx, e.user, &e.folder)
	if len(kids) != 0 {
		t.Fatalf("after confirming, %d left", len(kids))
	}
}

func TestSyncWithoutSHA256AndRevoked(t *testing.T) {
	e := newSyncEnv(t)
	e.d.noSHA = true
	e.d.put(e.groot, "a.txt", "hello")
	e.xPut(nil, "a.txt", "hello")
	if r := e.sync(); r.Conflicts != 0 || r.Down != 0 {
		t.Fatalf("same content without sha256 should pair by downloading: %+v", r)
	}
	// 授权失效：不动任何文件，记下要重新连接
	sealed, _ := e.s.SealRefresh("revoked")
	_ = e.store.SaveGDriveAccount(e.ctx, e.user, "me@example.com", sealed)
	if err := e.s.SyncOnce(e.ctx); err == nil {
		t.Fatal("expected error")
	}
	acct, _ := e.store.GetGDriveAccount(e.ctx, e.user)
	if acct.Error == "" {
		t.Fatal("account error not recorded")
	}
	if !bytes.Equal([]byte(e.xData("a.txt")), []byte("hello")) {
		t.Fatal("file changed")
	}
}

func TestNames(t *testing.T) {
	if got := tagged("a.jpg", "谷歌"); got != "a (谷歌).jpg" {
		t.Fatal(got)
	}
	if got := tagged(".env", "谷歌"); got != ".env (谷歌)" {
		t.Fatal(got)
	}
	if got := freeName(map[string]bool{"a.jpg": true, "a (2).jpg": true}, "a.jpg"); got != "a (3).jpg" {
		t.Fatal(got)
	}
	if got := LocalName("a/b\\c"); got != "a／b＼c" {
		t.Fatal(got)
	}
}
