package service

// 断点续传：网页把大文件分成几 MB 一段，一段一个请求传上来（PUT /api/uploads/{id}?offset=…）。
// 收到的数据加密后先攒在临时文件里，攒满一个分块（partSize）就写进存储端，同时把进度和整个文件 SHA-256 的
// 中间状态记进数据库。网络断了：网页问一下服务端收到了多少，从那里接着传（攒在临时文件里的也算）。
// 服务重启了：临时文件没了，从最后一个写好的分块接着传。

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// ResumableChunk 是建议网页每个请求传多少：断网时最多重传这么多。
const ResumableChunk = 8 << 20

// resumableIdle：这么久没收到这个上传的请求，就把攒在内存、临时文件里的状态丢掉（下次从写好的分块接着传）。
const resumableIdle = time.Hour

type ResumableRequest struct {
	UserID   uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Mime     string
	Size     int64
	// Probe 是文件开头、结尾各 1MB 的 SHA-256（网页算）：再选同名同大小的文件时，确认是同一个才接着传。
	Probe string
	// SkipSameContent 同 UploadRequest：传完时网盘里已经有内容一样的就不存，返回 *SameContentError。
	SkipSameContent bool
}

// UploadProgress 是断点续传的进度。
type UploadProgress struct {
	ID     uuid.UUID  `json:"id"`
	Size   int64      `json:"size"`
	Offset int64      `json:"offset"`         // 已经收到的字节数：下一段从这里开始
	Chunk  int64      `json:"chunk"`          // 建议每段多大
	File   *meta.File `json:"file,omitempty"` // 传完了
}

// OffsetError：请求的起点和服务端已经收到的对不上（比如上一段其实收到了、只是回复丢了），从 Offset 接着传。
type OffsetError struct{ Offset int64 }

func (e *OffsetError) Error() string {
	return fmt.Sprintf("upload offset mismatch: continue from %d", e.Offset)
}

var (
	ErrEmptyUpload  = errors.New("resumable uploads need a size above zero")
	ErrBodyTooLong  = errors.New("the request carries more bytes than the file size")
	errBodyStopped  = errors.New("request body stopped") // 客户端断开、太久没数据或被新请求接手：收到的都记着
	errUploadClosed = errors.New("upload was cleaned up")
)

// resumable 是一个正在传的文件在内存里的状态。同一时间只处理一个请求：新请求来了，让旧请求马上停止读数据。
type resumable struct {
	lock chan struct{} // 容量 1：放进去才能处理

	interruptMu sync.Mutex
	interrupt   func() // 正在处理的请求：调用它让那个请求停止读数据

	offset     atomic.Int64 // 已经收到的字节数（写进存储端的 + 攒在 pending 里的），查进度不用等锁
	lastActive atomic.Int64 // 最后一次处理请求的时间（Unix 秒）

	// 以下只在拿到 lock 时访问
	loaded    bool
	closed    bool
	file      *meta.File
	sess      *meta.UploadSession
	dek       []byte
	committed int64
	nextPart  int
	hash      hash.Hash // 整个文件明文的 SHA-256，读到 offset 为止
	pending   *pendingPart
}

// pendingPart 是正在攒的分块：密文写在临时文件里。
type pendingPart struct {
	tmp    *os.File
	size   int64
	iv     []byte
	stream cipher.Stream
	sum    hash.Hash // 密文的 SHA-256
}

func (p *pendingPart) remove() {
	p.tmp.Close()
	os.Remove(p.tmp.Name())
}

