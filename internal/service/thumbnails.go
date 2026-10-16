package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/thumb"
)

// ErrNoThumbnail：这个文件没有缩略图（不是照片、视频，或者格式不支持、文件损坏）。
var ErrNoThumbnail = thumb.ErrNoThumbnail

const (
	thumbWorkers = 3   // 同时生成几张（要从 Telegram 拉原图、解码，比较占 CPU 和内存）
	thumbQueue   = 512 // 上传后排队等后台生成的上限，满了就等第一次查看时再生成
)

// thumbnails 生成、缓存缩略图。同一个文件同时只生成一次；生成好的加密存进数据库。
type thumbnails struct {
	gen     *thumb.Generator
	slots   chan struct{}
	mu      sync.Mutex
	pending map[uuid.UUID]*thumbCall
	failed  sync.Map // 做不了缩略图的文件 ID（只记在内存里，部署新版本、重启后再试一次）
	queue   chan *meta.File
	once    sync.Once
}

type thumbCall struct {
	done chan struct{}
	jpeg []byte
	err  error
}

func newThumbnails(gen *thumb.Generator) *thumbnails {
	return &thumbnails{
		gen: gen, slots: make(chan struct{}, thumbWorkers),
		pending: make(map[uuid.UUID]*thumbCall), queue: make(chan *meta.File, thumbQueue),
	}
}

// CanThumbnail：这个文件应该有缩略图（列目录时据此给出缩略图地址）。
func (s *Service) CanThumbnail(f *meta.File) bool {
	if s.thumbs == nil || f.Type != meta.TypeFile || !s.thumbs.gen.Supported(f.Mime) {
		return false
	}
	_, failed := s.thumbs.failed.Load(f.ID)
	return !failed
}

// Thumbnail 返回文件缩略图（JPEG）。还没有时当场生成；做不了时返回 ErrNoThumbnail。
// 生成过程不随请求取消（浏览器划走了，生成好的下次直接用）。
func (s *Service) Thumbnail(ctx context.Context, userID, id uuid.UUID) ([]byte, error) {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		return nil, meta.ErrNotFound
	}
	if t, err := s.store.GetThumbnail(ctx, id); err == nil {
		return crypto.Open(s.masterKey, t.Data, thumbAAD(id))
	} else if !errors.Is(err, meta.ErrNotFound) {
		return nil, err
	}
	if !s.CanThumbnail(f) {
		return nil, ErrNoThumbnail
	}
	c := s.startThumbnail(f)
	select {
	case <-c.done:
		return c.jpeg, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func thumbAAD(id uuid.UUID) []byte { return []byte("xiaoju thumbnail " + id.String()) }

// startThumbnail 开始生成（已经在生成就返回同一个任务）。
func (s *Service) startThumbnail(f *meta.File) *thumbCall {
	t := s.thumbs
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.pending[f.ID]; ok {
		return c
	}
	c := &thumbCall{done: make(chan struct{})}
	t.pending[f.ID] = c
	go func() {
		c.jpeg, c.err = s.makeThumbnail(f)
		t.mu.Lock()
		delete(t.pending, f.ID)
		t.mu.Unlock()
		close(c.done)
	}()
	return c
}

func (s *Service) makeThumbnail(f *meta.File) ([]byte, error) {
	t := s.thumbs
	t.slots <- struct{}{}
	defer func() { <-t.slots }()

	ctx := context.Background()
	start := time.Now()
	res, err := t.gen.Generate(ctx, func(ctx context.Context) (io.ReadSeekCloser, error) {
		_, rd, err := s.Open(ctx, f.UserID, f.ID)
		if err != nil {
			return nil, err
		}
		return rd, nil
	})
	if errors.Is(err, thumb.ErrNoThumbnail) {
		t.failed.Store(f.ID, struct{}{})
		s.log.Info("no thumbnail for file", "file", f.ID, "mime", f.Mime, "reason", err)
		return nil, err
	}
	if err != nil {
		s.log.Warn("thumbnail generation failed; will retry on next request", "file", f.ID, "err", err)
		return nil, err
	}
	sealed, err := crypto.Seal(s.masterKey, res.JPEG, thumbAAD(f.ID))
	if err != nil {
		return nil, err
	}
	th := &meta.Thumbnail{FileID: f.ID, Data: sealed, Duration: res.Duration}
	if !res.Taken.IsZero() {
		th.Taken = &res.Taken
	}
	if err := s.store.PutThumbnail(ctx, th); err != nil {
		return nil, err
	}
	s.log.Debug("thumbnail generated", "file", f.ID, "bytes", len(res.JPEG), "took", time.Since(start))
	return res.JPEG, nil
}

// queueThumbnail 上传完成后排队在后台生成缩略图，打开相册时就不用等了。
func (s *Service) queueThumbnail(f *meta.File) {
	if !s.CanThumbnail(f) || !(strings.HasPrefix(f.Mime, "image/") || strings.HasPrefix(f.Mime, "video/")) {
		return
	}
	t := s.thumbs
	t.once.Do(func() { go s.thumbnailWorker() })
	select {
	case t.queue <- f:
	default: // 排满了：第一次查看时再生成
	}
}

// thumbnailWorker 一个一个地生成排队的缩略图（只占一个名额，查看时的请求可以插队用其他名额）。
func (s *Service) thumbnailWorker() {
	for f := range s.thumbs.queue {
		if t, err := s.store.GetThumbnail(context.Background(), f.ID); err == nil && t.Version >= meta.ThumbnailVersion {
			continue
		}
		<-s.startThumbnail(f).done
	}
}

// BackfillThumbnails 给已经上传的照片、视频补生成缩略图：还没有的，以及旧版本生成的（那时还不读拍摄时间，
// 相册排序要用）。一个接一个地做，做完一遍就结束；服务启动时在后台调用一次。
func (s *Service) BackfillThumbnails(ctx context.Context) {
	if s.thumbs == nil {
		return
	}
	var (
		afterCreated time.Time
		afterID      uuid.UUID
		done, failed int
	)
	for ctx.Err() == nil {
		files, err := s.store.ListThumbnailBackfill(ctx, afterCreated, afterID, 100)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("thumbnail backfill stopped", "err", err)
			}
			return
		}
		if len(files) == 0 {
			break
		}
		if afterCreated.IsZero() {
			s.log.Info("thumbnail backfill started")
		}
		for _, f := range files {
			afterCreated, afterID = f.CreatedAt, f.ID
			if !s.CanThumbnail(f) {
				continue
			}
			select {
			case <-s.startThumbnail(f).done:
			case <-ctx.Done():
				return
			}
			// 结果看数据库：生成失败的不算（startThumbnail 的结果可能是别的请求先开始的那次）
			if t, err := s.store.GetThumbnail(ctx, f.ID); err == nil && t.Version >= meta.ThumbnailVersion {
				done++
			} else {
				failed++
			}
		}
	}
	if done+failed > 0 {
		s.log.Info("thumbnail backfill finished", "generated", done, "failed", failed)
	}
}

// ThumbnailDurations 返回这些文件中已生成缩略图的视频时长（照片为 0）。
func (s *Service) ThumbnailDurations(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Duration, error) {
	return s.store.ThumbnailDurations(ctx, ids)
}
