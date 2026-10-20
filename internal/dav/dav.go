// Package dav 以 WebDAV 协议提供云盘，给相册备份 App、电脑的文件管理器挂载用。
// 目录树、文件直接映射到元数据库和存储驱动，上传时边收边加密写进存储端。
//
// 私密文件夹（它自己或上面任何一层设了私密）在 WebDAV 里看不到也进不去：
// 网页上它们要刷脸才显示，WebDAV 只凭一个密码，不能绕过去。
// 删除都是移进回收站，客户端误删了还能在网页上找回来。
package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// Prefix 是 WebDAV 的挂载路径。
const Prefix = "/dav"

// New 返回 WebDAV 处理器（不做认证，由调用方套上）。
func New(svc *service.Service, userID uuid.UUID, log *slog.Logger) http.Handler {
	h := &webdav.Handler{
		Prefix:     Prefix,
		FileSystem: &FS{svc: svc, userID: userID, cache: map[string]cacheEntry{}},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Warn("webdav", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PUT 时记下请求体有没有收完整：读请求体出错时 webdav 包照样会关闭文件，
		// 不检查的话半截文件会被当成传完了存下来（客户端断开时请求的 context 也会取消，这里再保险一层）。
		if r.Method == http.MethodPut {
			b := &body{ReadCloser: r.Body, want: r.ContentLength}
			r.Body = b
			r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, b))
		}
		h.ServeHTTP(w, r)
	})
}

type bodyKey struct{}

// body 记着读了多少、读的时候有没有出错。
type body struct {
	io.ReadCloser
	want int64 // Content-Length，-1 表示不知道
	got  int64
	err  error
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.got += int64(n)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

// incomplete 报告请求体是不是没收完整。
func (b *body) incomplete() error {
	if b.err != nil {
		return fmt.Errorf("request body: %w", b.err)
	}
	if b.want >= 0 && b.got != b.want {
		return fmt.Errorf("request body: got %d of %d bytes", b.got, b.want)
	}
	return nil
}

// cacheTTL 内复用 Readdir 查到的条目：PROPFIND 列目录时会对每个子项再 Stat 一次，
// 不缓存的话一个几千张照片的文件夹要一个个查库。任何写操作都会清空缓存。
const cacheTTL = 15 * time.Second

type cacheEntry struct {
	f   *meta.File
	exp time.Time
}

// FS 实现 webdav.FileSystem。
type FS struct {
	svc    *service.Service
	userID uuid.UUID

	mu    sync.Mutex
	cache map[string]cacheEntry
}

var (
	errIsDir     = errors.New("is a directory")
	errNotDir    = errors.New("not a directory")
	errReadOnly  = errors.New("file opened read-only")
	errWriteOnly = errors.New("file opened write-only")
)

func clean(name string) string { return path.Clean("/" + name) }

func (fsys *FS) cached(p string) *meta.File {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if e, ok := fsys.cache[p]; ok && time.Now().Before(e.exp) {
		return e.f
	}
	return nil
}

func (fsys *FS) remember(p string, f *meta.File) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if len(fsys.cache) > 20000 {
		clear(fsys.cache)
	}
	fsys.cache[p] = cacheEntry{f: f, exp: time.Now().Add(cacheTTL)}
}

func (fsys *FS) invalidate() {
	fsys.mu.Lock()
	clear(fsys.cache)
	fsys.mu.Unlock()
}

