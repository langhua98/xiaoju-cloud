package service

// 断点续传：网页、App 把大文件分成一段段传上来（PUT /api/uploads/{id}?offset=…）。
// 文件按 partSize 分成几个分块，每个分块单独加密（自己的 IV），收到的数据按位置加密后写进这个分块的临时文件；
// 一个分块收齐了就交给后台写进存储端，接着收别的。整个文件的 SHA-256 按分块顺序算：前面的分块都写好了，
// 才把这一块算进去，并把中间状态记进数据库（committed 是已经算进摘要的字节数）。
//
// 两种传法：
//   - 按顺序传（老的 App、网页）：每次从“已经收到的连续部分”的末尾接着传，起点对不上返回 409 和该从哪接着传。
//   - 分段同时传（parallel=1）：几条连接各传文件里的一段，起点可以是任何还没收到的位置；进度里的 missing
//     列出还缺哪些字节。跨境网络一条连接跑不满带宽，几条一起传快得多。
//
// 网络断了：问一下服务端收到了多少（攒在临时文件里的也算），从那里接着传。
// 服务重启了：临时文件没了，没写好的分块重传。

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// ResumableLanes 是建议分段同时传时开几条连接。
const ResumableLanes = 4

// speedLogBytes：一次请求收到这么多（或传完了）才记一条速度日志。网页每 8MB 一个请求，每个都记太多了。
const speedLogBytes = 64 << 20

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
	ID     uuid.UUID `json:"id"`
	Size   int64     `json:"size"`
	Offset int64     `json:"offset"` // 从头开始连续收到的字节数：按顺序传的下一段从这里开始
	Chunk  int64     `json:"chunk"`  // 建议每段多大
	// 分段同时传用：还缺的字节 [起点, 终点)，建议开几条连接，分块多大（一段别跨过分块边界）
	Missing [][2]int64 `json:"missing"`
	Lanes   int        `json:"lanes"`
	Part    int64      `json:"part"`
	File    *meta.File `json:"file,omitempty"` // 传完了
}

// OffsetError：请求的起点已经收到了（比如上一段其实收到了、只是回复丢了），从 Offset 接着传。
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

// resumable 是一个正在传的文件在内存里的状态。可以同时处理几个请求（各写文件的不同位置）。
type resumable struct {
	mu         sync.Mutex
	lastActive atomic.Int64 // 最后一次处理请求的时间（Unix 秒）

	// 以下拿着 mu 时访问
	loaded  bool
	closed  bool
	file    *meta.File
	sess    *meta.UploadSession
	dek     []byte
	parts   []*uploadPart // 每个分块一个
	writers map[*partWriterReq]bool
	// 整个文件明文的 SHA-256：算到第 hashed 块之前（hashedBytes 字节）
	hash        hash.Hash
	hashed      int
	hashedBytes int64
	hashing     bool           // 正在往后算（同一时间只有一个）
	storing     sync.WaitGroup // 在后台写的分块
	finishing   chan struct{}  // 正在收尾（等分块写完、算摘要、激活文件）；结束后关掉
	result      *finishResult
}

type finishResult struct {
	file *meta.File
	err  error
}

// uploadPart 是一个分块：还在收（tmp、got），在后台写，或者已经写好（stored）。
type uploadPart struct {
	no    int
	start int64 // 在文件里的起点
	size  int64
	tmp   *os.File
	iv    []byte
	got   spans // 收到了分块里的哪些字节
	busy  bool  // 在后台写
	part  *meta.Part
}

func (p *uploadPart) complete() bool { return p.part != nil || p.got.covers(0, p.size) }

// drop 丢掉还没写好的数据（拿着 mu 时调用）。
func (p *uploadPart) drop() {
	if p.tmp != nil {
		p.tmp.Close()
		os.Remove(p.tmp.Name())
		p.tmp = nil
	}
	p.got = nil
}

// partWriterReq 是一个正在写数据的请求：从 start 写到了 pos。stop 让它马上停止读数据。
type partWriterReq struct {
	start, pos int64
	stop       func()
}

// spans 是排好序、互不相连的 [起点, 终点) 区间。
type spans [][2]int64

