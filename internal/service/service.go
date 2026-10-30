// Package service 实现云盘核心业务：分块加密上传、解密下载、删除。
package service

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/thumb"
)

type Service struct {
	store     *meta.Store
	driver    storage.Driver
	backup    storage.Driver // 可选：备份存储（另一个账号的频道），每个分块再写一份
	backupErr string         // 配置了备份但连接失败时的原因
	masterKey []byte
	partSize  int64
	tempDir   string
	log       *slog.Logger
	thumbs    *thumbnails // nil 时不生成缩略图
	previews  *previews   // 大图预览的内存缓存（见 previews.go）

	uploadsMu sync.Mutex
	uploads   map[uuid.UUID]*resumable // 断点续传中的上传（见 resumable.go）

	backupKick chan struct{} // 有新分块要备份：叫醒后台备份同步（见 RunBackupSync）
}

type Options struct {
	MasterKey []byte
	PartSize  int64
	TempDir   string
	Log       *slog.Logger
	// Backup 不为 nil 时启用双备份：上传时同时写入，主存储读取失败时改读备份。
	Backup storage.Driver
	// BackupError 记录备份已配置但无法连接的原因，用于状态接口展示。
	BackupError string
	// Thumbs 不为 nil 时给照片、视频生成缩略图（上传后在后台生成，没生成过的在第一次请求时生成）。
	Thumbs *thumb.Generator
}

func New(store *meta.Store, driver storage.Driver, opt Options) *Service {
	s := &Service{
		store:     store,
		driver:    driver,
		backup:    opt.Backup,
		backupErr: opt.BackupError,
		masterKey: opt.MasterKey,
		partSize:  opt.PartSize,
		tempDir:   opt.TempDir,
		log:       opt.Log,
		uploads:   map[uuid.UUID]*resumable{},
		previews:  newPreviews(),

		backupKick: make(chan struct{}, 1),
	}
	if opt.Thumbs != nil {
		s.thumbs = newThumbnails(opt.Thumbs)
	}
	return s
}

func (s *Service) Store() *meta.Store { return s.store }

type UploadRequest struct {
	UserID   uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Mime     string
	Body     io.Reader
	// Spool 为 true 时先把整个请求体读完、加密存进临时文件，再写存储端，
	// 读完时调用 OnSpooled（批量上传接口在这之后给客户端发心跳，免得客户端等回复超时）。
	// 临时目录需要能放下整个文件的密文。
	Spool     bool
	OnSpooled func()
	// SkipSameContent 为 true 时，网盘里已经有内容完全相同的文件（见 meta.SameContent）就不再存，
	// 返回 *SameContentError。要读完整个文件才知道摘要：Spool 时，或文件只有一个分块（照片都是）时，
	// 在写存储端之前就能发现；分块多的大文件已经写上去的分块会删掉。
	SkipSameContent bool
}

// SameContentError 表示网盘里已经有内容完全相同的文件 File，这次上传没有存。
type SameContentError struct{ File *meta.File }

func (e *SameContentError) Error() string {
	return "a file with the same content is already in the drive: " + e.File.Name
}

