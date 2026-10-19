package service

import (
	"container/list"
	"context"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// 大图预览（最长边 1600 的 JPEG，见 thumb.Preview）：看大图时先显示它，比原图小得多，HEIC 在别的浏览器里也能看。
// 当场从原图生成，只放在内存里（不写磁盘、不进数据库：数据库是免费的，放不下这么多；解密后的照片也不该留在磁盘上），
// 最近看过的留着，超过 previewCacheBytes 时丢掉最久没看的。同一张同时只生成一次，生成不随请求取消（划走了，
// 生成好的下次直接用）。

const (
	previewCacheBytes = 96 << 20 // 约三四百张
	previewWorkers    = 2        // 同时生成几张（要从 Telegram 拉原图、解码，占 CPU 和内存）
)

type previews struct {
	slots   chan struct{}
	mu      sync.Mutex
	items   map[uuid.UUID]*list.Element // 值是 *previewEntry
	order   *list.List                  // 前面是最近看过的
	bytes   int
	pending map[uuid.UUID]*thumbCall
}

type previewEntry struct {
	id   uuid.UUID
	jpeg []byte
}

func newPreviews() *previews {
	return &previews{
		slots: make(chan struct{}, previewWorkers), items: map[uuid.UUID]*list.Element{},
		order: list.New(), pending: map[uuid.UUID]*thumbCall{},
	}
}

func (p *previews) get(id uuid.UUID) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.items[id]; ok {
		p.order.MoveToFront(e)
		return e.Value.(*previewEntry).jpeg
	}
	return nil
}

func (p *previews) put(id uuid.UUID, jpeg []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.items[id]; ok || len(jpeg) > previewCacheBytes/4 {
		return
	}
	p.items[id] = p.order.PushFront(&previewEntry{id: id, jpeg: jpeg})
	p.bytes += len(jpeg)
	for p.bytes > previewCacheBytes {
		old := p.order.Remove(p.order.Back()).(*previewEntry)
		delete(p.items, old.id)
		p.bytes -= len(old.jpeg)
	}
}

// forget 丢掉一个文件的预览（文件永久删除了）。
func (p *previews) forget(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.items[id]; ok {
		p.order.Remove(e)
		delete(p.items, id)
		p.bytes -= len(e.Value.(*previewEntry).jpeg)
	}
}

// CanPreview：这个文件能做大图预览（照片，且装了能解它的程序）。
func (s *Service) CanPreview(f *meta.File) bool {
	return s.thumbs != nil && f.Type == meta.TypeFile && s.thumbs.gen.SupportsPreview(f.Mime)
}

// Preview 返回照片的大图预览（JPEG）；做不了时返回 ErrNoThumbnail。
func (s *Service) Preview(ctx context.Context, userID, id uuid.UUID) ([]byte, error) {
	f, err := s.store.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if f.Type != meta.TypeFile || f.TrashedAt != nil {
		return nil, meta.ErrNotFound
	}
	if !s.CanPreview(f) {
		return nil, ErrNoThumbnail
	}
	p := s.previews
	if b := p.get(id); b != nil {
		return b, nil
	}
	p.mu.Lock()
	c, ok := p.pending[id]
	if !ok {
		c = &thumbCall{done: make(chan struct{})}
		p.pending[id] = c
		go func() {
			c.jpeg, c.err = s.makePreview(f)
			if c.err == nil {
				p.put(id, c.jpeg)
			}
			p.mu.Lock()
			delete(p.pending, id)
			p.mu.Unlock()
			close(c.done)
		}()
	}
	p.mu.Unlock()
	select {
	case <-c.done:
		return c.jpeg, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) makePreview(f *meta.File) ([]byte, error) {
	p := s.previews
	p.slots <- struct{}{}
	defer func() { <-p.slots }()
	start := time.Now()
	b, err := s.thumbs.gen.Preview(context.Background(), func(ctx context.Context) (io.ReadSeekCloser, error) {
		_, rd, err := s.Open(ctx, f.UserID, f.ID)
		if err != nil {
			return nil, err
		}
		return rd, nil
	})
	if err != nil {
		s.log.Info("no preview for file", "file", f.ID, "mime", f.Mime, "reason", err)
		return nil, err
	}
	s.log.Debug("preview generated", "file", f.ID, "bytes", len(b), "took", time.Since(start))
	return b, nil
}