// resolve 返回路径对应的条目：根目录返回 (nil, nil)，不存在（或在私密文件夹里）返回 fs.ErrNotExist。
func (fsys *FS) resolve(ctx context.Context, name string) (*meta.File, error) {
	p := clean(name)
	if p == "/" {
		return nil, nil
	}
	if f := fsys.cached(p); f != nil {
		return f, nil
	}
	var cur *meta.File
	for _, seg := range strings.Split(p[1:], "/") {
		var parent *uuid.UUID
		if cur != nil {
			if cur.Type != meta.TypeFolder {
				return nil, fs.ErrNotExist
			}
			parent = &cur.ID
		}
		f, err := fsys.svc.Store().FindChild(ctx, fsys.userID, parent, seg)
		if errors.Is(err, meta.ErrNotFound) {
			return nil, fs.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		if f.Private { // 私密是往下继承的，上面一层私密就整个看不到
			return nil, fs.ErrNotExist
		}
		cur = f
	}
	fsys.remember(p, cur)
	return cur, nil
}

// parentOf 返回路径所在文件夹的 ID（根目录为 nil）和文件名。
func (fsys *FS) parentOf(ctx context.Context, name string) (*uuid.UUID, string, error) {
	p := clean(name)
	if p == "/" {
		return nil, "", fs.ErrPermission
	}
	base := path.Base(p)
	if len(base) > 255 || strings.ContainsAny(base, "\\\x00") {
		return nil, "", fs.ErrInvalid
	}
	dir, err := fsys.resolve(ctx, path.Dir(p))
	if err != nil {
		return nil, "", err
	}
	if dir == nil {
		return nil, base, nil
	}
	if dir.Type != meta.TypeFolder {
		return nil, "", fs.ErrNotExist
	}
	return &dir.ID, base, nil
}

func (fsys *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	f, err := fsys.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return newInfo(f, path.Base(clean(name))), nil
}

func (fsys *FS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	parent, base, err := fsys.parentOf(ctx, name)
	if err != nil {
		return err
	}
	defer fsys.invalidate()
	_, err = fsys.svc.Store().CreateFolder(ctx, fsys.userID, parent, base)
	return mapErr(err)
}

func (fsys *FS) RemoveAll(ctx context.Context, name string) error {
	f, err := fsys.resolve(ctx, name)
	if err != nil {
		return err
	}
	if f == nil {
		return fs.ErrPermission // 不能删根目录
	}
	defer fsys.invalidate()
	return mapErr(fsys.svc.Store().Trash(ctx, fsys.userID, f.ID))
}

func (fsys *FS) Rename(ctx context.Context, oldName, newName string) error {
	src, err := fsys.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	if src == nil {
		return fs.ErrPermission
	}
	parent, base, err := fsys.parentOf(ctx, newName)
	if err != nil {
		return err
	}
	defer fsys.invalidate()
	_, err = fsys.svc.Store().UpdateFile(ctx, fsys.userID, src.ID, meta.FileUpdate{Name: &base, Move: true, ParentID: parent})
	return mapErr(err)
}

func (fsys *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		return fsys.create(ctx, name, flag)
	}
	f, err := fsys.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	info := newInfo(f, path.Base(clean(name)))
	if f == nil || f.Type == meta.TypeFolder {
		d := &dir{fsys: fsys, ctx: ctx, path: clean(name), info: info}
		if f != nil {
			d.id = &f.ID
		}
		return d, nil
	}
	_, rd, err := fsys.svc.Open(ctx, fsys.userID, f.ID)
	if err != nil {
		return nil, mapErr(err)
	}
	return &file{FileReader: rd, info: info}, nil
}

// create 打开一个写入流：数据经管道直接交给 service.Upload 加密上传。覆盖已有文件时
// 先用临时名字上传，传完再把旧文件移进回收站、改回原名，上传失败不会丢掉旧文件。
func (fsys *FS) create(ctx context.Context, name string, flag int) (webdav.File, error) {
	if flag&os.O_TRUNC == 0 {
		return nil, errors.New("only whole-file writes are supported")
	}
	parent, base, err := fsys.parentOf(ctx, name)
	if err != nil {
		return nil, err
	}
	existing, err := fsys.svc.Store().FindChild(ctx, fsys.userID, parent, base)
	switch {
	case errors.Is(err, meta.ErrNotFound):
		existing = nil
	case err != nil:
		return nil, err
	case existing.Type == meta.TypeFolder:
		return nil, errIsDir
	}
	uploadName := base
	if existing != nil {
		uploadName = fmt.Sprintf("%s.uploading-%s", base, uuid.NewString()[:8])
	}
	ctype := mime.TypeByExtension(path.Ext(base))
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	pr, pw := io.Pipe()
	w := &writer{
		fsys: fsys, ctx: ctx, parent: parent, name: base, existing: existing, pw: pw,
		done: make(chan uploadResult, 1), started: time.Now(), ctype: ctype,
	}
	go func() {
		f, err := fsys.svc.Upload(ctx, service.UploadRequest{
			UserID: fsys.userID, ParentID: parent, Name: uploadName, Mime: ctype, Body: pr,
		})
		pr.CloseWithError(err) // 上传提前失败时让 Write 马上返回错误
		w.done <- uploadResult{f, err}
	}()
	return w, nil
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, meta.ErrNotFound):
		return fs.ErrNotExist
	case errors.Is(err, meta.ErrConflict):
		return fs.ErrExist
	case errors.Is(err, meta.ErrMoveIntoSelf):
		return fs.ErrPermission
	default:
		return err
	}
}

// ---------- 文件信息 ----------

type info struct {
	f     *meta.File // nil 表示根目录
	name  string
	size  int64
	mod   time.Time
	ctype string
}