// Upload 把 Body 按 partSize 切块：每块先加密写入临时文件（便于失败重试、长度已知），
// 再交给存储驱动上传，最后记录分块映射并激活文件。失败时清理已上传的分块。
func (s *Service) Upload(ctx context.Context, req UploadRequest) (_ *meta.File, err error) {
	dek, err := crypto.NewKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := crypto.WrapKey(s.masterKey, dek)
	if err != nil {
		return nil, err
	}
	f, err := s.store.CreateUpload(ctx, &meta.File{
		UserID: req.UserID, ParentID: req.ParentID, Name: req.Name, Mime: req.Mime, DEKWrapped: wrapped,
	})
	if err != nil {
		return nil, err
	}

	var uploaded, uploadedBackup []storage.Location
	defer func() {
		if err == nil {
			return
		}
		// 用独立 context 清理，避免请求被取消后无法清理
		cleanup := context.WithoutCancel(ctx)
		if len(uploaded) > 0 {
			if derr := s.driver.Delete(cleanup, uploaded); derr != nil {
				s.log.Error("cleanup uploaded parts", "file", f.ID, "err", derr)
			}
		}
		if len(uploadedBackup) > 0 {
			if derr := s.backup.Delete(cleanup, uploadedBackup); derr != nil {
				s.log.Error("cleanup uploaded backup parts", "file", f.ID, "err", derr)
			}
		}
		if derr := s.store.DeleteRow(cleanup, f.ID); derr != nil {
			s.log.Error("cleanup file row", "file", f.ID, "err", derr)
		}
	}()

	store := func(sp *sealedPart) error {
		part, loc, err := s.storePart(ctx, f.ID, sp)
		if err != nil {
			return err
		}
		uploaded = append(uploaded, loc)
		if part.HasBackup() {
			uploadedBackup = append(uploadedBackup, backupLocation(part))
		}
		return s.store.AddPart(ctx, part)
	}

	fileHash := sha256.New()
	body := io.TeeReader(req.Body, fileHash)
	var total int64
	same, err := s.sameContentCheck(ctx, req)
	if err != nil {
		return nil, err
	}
	checked := false
	check := func() error {
		checked = true
		return same(total, fileHash.Sum(nil))
	}
	// Spool（快捷指令上传）边收边写：前面的分块一收好就交给后台写进存储端，接着收下一块；最后一块等整个文件收完、
	// 按内容查过、开始发心跳（OnSpooled）后再写。以前是整个文件收完才开始写：2.8GB 的视频收了 47 分钟，又写了 12 分钟，
	// 临时目录还得放下整个文件。
	var bg *partWriter
	if req.Spool {
		bg = newPartWriter(store)
		defer bg.wait() // 比上面的清理先执行：清理要等后台写完
	}
	var lastPart *sealedPart
	defer func() {
		if lastPart != nil {
			lastPart.remove()
		}
	}()
	for partNo := 0; ; partNo++ {
		sp, perr := s.sealPart(partNo, dek, body)
		if perr != nil {
			return nil, perr
		}
		if sp == nil {
			break // 输入已读完
		}
		total += sp.size
		last := sp.size < s.partSize
		if last { // 整个文件读完了：先看网盘里有没有一样的，再写最后一块
			if err := check(); err != nil {
				sp.remove()
				return nil, err
			}
		}
		switch {
		case bg != nil && last:
			lastPart = sp
		case bg != nil:
			if err := bg.add(sp); err != nil {
				return nil, err
			}
		default:
			err := store(sp)
			sp.remove()
			if err != nil {
				return nil, err
			}
		}
		if last {
			break
		}
	}
	if !checked {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if bg != nil {
		if req.OnSpooled != nil {
			req.OnSpooled()
		}
		if sp := lastPart; sp != nil {
			lastPart = nil
			if err := bg.add(sp); err != nil {
				return nil, err
			}
		}
		if err := bg.wait(); err != nil {
			return nil, err
		}
	}
	done, err := s.store.ActivateFile(ctx, f.ID, total, fileHash.Sum(nil))
	if err == nil {
		s.queueThumbnail(done)
	}
	return done, err
}

// partWriter 在后台按顺序把分块写进存储端（边收边写，见 Upload）。最多排着一块，存储端慢的时候收数据的一方等着，
// 临时目录里最多同时有三块。写失败后不再写，之后交来的分块直接删掉。
type partWriter struct {
	queue chan *sealedPart
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	err   error
}

func newPartWriter(store func(*sealedPart) error) *partWriter {
	w := &partWriter{queue: make(chan *sealedPart, 1), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for sp := range w.queue {
			if w.failed() == nil {
				if err := store(sp); err != nil {
					w.mu.Lock()
					w.err = err
					w.mu.Unlock()
				}
			}
			sp.remove()
		}
	}()
	return w
}

func (w *partWriter) failed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// add 交给后台写；前面已经写失败了就返回那个错误。
func (w *partWriter) add(sp *sealedPart) error {
	if err := w.failed(); err != nil {
		sp.remove()
		return err
	}
	w.queue <- sp
	return nil
}

// wait 等交来的分块都写完（可以调用多次）。
func (w *partWriter) wait() error {
	w.once.Do(func() { close(w.queue) })
	<-w.done
	return w.failed()
}

// sameContentCheck 返回检查“网盘里有没有内容一样的文件”的函数（读完整个文件后调用，传入大小和摘要）。
// 没要求按内容去重时什么都不查；空文件不算。
func (s *Service) sameContentCheck(ctx context.Context, req UploadRequest) (func(size int64, sum []byte) error, error) {
	if !req.SkipSameContent {
		return func(int64, []byte) error { return nil }, nil
	}
	private, err := s.store.InPrivateFolder(ctx, req.UserID, req.ParentID)
	if err != nil {
		return nil, err
	}
	return func(size int64, sum []byte) error {
		if size == 0 {
			return nil
		}
		found, err := s.store.SameContent(ctx, req.UserID, [][]byte{sum}, private, req.ParentID)
		if err != nil {
			return err
		}
		if f := found[hex.EncodeToString(sum)]; f != nil {
			return &SameContentError{File: f}
		}
		return nil
	}, nil
}

// sealedPart 是加密好、存在临时文件里等着写存储端的一个分块。
type sealedPart struct {
	no   int
	tmp  *os.File
	size int64
	iv   []byte
	sum  []byte // 密文 sha256
}

func (sp *sealedPart) remove() {
	if sp.tmp != nil {
		sp.tmp.Close()
		os.Remove(sp.tmp.Name())
		sp.tmp = nil
	}
}

// sealPart 读取最多 partSize 字节，加密写入临时文件。输入为空时返回 (nil, nil)。
func (s *Service) sealPart(partNo int, dek []byte, body io.Reader) (_ *sealedPart, err error) {
	tmp, err := os.CreateTemp(s.tempDir, "xiaoju-part-*")
	if err != nil {
		return nil, err
	}
	sp := &sealedPart{no: partNo, tmp: tmp}
	defer func() {
		if err != nil || sp.size == 0 {
			sp.remove()
		}
	}()
	if sp.iv, err = crypto.NewIV(); err != nil {
		return nil, err
	}
	stream, err := crypto.NewCTRAt(dek, sp.iv, 0)
	if err != nil {
		return nil, err
	}
	partHash := sha256.New()
	enc := &cipher.StreamWriter{S: stream, W: io.MultiWriter(tmp, partHash)}
	if sp.size, err = io.Copy(enc, io.LimitReader(body, s.partSize)); err != nil {
		return nil, fmt.Errorf("read upload body: %w", err)
	}
	if sp.size == 0 {
		return nil, nil // 空文件没有分块
	}
	sp.sum = partHash.Sum(nil)
	return sp, nil
}

// storePart 把加密好的分块写进主存储。备份端不在这里写：以前两边一起写、等慢的那一个，备份频道的机器人少
// （Telegram 限制每个机器人约 3 秒发一条），每张照片都要等它，上传快不起来。现在写完主存储就算存好，
// 叫醒后台备份同步（RunBackupSync）从主存储复制过去。
func (s *Service) storePart(ctx context.Context, fileID uuid.UUID, sp *sealedPart) (*meta.Part, storage.Location, error) {
	if s.backup != nil {
		defer s.kickBackup()
	}
	return s.storePrimary(ctx, fileID, sp)
}

// kickBackup 叫醒后台备份同步（已经叫过、它还没醒的话什么都不做）。
func (s *Service) kickBackup() {
	select {
	case s.backupKick <- struct{}{}:
	default:
	}
}

// storePrimary 把加密好的分块写进主存储（失败了按 putRetryDelays 再试）。
func (s *Service) storePrimary(ctx context.Context, fileID uuid.UUID, sp *sealedPart) (*meta.Part, storage.Location, error) {
	req := func() storage.PutRequest {
		return storage.PutRequest{
			Name:    partName(fileID, sp.no),
			Caption: caption(fileID, sp.no, sp.sum),
			Body:    io.NewSectionReader(sp.tmp, 0, sp.size), // 各读各的
			Size:    sp.size,
		}
	}
	loc, err := s.putPart(ctx, fileID, sp, req)
	if err != nil {
		return nil, storage.Location{}, err
	}
	return &meta.Part{
		FileID: fileID, PartNo: sp.no, Storage: s.driver.Name(),
		ChannelID: loc.ChannelID, MessageID: loc.MessageID,
		Size: sp.size, IV: sp.iv, SHA256: sp.sum,
	}, loc, nil
}

// putRetryDelays：分块写主存储失败后，隔多久再试一次（试完这几次还不行才算上传失败）。
// Telegram 的连接偶尔会断一下（“engine forcibly closed”），以前一次就让整个文件失败：4GB 的视频要写十几块，
// 几乎总有一块碰上，快捷指令再从头传也一样。分块存在临时文件里，可以从头再读。
// 等的时间不能太长：等着的时候收数据的一方也停着（见 partWriter），手机那边太久没进展会断开。
var putRetryDelays = []time.Duration{3 * time.Second, 10 * time.Second, 30 * time.Second}

// putPart 把分块写进主存储，失败了按 putRetryDelays 再试；ctx 取消了就不再试。
func (s *Service) putPart(ctx context.Context, fileID uuid.UUID, sp *sealedPart, req func() storage.PutRequest) (storage.Location, error) {
	for attempt := 0; ; attempt++ {
		loc, err := s.driver.Put(ctx, req())
		if err == nil || attempt >= len(putRetryDelays) || ctx.Err() != nil {
			return loc, err
		}
		d := putRetryDelays[attempt]
		s.log.Warn("storage write failed; retrying the part", "file", fileID, "part", sp.no, "attempt", attempt+1, "in", d, "err", err)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return storage.Location{}, err
		}
	}
}