func (s *spans) add(a, b int64) {
	if a >= b {
		return
	}
	out := (*s)[:0:0]
	for _, x := range *s {
		if x[1] < a || x[0] > b {
			out = append(out, x)
			continue
		}
		a, b = min(a, x[0]), max(b, x[1])
	}
	out = append(out, [2]int64{a, b})
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	*s = out
}

func (s spans) covers(a, b int64) bool {
	for _, x := range s {
		if x[0] <= a && b <= x[1] {
			return true
		}
	}
	return a >= b
}

// next 返回 off 之后第一个没收到的位置（都收到了是 end）。
func (s spans) next(off, end int64) int64 {
	for _, x := range s {
		if x[0] <= off && off < x[1] {
			off = x[1]
		}
	}
	return min(off, end)
}

func (s *Service) resumableFor(id uuid.UUID) *resumable {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()
	u := s.uploads[id]
	if u == nil {
		u = &resumable{writers: map[*partWriterReq]bool{}}
		s.uploads[id] = u
	}
	return u
}

// reset 丢掉攒着的分块和内存里的进度，下次从数据库里写好的分块接着传（拿着 mu 时调用，中间会放开等后台写完）。
func (u *resumable) reset() {
	u.mu.Unlock()
	u.storing.Wait() // 在后台写的分块写好了会记进数据库，下次读进度时要算上
	u.mu.Lock()
	for _, p := range u.parts {
		p.drop()
	}
	u.parts = nil
	u.loaded = false
}

// forgetResumable 丢掉一个上传在内存里的状态（拿着 mu 时调用）。
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
		u.mu.Lock()
		if len(u.writers) == 0 && u.finishing == nil {
			s.forgetResumable(id, u)
		}
		u.mu.Unlock()
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
	return s.progress(f.ID, req.Size, nil, nil), nil
}

// progress 算进度。u 为 nil 时只看数据库里写好的分块（stored，按分块编号）。
func (s *Service) progress(id uuid.UUID, size int64, u *resumable, stored map[int]bool) *UploadProgress {
	p := &UploadProgress{ID: id, Size: size, Chunk: ResumableChunk, Lanes: ResumableLanes, Part: s.partSize, Missing: [][2]int64{}}
	contiguous := true
	for no, start := 0, int64(0); start < size; no, start = no+1, start+s.partSize {
		end := min(start+s.partSize, size)
		var got spans
		switch {
		case u != nil && u.parts[no].part != nil, u == nil && stored[no]:
			got = spans{{0, end - start}}
		case u != nil:
			got = u.parts[no].got
		}
		for off := start; off < end; {
			from := got.next(off-start, end-start) + start
			if from > off && contiguous {
				p.Offset = from
			}
			if from >= end {
				break
			}
			contiguous = false
			to := end
			for _, x := range got {
				if x[0]+start > from {
					to = x[0] + start
					break
				}
			}
			if n := len(p.Missing); n > 0 && p.Missing[n-1][1] == from { // 接着上一段（跨过分块边界）
				p.Missing[n-1][1] = to
			} else {
				p.Missing = append(p.Missing, [2]int64{from, to})
			}
			off = to
		}
	}
	return p
}