func (u *resumable) acquire(ctx context.Context) error {
	u.interruptMu.Lock()
	if u.interrupt != nil {
		u.interrupt()
	}
	u.interruptMu.Unlock()
	select {
	case u.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *resumable) release() { <-u.lock }

func (u *resumable) setInterrupt(f func()) {
	u.interruptMu.Lock()
	u.interrupt = f
	u.interruptMu.Unlock()
}

// reset 丢掉攒着的分块和内存里的进度，下次从数据库里写好的分块接着传（拿着 lock 时调用）。
func (u *resumable) reset() {
	if u.pending != nil {
		u.pending.remove()
		u.pending = nil
	}
	u.loaded = false
	u.offset.Store(-1) // 不知道：查进度时看数据库
}

func (s *Service) resumableFor(id uuid.UUID) *resumable {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	u := s.uploads[id]
	if u == nil {
		u = &resumable{lock: make(chan struct{}, 1)}
		u.offset.Store(-1)
		s.uploads[id] = u
	}
	return u
}

// forgetResumable 丢掉一个上传在内存里的状态（拿着它的 lock 时调用）。
func (s *Service) forgetResumable(id uuid.UUID, u *resumable) {
	u.reset()
	u.closed = true
	s.uploadsMu.Lock()
	if s.uploads[id] == u {
		delete(s.uploads, id)
	}
	s.uploadsMu.Unlock()
}

// evictIdleResumables 丢掉很久没动静的上传在内存里的状态和临时文件（数据库里的进度还在，回来时从写好的分块接着传）；
// only 不为 nil 时只看这些（清理掉的上传）。正在处理请求的跳过。
func (s *Service) evictIdleResumables(idle time.Duration, only map[uuid.UUID]bool) {
	s.uploadsMu.Lock()
	var victims []uuid.UUID
	for id, u := range s.uploads {
		if only != nil && only[id] || only == nil && time.Since(time.Unix(u.lastActive.Load(), 0)) > idle {
			victims = append(victims, id)
		}
	}
	s.uploadsMu.Unlock()
	for _, id := range victims {
		u := s.resumableFor(id)
		select {
		case u.lock <- struct{}{}:
			s.forgetResumable(id, u)
			u.release()
		default:
		}
	}
}

// StartUpload 开始一个断点续传的上传。同一个文件夹里有同名、同大小、Probe 也一样的未完成上传时，返回它的进度
// （网页刷新后重新选了同一个文件）；Probe 不一样的旧上传作废。
func (s *Service) StartUpload(ctx context.Context, req ResumableRequest) (*UploadProgress, error) {
	if req.Size <= 0 {
		return nil, ErrEmptyUpload
	}
	sess, f, err := s.store.FindUploadSession(ctx, req.UserID, req.ParentID, req.Name, req.Size)
	switch {
	case err == nil && sess.Probe == req.Probe:
		return s.UploadStatus(ctx, req.UserID, f.ID)
	case err == nil:
		s.log.Info("resumable upload replaced: same name and size, different content", "file", f.ID, "name", f.Name)
		if err := s.discardUpload(ctx, f.ID); err != nil {
			return nil, err
		}
		s.evictIdleResumables(0, map[uuid.UUID]bool{f.ID: true})
	case !errors.Is(err, meta.ErrNotFound):
		return nil, err
	}

	dek, err := crypto.NewKey()
	if err != nil {
		return nil, err
	}
	wrapped, err := crypto.WrapKey(s.masterKey, dek)
	if err != nil {
		return nil, err
	}
	f, err = s.store.CreateUpload(ctx, &meta.File{
		UserID: req.UserID, ParentID: req.ParentID, Name: req.Name, Mime: req.Mime, DEKWrapped: wrapped,
	})
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateUploadSession(ctx, &meta.UploadSession{
		FileID: f.ID, Size: req.Size, Probe: req.Probe, Dedupe: req.SkipSameContent,
	}); err != nil {
		s.store.DeleteRow(context.WithoutCancel(ctx), f.ID)
		return nil, err
	}
	s.log.Info("resumable upload started", "file", f.ID, "name", f.Name, "size", req.Size)
	return &UploadProgress{ID: f.ID, Size: req.Size, Chunk: ResumableChunk}, nil
}

// UploadStatus 返回一个未完成上传的进度。
func (s *Service) UploadStatus(ctx context.Context, userID, id uuid.UUID) (*UploadProgress, error) {
	sess, _, err := s.store.GetUploadSession(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	offset := sess.Committed
	s.uploadsMu.Lock()
	if u := s.uploads[id]; u != nil && u.offset.Load() >= 0 {
		offset = u.offset.Load()
	}
	s.uploadsMu.Unlock()
	return &UploadProgress{ID: id, Size: sess.Size, Offset: offset, Chunk: ResumableChunk}, nil
}

// WriteUpload 收一段数据：body 是文件里从 offset 开始的字节（最多到文件末尾）。interrupt 让这个请求马上停止读数据，
// 同一个上传的新请求来了时调用（旧请求可能还挂着：手机网络断了，服务端要等一会儿才知道）。
//
// 返回收完这段后的进度；文件传完了 File 不为 nil。offset 对不上返回 *OffsetError；传完时网盘里已经有
// 内容一样的文件返回 *SameContentError（这个上传删掉）；客户端中途断开不算错，进度里是实际收到的。
func (s *Service) WriteUpload(ctx context.Context, userID, id uuid.UUID, offset int64, body io.Reader, interrupt func()) (*UploadProgress, error) {
	u := s.resumableFor(id)
	if err := u.acquire(ctx); err != nil {
		return nil, err
	}
	defer u.release()
	u.setInterrupt(interrupt)
	defer u.setInterrupt(nil)
	u.lastActive.Store(time.Now().Unix())
	if u.closed {
		return nil, meta.ErrNotFound
	}
	if !u.loaded {
		if err := s.loadResumable(ctx, userID, id, u); err != nil {
			s.forgetResumable(id, u)
			return nil, err
		}
	}
	if u.file.UserID != userID {
		return nil, meta.ErrNotFound
	}
	if offset != u.offset.Load() {
		return nil, &OffsetError{Offset: u.offset.Load()}
	}
	if err := s.store.TouchUploadSession(ctx, id); err != nil {
		return nil, err
	}
	err := s.receive(ctx, u, body)
	switch {
	case errors.Is(err, errBodyStopped), errors.Is(err, ErrBodyTooLong):
	case err != nil:
		// 写存储端、数据库出错：攒着的这块不要了，退回写好的分块（网页会从那里重传）
		s.log.Warn("resumable upload: write failed; client will resend from the last stored part", "file", id, "err", err)
		u.reset()
		return nil, err
	}
	p := &UploadProgress{ID: id, Size: u.sess.Size, Offset: u.offset.Load(), Chunk: ResumableChunk}
	if errors.Is(err, ErrBodyTooLong) {
		return p, err
	}
	if p.Offset < p.Size {
		return p, nil
	}
	f, err := s.finishResumable(context.WithoutCancel(ctx), u)
	var same *SameContentError
	if err != nil && !errors.As(err, &same) {
		return nil, err // 比如同名文件刚刚出现了（ErrConflict）：留着，下次再试
	}
	s.forgetResumable(id, u)
	if err != nil {
		return nil, err
	}
	p.File = f
	return p, nil
}

// loadResumable 从数据库读出上传的进度（服务重启后、或丢掉了内存里的状态后）。
func (s *Service) loadResumable(ctx context.Context, userID, id uuid.UUID, u *resumable) error {
	sess, f, err := s.store.GetUploadSession(ctx, userID, id)
	if err != nil {
		return err
	}
	dek, err := crypto.UnwrapKey(s.masterKey, f.DEKWrapped)
	if err != nil {
		return fmt.Errorf("unwrap file key: %w", err)
	}
	parts, err := s.store.ListParts(ctx, id)
	if err != nil {
		return err
	}
	var stored int64
	for _, p := range parts {
		stored += p.Size
	}
	if stored != sess.Committed {
		return fmt.Errorf("upload %s: parts add up to %d bytes, session says %d", id, stored, sess.Committed)
	}
	h := sha256.New()
	if sess.Committed > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(sess.HashState); err != nil {
			return fmt.Errorf("restore hash state: %w", err)
		}
	}
	u.file, u.sess, u.dek, u.committed, u.nextPart, u.hash = f, sess, dek, sess.Committed, len(parts), h
	u.offset.Store(sess.Committed)
	u.loaded = true
	return nil
}

// receive 把 body 加密写进攒着的分块，攒满就写存储端。body 读出错（客户端断开等）返回 errBodyStopped，收到的都算数。
func (s *Service) receive(ctx context.Context, u *resumable, body io.Reader) error {
	buf := make([]byte, 256<<10)
	size := u.sess.Size
	for u.offset.Load() < size {
		if u.pending == nil {
			if err := s.newPending(u); err != nil {
				return err
			}
		}
		want := min(int64(len(buf)), size-u.offset.Load(), s.partSize-u.pending.size)
		n, rerr := body.Read(buf[:want])
		if n > 0 {
			u.hash.Write(buf[:n])
			u.pending.stream.XORKeyStream(buf[:n], buf[:n])
			if _, err := u.pending.tmp.Write(buf[:n]); err != nil {
				return err
			}
			u.pending.sum.Write(buf[:n])
			u.pending.size += int64(n)
			u.offset.Add(int64(n))
		}
		if u.pending.size == s.partSize || u.offset.Load() == size {
			// 客户端走了也要写完：数据已经收到了
			if err := s.commitPending(context.WithoutCancel(ctx), u); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("%w: %v", errBodyStopped, rerr)
		}
	}
	if n, _ := body.Read(buf[:1]); n > 0 {
		return ErrBodyTooLong
	}
	return nil
}

func (s *Service) newPending(u *resumable) error {
	tmp, err := os.CreateTemp(s.tempDir, "xiaoju-resume-*")
	if err != nil {
		return err
	}
	iv, err := crypto.NewIV()
	if err == nil {
		var stream cipher.Stream
		if stream, err = crypto.NewCTRAt(u.dek, iv, 0); err == nil {
			u.pending = &pendingPart{tmp: tmp, iv: iv, stream: stream, sum: sha256.New()}
			return nil
		}
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return err
}

// commitPending 把攒好的分块写进存储端，记下分块和进度。
func (s *Service) commitPending(ctx context.Context, u *resumable) error {
	p := u.pending
	sp := &sealedPart{no: u.nextPart, tmp: p.tmp, size: p.size, iv: p.iv, sum: p.sum.Sum(nil)}
	part, loc, err := s.storePart(ctx, u.file.ID, sp)
	if err != nil {
		return err
	}
	state, err := u.hash.(encoding.BinaryMarshaler).MarshalBinary()
	if err == nil {
		err = s.store.CommitUploadPart(ctx, part, u.offset.Load(), state)
	}
	if err != nil {
		// 记不下来：刚写上去的分块删掉，免得成了没人认领的残块
		s.driver.Delete(ctx, []storage.Location{loc})
		if part.HasBackup() {
			s.backup.Delete(ctx, []storage.Location{backupLocation(part)})
		}
		if errors.Is(err, meta.ErrNotFound) {
			return errUploadClosed
		}
		return err
	}
	p.remove()
	u.pending = nil
	u.committed = u.offset.Load()
	u.nextPart++
	return nil
}

// finishResumable 传完了：按内容去重（要求了的话），激活文件。
func (s *Service) finishResumable(ctx context.Context, u *resumable) (*meta.File, error) {
	sum := u.hash.Sum(nil)
	if u.sess.Dedupe {
		same, err := s.sameContentCheck(ctx, UploadRequest{UserID: u.file.UserID, ParentID: u.file.ParentID, SkipSameContent: true})
		if err != nil {
			return nil, err
		}
		if err := same(u.sess.Size, sum); err != nil {
			if derr := s.discardUpload(ctx, u.file.ID); derr != nil {
				s.log.Warn("resumable upload: discard duplicate", "file", u.file.ID, "err", derr)
			}
			return nil, err
		}
	}
	f, err := s.store.FinishUpload(ctx, u.file.ID, u.sess.Size, sum)
	if err != nil {
		return nil, err
	}
	s.log.Info("resumable upload finished", "file", f.ID, "name", f.Name, "size", f.Size, "sha256", hex.EncodeToString(sum)[:12])
	s.queueThumbnail(f)
	return f, nil
}