func partName(fileID uuid.UUID, partNo int) string {
	return fmt.Sprintf("%s.part%04d", fileID, partNo)
}

func backupLocation(p *meta.Part) storage.Location {
	return storage.Location{ChannelID: *p.BackupChannelID, MessageID: *p.BackupMessageID}
}

// caption 写入消息说明，数据库丢失时可以扫描频道重建 文件ID→分块 的映射（文件名等仍需数据库）。
func caption(fileID uuid.UUID, partNo int, sum []byte) string {
	b, _ := json.Marshal(map[string]any{"f": fileID, "p": partNo, "sha256": hex.EncodeToString(sum)})
	return string(b)
}

// Open 返回文件的明文读取器。
func (s *Service) Open(ctx context.Context, userID, id uuid.UUID) (*meta.File, *FileReader, error) {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		return nil, nil, meta.ErrNotFound
	}
	dek, err := crypto.UnwrapKey(s.masterKey, f.DEKWrapped)
	if err != nil {
		return nil, nil, fmt.Errorf("unwrap file key: %w", err)
	}
	parts, err := s.store.ListParts(ctx, f.ID)
	if err != nil {
		return nil, nil, err
	}
	r := NewFileReader(ctx, s.driver, dek, parts)
	r.backup = s.backup
	r.log = s.log
	return f, r, nil
}