// UploadStatus 返回一个未完成上传的进度。
func (s *Service) UploadStatus(ctx context.Context, userID, id uuid.UUID) (*UploadProgress, error) {
	sess, f, err := s.store.GetUploadSession(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	s.uploadsMu.Lock()
	u := s.uploads[id]
	s.uploadsMu.Unlock()
	if u != nil {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.loaded && !u.closed && u.file.UserID == f.UserID {
			return s.progress(id, sess.Size, u, nil), nil
		}
	}
	parts, err := s.store.ListParts(ctx, id)
	if err != nil {
		return nil, err
	}
	stored := map[int]bool{}
	for _, p := range parts {
		stored[p.PartNo] = true
	}
	return s.progress(id, sess.Size, nil, stored), nil
}

// WriteUpload 收一段数据：body 是文件里从 offset 开始的字节（最多到文件末尾）。interrupt 让这个请求马上停止读数据，
// 同一段的新请求来了时调用（旧请求可能还挂着：手机网络断了，服务端要等一会儿才知道）。
// parallel 为 false 时 offset 必须是从头连续收到的末尾（按顺序传）；为 true 时可以是任何还没收到的位置。
//
// 返回收完这段后的进度；文件传完了 File 不为 nil。offset 已经收到了返回 *OffsetError；传完时网盘里已经有
// 内容一样的文件返回 *SameContentError（这个上传删掉）；客户端中途断开不算错，进度里是实际收到的。
func (s *Service) WriteUpload(ctx context.Context, userID, id uuid.UUID, offset int64, parallel bool, body io.Reader, interrupt func()) (*UploadProgress, error) {
	u := s.resumableFor(id)
	u.mu.Lock()
	locked := true
	defer func() {
		if locked {
			u.mu.Unlock()
		}
	}()
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
	size := u.sess.Size
	cur := s.progress(id, size, u, nil)
	if !parallel && offset != cur.Offset || offset > size || offset < size && s.received(u, offset) {
		next := cur.Offset
		if parallel {
			next = s.nextMissing(u, offset)
		}
		return nil, &OffsetError{Offset: next}
	}
	if err := s.store.TouchUploadSession(ctx, id); err != nil {
		return nil, err
	}
	// 同一段的旧请求（按顺序传时是所有旧请求）：让它停下，这个请求接手
	for w := range u.writers {
		if !parallel || w.start <= offset && offset <= w.pos {
			w.stop()
			delete(u.writers, w)
		}
	}
	w := &partWriterReq{start: offset, pos: offset, stop: interrupt}
	u.writers[w] = true
	u.mu.Unlock()
	locked = false

	began := time.Now()
	err := s.receive(ctx, u, w, body)

	u.mu.Lock()
	locked = true
	delete(u.writers, w)
	got := w.pos - offset
	if u.closed { // 别的请求传完了（或上传被清理了）：这段多余
		return nil, meta.ErrNotFound
	}
	if err != nil && !errors.Is(err, errBodyStopped) && !errors.Is(err, ErrBodyTooLong) {
		// 写临时文件、数据库出错：攒着的不要了，退回写好的分块（客户端会从那里重传）
		s.log.Warn("resumable upload: write failed; client will resend from the last stored part", "file", id, "err", err)
		u.reset()
		return nil, err
	}
	p := s.progress(id, size, u, nil)
	if errors.Is(err, ErrBodyTooLong) {
		return p, err
	}
	var waited time.Duration
	if len(p.Missing) == 0 {
		// 都收到了：等分块写完、算完摘要、激活文件（几个请求同时到这里时一起等同一个结果）
		wait := time.Now()
		f, ferr := s.finish(ctx, id, u)
		waited = time.Since(wait)
		if ferr == nil {
			p.File = f
		}
		err = ferr
		if f == nil && ferr == nil { // 有分块没写成：重传缺的
			p = s.progress(id, size, u, nil)
		}
	}
	if got >= speedLogBytes || len(p.Missing) == 0 {
		// 这次请求收了多少、多快；waited：传完后等写 Telegram、收尾等了多久
		took := time.Since(began) - waited
		s.log.Info("resumable upload received", "file", id, "bytes", got, "took", took.Round(100*time.Millisecond),
			"mb_per_s", fmt.Sprintf("%.2f", float64(got)/(1<<20)/max(took.Seconds(), 0.001)),
			"waited", waited.Round(100*time.Millisecond), "parallel", parallel)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// received：offset 这个字节收到了没有（拿着 mu 时调用）。
func (s *Service) received(u *resumable, offset int64) bool {
	p := u.parts[offset/s.partSize]
	return p.part != nil || p.got.covers(offset-p.start, offset-p.start+1)
}

// nextMissing 返回 offset 之后第一个没收到的位置（拿着 mu 时调用）。
func (s *Service) nextMissing(u *resumable, offset int64) int64 {
	for _, m := range s.progress(u.file.ID, u.sess.Size, u, nil).Missing {
		if m[1] > offset {
			return max(m[0], offset)
		}
	}
	return u.sess.Size
}

// loadResumable 从数据库读出上传的进度（服务重启后、或丢掉了内存里的状态后；拿着 mu 时调用）。
func (s *Service) loadResumable(ctx context.Context, userID, id uuid.UUID, u *resumable) error {
	sess, f, err := s.store.GetUploadSession(ctx, userID, id)
	if err != nil {
		return err
	}
	dek, err := crypto.UnwrapKey(s.masterKey, f.DEKWrapped)
	if err != nil {
		return fmt.Errorf("unwrap file key: %w", err)
	}
	stored, err := s.store.ListParts(ctx, id)
	if err != nil {
		return err
	}
	var parts []*uploadPart
	for no, start := 0, int64(0); start < sess.Size; no, start = no+1, start+s.partSize {
		parts = append(parts, &uploadPart{no: no, start: start, size: min(s.partSize, sess.Size-start)})
	}
	for i := range stored {
		p := &stored[i]
		if p.PartNo >= len(parts) || p.Size != parts[p.PartNo].size {
			return fmt.Errorf("upload %s: stored part %d has %d bytes, does not fit a %d-byte file", id, p.PartNo, p.Size, sess.Size)
		}
		parts[p.PartNo].part = p
	}
	// 算进摘要的是前面连续写好的几块
	hashed, hashedBytes := 0, int64(0)
	for hashed < len(parts) && hashedBytes < sess.Committed && parts[hashed].part != nil {
		hashedBytes += parts[hashed].size
		hashed++
	}
	if hashedBytes != sess.Committed {
		return fmt.Errorf("upload %s: stored parts add up to %d bytes, session says %d were hashed", id, hashedBytes, sess.Committed)
	}
	h := sha256.New()
	if sess.Committed > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(sess.HashState); err != nil {
			return fmt.Errorf("restore hash state: %w", err)
		}
	}
	u.file, u.sess, u.dek, u.parts = f, sess, dek, parts
	u.hash, u.hashed, u.hashedBytes = h, hashed, hashedBytes
	u.loaded = true
	return nil
}

// receive 把 body 从 w.pos 开始按位置加密写进分块的临时文件，一个分块收齐了就交给后台写存储端。
// 已经收到的字节跳过（几条连接传到了一起）。body 读出错（客户端断开等）返回 errBodyStopped，收到的都算数。
func (s *Service) receive(ctx context.Context, u *resumable, w *partWriterReq, body io.Reader) error {
	buf := make([]byte, 256<<10)
	u.mu.Lock()
	size, dek := u.sess.Size, u.dek
	u.mu.Unlock()
	for w.pos < size {
		no := int(w.pos / s.partSize)
		u.mu.Lock()
		if !u.loaded || no >= len(u.parts) {
			u.mu.Unlock()
			return errUploadClosed
		}
		p := u.parts[no]
		want := min(int64(len(buf)), p.start+p.size-w.pos)
		u.mu.Unlock()

		n, rerr := body.Read(buf[:want])
		if n > 0 {
			if err := s.writeAt(ctx, u, p, dek, w.pos, buf[:n]); err != nil {
				return err
			}
			u.mu.Lock()
			w.pos += int64(n)
			u.mu.Unlock()
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

// writeAt 把文件里 [off, off+len(data)) 的明文加密写进分块 p 的临时文件，收齐了交给后台写。
func (s *Service) writeAt(ctx context.Context, u *resumable, p *uploadPart, dek []byte, off int64, data []byte) error {
	rel := off - p.start
	u.mu.Lock()
	if !u.loaded || p.no >= len(u.parts) || u.parts[p.no] != p { // 进度被丢掉、重新加载了
		u.mu.Unlock()
		return errUploadClosed
	}
	if p.part != nil || p.busy || p.got.covers(rel, rel+int64(len(data))) {
		u.mu.Unlock()
		return nil // 已经收到了
	}
	if p.tmp == nil {
		tmp, err := os.CreateTemp(s.tempDir, "xiaoju-resume-*")
		if err != nil {
			u.mu.Unlock()
			return err
		}
		iv, err := crypto.NewIV()
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			u.mu.Unlock()
			return err
		}
		p.tmp, p.iv = tmp, iv
	}
	tmp, iv := p.tmp, p.iv
	u.mu.Unlock()

	stream, err := crypto.NewCTRAt(dek, iv, rel)
	if err != nil {
		return err
	}
	enc := make([]byte, len(data))
	stream.XORKeyStream(enc, data)
	_, werr := tmp.WriteAt(enc, rel)

	u.mu.Lock()
	defer u.mu.Unlock()
	if p.tmp != tmp { // 这期间分块被丢掉、重来了：这段不算
		return nil
	}
	if werr != nil {
		return werr
	}
	p.got.add(rel, rel+int64(len(data)))
	if p.got.covers(0, p.size) && !p.busy {
		p.busy = true
		u.storing.Add(1)
		go s.storeResumablePart(context.WithoutCancel(ctx), u, p)
	}
	return nil
}

// storeResumablePart 在后台把收齐的分块写进存储端、记进数据库，然后接着算整个文件的摘要。
// 没写成：这块收到的都丢掉，客户端重传（进度里又缺这一块了）。
func (s *Service) storeResumablePart(ctx context.Context, u *resumable, p *uploadPart) {
	defer u.storing.Done()
	u.mu.Lock()
	fileID, tmp, iv := u.file.ID, p.tmp, p.iv
	u.mu.Unlock()

	part, err := s.storeTemp(ctx, fileID, p.no, tmp, iv, p.size)
	u.mu.Lock()
	p.busy = false
	if err != nil {
		s.log.Warn("resumable upload: part not stored; client will resend it", "file", fileID, "part", p.no, "err", err)
		p.drop()
		u.mu.Unlock()
		return
	}
	p.part = part
	u.mu.Unlock()
	s.advanceHash(ctx, u)
}

// storeTemp 把临时文件里加密好的分块写进存储端（主、备份），记进数据库。
func (s *Service) storeTemp(ctx context.Context, fileID uuid.UUID, no int, tmp *os.File, iv []byte, size int64) (*meta.Part, error) {
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(tmp, 0, size)); err != nil {
		return nil, err
	}
	sp := &sealedPart{no: no, tmp: tmp, size: size, iv: iv, sum: sum.Sum(nil)}
	part, loc, err := s.storePart(ctx, fileID, sp)
	if err != nil {
		return nil, err
	}
	if err := s.store.AddUploadPart(ctx, part); err != nil {
		// 记不下来：刚写上去的分块删掉，免得成了没人认领的残块
		s.driver.Delete(ctx, []storage.Location{loc})
		if part.HasBackup() {
			s.backup.Delete(ctx, []storage.Location{backupLocation(part)})
		}
		if errors.Is(err, meta.ErrNotFound) {
			return nil, errUploadClosed
		}
		return nil, err
	}
	return part, nil
}

// advanceHash 把前面连续写好的分块按顺序算进整个文件的摘要，记下中间状态，删掉它们的临时文件。
// 临时文件没了的（服务重启前写好、还没算的）从存储端读回来算。同一时间只有一个在算。
func (s *Service) advanceHash(ctx context.Context, u *resumable) error {
	u.mu.Lock()
	if u.hashing || !u.loaded {
		u.mu.Unlock()
		return nil
	}
	u.hashing = true
	stop := func(err error) error { // 拿着 mu 时调用
		u.hashing = false
		u.mu.Unlock()
		return err
	}
	for {
		if u.hash == nil {
			return stop(errors.New("whole-file hash was lost"))
		}
		if u.hashed >= len(u.parts) || u.parts[u.hashed].part == nil {
			return stop(nil)
		}
		p, h, dek, fileID := u.parts[u.hashed], u.hash, u.dek, u.file.ID
		tmp := p.tmp
		u.mu.Unlock()

		var src io.Reader
		if tmp != nil {
			stream, err := crypto.NewCTRAt(dek, p.iv, 0)
			if err != nil {
				u.mu.Lock()
				return stop(err)
			}
			src = &cipherReader{r: io.NewSectionReader(tmp, 0, p.size), s: stream}
		} else {
			r := NewFileReader(ctx, s.driver, dek, []meta.Part{*p.part})
			r.backup, r.log = s.backup, s.log
			src = r
		}
		n, err := io.Copy(h, src)
		if err == nil && n != p.size {
			err = fmt.Errorf("part %d: hashed %d bytes, want %d", p.no, n, p.size)
		}
		if err != nil {
			// 摘要算坏了：从数据库里的中间状态重来（下次加载时）
			s.log.Error("resumable upload: hash part", "file", fileID, "part", p.no, "err", err)
			u.mu.Lock()
			u.hash = nil
			return stop(err)
		}
		state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
		if err == nil {
			err = s.store.SetUploadHash(ctx, fileID, u.hashedBytes+p.size, state)
		}
		u.mu.Lock()
		if err != nil {
			u.hash = nil
			if errors.Is(err, meta.ErrNotFound) {
				err = errUploadClosed
			}
			return stop(err)
		}
		u.hashed++
		u.hashedBytes += p.size
		if p.tmp == tmp {
			p.drop()
		}
	}
}

// cipherReader 边读边解密（CTR 加密、解密是同一个操作）。
type cipherReader struct {
	r io.Reader
	s interface{ XORKeyStream(dst, src []byte) }
}

func (c *cipherReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.s.XORKeyStream(b[:n], b[:n])
	return n, err
}

// finish 都收到了：等分块写完、摘要算完，按内容去重（要求了的话），激活文件。几个请求同时调用时等同一个结果。
// 有分块没写成返回 (nil, nil)：客户端看进度重传。拿着 mu 时调用，中间会放开。
func (s *Service) finish(ctx context.Context, id uuid.UUID, u *resumable) (*meta.File, error) {
	if u.finishing != nil {
		done := u.finishing
		u.mu.Unlock()
		<-done
		u.mu.Lock()
		if r := u.result; r != nil {
			return r.file, r.err
		}
		return nil, nil
	}
	done := make(chan struct{})
	u.finishing = done
	u.result = nil
	u.mu.Unlock()
	ctx = context.WithoutCancel(ctx) // 客户端走了也要收完尾：数据已经收到了
	u.storing.Wait()
	herr := s.advanceHash(ctx, u)
	u.mu.Lock()

	var f *meta.File
	var err error
	complete := true
	for _, p := range u.parts {
		complete = complete && p.part != nil
	}
	switch {
	case !complete: // 有分块没写成
	case herr != nil || u.hash == nil || u.hashed != len(u.parts):
		err = fmt.Errorf("upload %s: whole-file hash not complete: %v", id, herr)
		u.reset()
	default:
		f, err = s.finishResumable(ctx, u, u.hash.Sum(nil))
		var same *SameContentError
		if err == nil || errors.As(err, &same) {
			s.forgetResumable(id, u)
		}
	}
	if complete {
		u.result = &finishResult{file: f, err: err}
	}
	u.finishing = nil
	close(done)
	return f, err
}

// finishResumable 传完了：按内容去重（要求了的话），激活文件（拿着 mu 时调用）。
func (s *Service) finishResumable(ctx context.Context, u *resumable, sum []byte) (*meta.File, error) {
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
	// 同名的文件在这个上传开始之后才传完（手机上几个不同的视频同名、同时在传）：改叫“名字 (2)”…再激活。
	// 以前一直用原来的名字重试，永远冲突，手机上那个视频一直“保存中”
	for i := 2; errors.Is(err, meta.ErrConflict) && i < 100; i++ {
		name := copyName(u.file.Name, i)
		if err = s.store.RenameUpload(ctx, u.file.ID, name); err == nil {
			f, err = s.store.FinishUpload(ctx, u.file.ID, u.sess.Size, sum)
		}
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("resumable upload finished", "file", f.ID, "name", f.Name, "size", f.Size, "sha256", hex.EncodeToString(sum)[:12])
	s.queueThumbnail(f)
	return f, nil
}

// copyName 给名字加序号：copyName("IMG_1.mov", 2) == "IMG_1 (2).mov"。
func copyName(name string, i int) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + " (" + strconv.Itoa(i) + ")" + ext
}