func newInfo(f *meta.File, name string) *info {
	if f == nil {
		return &info{name: "/"}
	}
	mod := f.UpdatedAt
	if f.TakenAt != nil { // 照片、视频用拍摄时间，备份 App 和文件管理器按时间排序时和手机上一致
		mod = *f.TakenAt
	}
	return &info{f: f, name: name, size: f.Size, mod: mod, ctype: f.Mime}
}

func (i *info) Name() string       { return i.name }
func (i *info) Size() int64        { return i.size }
func (i *info) ModTime() time.Time { return i.mod }
func (i *info) IsDir() bool        { return i.f == nil || i.f.Type == meta.TypeFolder }
func (i *info) Sys() any           { return nil }
func (i *info) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// ContentType 直接用数据库里的 MIME，免得 PROPFIND 时为了猜类型去下载文件。
func (i *info) ContentType(context.Context) (string, error) {
	if i.ctype != "" {
		return i.ctype, nil
	}
	if t := mime.TypeByExtension(path.Ext(i.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

func (i *info) ETag(context.Context) (string, error) {
	if i.f == nil || i.f.ID == uuid.Nil { // 根目录、正在上传的文件：用默认的（修改时间 + 大小）
		return "", webdav.ErrNotImplemented
	}
	return fmt.Sprintf(`"%s-%x"`, i.f.ID.String()[:8], i.f.UpdatedAt.UnixNano()), nil
}

// ---------- 打开的文件、文件夹 ----------

type file struct {
	*service.FileReader
	info *info
}

func (f *file) Readdir(int) ([]fs.FileInfo, error) { return nil, errNotDir }
func (f *file) Stat() (fs.FileInfo, error)         { return f.info, nil }
func (f *file) Write([]byte) (int, error)          { return 0, errReadOnly }

type dir struct {
	fsys *FS
	ctx  context.Context
	path string
	id   *uuid.UUID
	info *info

	items []fs.FileInfo
	read  bool
	pos   int
}

func (d *dir) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.read {
		children, err := d.fsys.svc.Store().ListChildren(d.ctx, d.fsys.userID, d.id)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, c := range children {
			if c.Private {
				continue
			}
			d.fsys.remember(path.Join(d.path, c.Name), c)
			d.items = append(d.items, newInfo(c, c.Name))
		}
		d.read = true
	}
	rest := d.items[d.pos:]
	if count <= 0 {
		d.pos = len(d.items)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(rest))
	d.pos += n
	return rest[:n], nil
}

func (d *dir) Stat() (fs.FileInfo, error)     { return d.info, nil }
func (d *dir) Read([]byte) (int, error)       { return 0, errIsDir }
func (d *dir) Write([]byte) (int, error)      { return 0, errIsDir }
func (d *dir) Seek(int64, int) (int64, error) { return 0, nil }
func (d *dir) Close() error                   { return nil }

type uploadResult struct {
	f   *meta.File
	err error
}

type writer struct {
	fsys     *FS
	ctx      context.Context
	parent   *uuid.UUID
	name     string
	existing *meta.File
	pw       *io.PipeWriter
	done     chan uploadResult
	started  time.Time
	ctype    string
	size     int64
	closed   bool
	result   uploadResult
}

func (w *writer) Write(p []byte) (int, error) {
	n, err := w.pw.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *writer) Close() error {
	if w.closed {
		return w.result.err
	}
	w.closed = true
	if b, _ := w.ctx.Value(bodyKey{}).(*body); b != nil {
		if err := b.incomplete(); err != nil {
			w.pw.CloseWithError(err) // 上传失败，已经写上去的分块由 Upload 清理
		}
	}
	w.pw.Close()
	w.result = <-w.done
	defer w.fsys.invalidate()
	if w.result.err != nil {
		return mapErr(w.result.err)
	}
	if w.existing != nil {
		store := w.fsys.svc.Store()
		if err := store.Trash(w.ctx, w.fsys.userID, w.existing.ID); err != nil && !errors.Is(err, meta.ErrNotFound) {
			w.result.err = err
			return err
		}
		if _, err := store.UpdateFile(w.ctx, w.fsys.userID, w.result.f.ID, meta.FileUpdate{Name: &w.name}); err != nil {
			w.result.err = err
			return mapErr(err)
		}
	}
	return nil
}

func (w *writer) Stat() (fs.FileInfo, error) {
	return &info{f: &meta.File{Type: meta.TypeFile}, name: w.name, size: w.size, mod: w.started, ctype: w.ctype}, nil
}
func (w *writer) Read([]byte) (int, error)           { return 0, errWriteOnly }
func (w *writer) Seek(int64, int) (int64, error)     { return 0, errWriteOnly }
func (w *writer) Readdir(int) ([]fs.FileInfo, error) { return nil, errWriteOnly }