var ErrFolderPurge = errors.New("move the folder to the trash before deleting it permanently")

// Purge 永久删除文件：先删存储端分块，再删数据库记录。文件夹要先移入回收站，再连同里面所有文件一起删。
func (s *Service) Purge(ctx context.Context, userID, id uuid.UUID) error {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if f.Type != meta.TypeFile {
		if f.TrashedAt == nil {
			return ErrFolderPurge
		}
		return s.purgeFolder(ctx, userID, f)
	}
	return s.purgeFile(ctx, f)
}

// purgeFolder 一个个删掉文件夹里（任意层）的文件，最后删文件夹本身（子文件夹的记录跟着删）。
// 中途出错就停下，已删的不会恢复，再删一次接着删剩下的。
func (s *Service) purgeFolder(ctx context.Context, userID uuid.UUID, folder *meta.File) error {
	ids, err := s.store.FilesUnder(ctx, userID, folder.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		f, err := s.store.Get(ctx, userID, id)
		if errors.Is(err, meta.ErrNotFound) { // 还在上传中的：只删记录
			if err := s.store.DeleteRow(ctx, id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.purgeFile(ctx, f); err != nil {
			return err
		}
	}
	return s.store.DeleteRow(ctx, folder.ID)
}

func (s *Service) purgeFile(ctx context.Context, f *meta.File) error {
	parts, err := s.store.ListParts(ctx, f.ID)
	if err != nil {
		return err
	}
	locs := make([]storage.Location, 0, len(parts))
	var backupLocs []storage.Location
	for i := range parts {
		p := &parts[i]
		locs = append(locs, storage.Location{ChannelID: p.ChannelID, MessageID: p.MessageID})
		if p.HasBackup() {
			backupLocs = append(backupLocs, backupLocation(p))
		}
	}
	if err := s.driver.Delete(ctx, locs); err != nil {
		return err
	}
	s.previews.forget(f.ID)
	// 备份删除失败只记录日志：主存储已删，不能因为备份端的问题让文件“删不掉”
	if s.backup != nil && len(backupLocs) > 0 {
		if err := s.backup.Delete(ctx, backupLocs); err != nil {
			s.log.Warn("delete backup parts failed", "file", f.ID, "err", err)
		}
	}
	return s.store.DeleteRow(ctx, f.ID)
}
