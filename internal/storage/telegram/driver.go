package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

const (
	// upload.getFile 要求 offset/limit 按 4KB 对齐且不跨 1MB 边界；统一按 1MB 对齐读取最简单。
	chunkSize = 1 << 20
	// MTProto 上传分片大小，最大 512KB。
	uploadPartSize = 512 * 1024
	// file_reference 会过期，缓存的文档位置只用一段时间。
	locationTTL = 30 * time.Minute
)

type Driver struct {
	api     *tg.Client
	channel *tg.InputChannel
	threads int

	mu    sync.Mutex
	cache map[int64]cachedLocation
}

type cachedLocation struct {
	loc     *tg.InputDocumentFileLocation
	size    int64
	expires time.Time
}

// New 解析频道并返回驱动。channelID 可以是 -100xxxxxxxxxx 形式，也可以是去掉前缀的裸 ID。
// Bot 必须已被加为该频道管理员（需要发消息、删消息权限）。
func New(ctx context.Context, api *tg.Client, channelID int64) (*Driver, error) {
	id := NormalizeChannelID(channelID)
	// Bot 账号可以用 access_hash=0 查询自己所在的频道，拿到真实 access_hash。
	res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: id}})
	if err != nil {
		return nil, fmt.Errorf("resolve channel %d: %w", id, err)
	}
	for _, chat := range res.GetChats() {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == id {
			return &Driver{
				api:     api,
				channel: ch.AsInput(),
				threads: 4,
				cache:   make(map[int64]cachedLocation),
			}, nil
		}
	}
	return nil, fmt.Errorf("channel %d not found (is the bot an admin of it?)", id)
}

func (d *Driver) Name() string { return "telegram" }

func (d *Driver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	up := uploader.NewUploader(d.api).WithPartSize(uploadPartSize).WithThreads(d.threads)
	file, err := up.Upload(ctx, uploader.NewUpload(req.Name, req.Body, req.Size))
	if err != nil {
		return storage.Location{}, fmt.Errorf("upload part: %w", err)
	}

	randomID, err := randomInt64()
	if err != nil {
		return storage.Location{}, err
	}
	updates, err := d.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerChannel{ChannelID: d.channel.ChannelID, AccessHash: d.channel.AccessHash},
		Media: &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   "application/octet-stream",
			ForceFile:  true,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: req.Name}},
		},
		Message:  req.Caption,
		RandomID: randomID,
	})
	if err != nil {
		return storage.Location{}, fmt.Errorf("send media: %w", err)
	}
	msgID, err := extractMessageID(updates, randomID)
	if err != nil {
		return storage.Location{}, err
	}
	return storage.Location{ChannelID: d.channel.ChannelID, MessageID: int64(msgID)}, nil
}

func (d *Driver) ReadRange(ctx context.Context, loc storage.Location, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 0 {
		return nil, errors.New("telegram: invalid range")
	}
	return &rangeReader{ctx: ctx, d: d, msgID: loc.MessageID, pos: offset, end: offset + length}, nil
}

func (d *Driver) Delete(ctx context.Context, locs []storage.Location) error {
	ids := make([]int, 0, len(locs))
	for _, l := range locs {
		ids = append(ids, int(l.MessageID))
	}
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		if _, err := d.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: d.channel, ID: ids[start:end],
		}); err != nil {
			return fmt.Errorf("delete messages: %w", err)
		}
	}
	d.mu.Lock()
	for _, l := range locs {
		delete(d.cache, l.MessageID)
	}
	d.mu.Unlock()
	return nil
}

const (
	listBatch = 100 // channels.getMessages 单次最多 100 个 ID
	// 已扫过已知最大 ID 后，连续这么多批（500 个 ID）都没有任何消息才认为到头了
	listEmptyBatches = 5
)

// List 按消息 ID 从 1 开始逐批扫描频道（Bot 不能调用 messages.getHistory），回调其中的文档消息。
// 已删除的消息（MessageEmpty）和服务消息跳过。
func (d *Driver) List(ctx context.Context, minMessageID int64, fn func(storage.Object) error) error {
	empty := 0
	for start := 1; ; start += listBatch {
		ids := make([]tg.InputMessageClass, listBatch)
		for i := range ids {
			ids[i] = &tg.InputMessageID{ID: start + i}
		}
		res, err := d.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: d.channel, ID: ids})
		if err != nil {
			return fmt.Errorf("list messages from %d: %w", start, err)
		}
		msgs, ok := res.(*tg.MessagesChannelMessages)
		if !ok {
			return fmt.Errorf("unexpected getMessages result %T", res)
		}
		found := false
		for _, m := range msgs.Messages {
			switch msg := m.(type) {
			case *tg.MessageService:
				found = true
			case *tg.Message:
				found = true
				if obj, ok := documentObject(d.channel.ChannelID, msg); ok {
					if err := fn(obj); err != nil {
						return err
					}
				}
			}
		}
		if found {
			empty = 0
		} else if empty++; empty >= listEmptyBatches && int64(start+listBatch) > minMessageID {
			return nil
		}
	}
}

func documentObject(channelID int64, msg *tg.Message) (storage.Object, bool) {
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return storage.Object{}, false
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return storage.Object{}, false
	}
	obj := storage.Object{
		Location: storage.Location{ChannelID: channelID, MessageID: int64(msg.ID)},
		Size:     doc.Size,
		Created:  time.Unix(int64(msg.Date), 0),
	}
	for _, a := range doc.Attributes {
		if fa, ok := a.(*tg.DocumentAttributeFilename); ok {
			obj.Name = fa.FileName
		}
	}
	return obj, true
}

// location 返回消息中文档的下载位置，带缓存；refresh=true 时强制重新获取（file_reference 过期时）。
func (d *Driver) location(ctx context.Context, msgID int64, refresh bool) (*tg.InputDocumentFileLocation, error) {
	d.mu.Lock()
	c, ok := d.cache[msgID]
	d.mu.Unlock()
	if ok && !refresh && time.Now().Before(c.expires) {
		return c.loc, nil
	}

	res, err := d.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: d.channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(msgID)}},
	})
	if err != nil {
		return nil, fmt.Errorf("get message %d: %w", msgID, err)
	}
	msgs, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(msgs.Messages) == 0 {
		return nil, storage.ErrNotFound
	}
	msg, ok := msgs.Messages[0].(*tg.Message)
	if !ok {
		return nil, storage.ErrNotFound // 已删除的消息会返回 MessageEmpty
	}
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, storage.ErrNotFound
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return nil, storage.ErrNotFound
	}
	loc := &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}

	d.mu.Lock()
	d.cache[msgID] = cachedLocation{loc: loc, size: doc.Size, expires: time.Now().Add(locationTTL)}
	d.mu.Unlock()
	return loc, nil
}

// getChunk 下载从 alignedOffset（1MB 对齐）开始的最多 1MB 数据，file_reference 过期时刷新一次重试。
func (d *Driver) getChunk(ctx context.Context, msgID, alignedOffset int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		loc, err := d.location(ctx, msgID, attempt > 0)
		if err != nil {
			return nil, err
		}
		res, err := d.api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: loc,
			Offset:   alignedOffset,
			Limit:    chunkSize,
		})
		if err != nil {
			if attempt == 0 && tgerr.Is(err, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID") {
				continue
			}
			return nil, fmt.Errorf("get file: %w", err)
		}
		f, ok := res.(*tg.UploadFile)
		if !ok {
			return nil, fmt.Errorf("unexpected getFile result %T", res)
		}
		return f.Bytes, nil
	}
}

// rangeReader 按 1MB 对齐块懒加载读取 [pos, end)。
type rangeReader struct {
	ctx   context.Context
	d     *Driver
	msgID int64
	pos   int64
	end   int64
	buf   []byte // 当前块中从 pos 开始尚未消费的数据
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if len(r.buf) == 0 {
		aligned, skip := alignChunk(r.pos)
		data, err := r.d.getChunk(r.ctx, r.msgID, aligned)
		if err != nil {
			return 0, err
		}
		if int64(len(data)) <= skip {
			return 0, io.ErrUnexpectedEOF
		}
		r.buf = data[skip:]
	}
	n := copy(p, r.buf[:min(int64(len(r.buf)), r.end-r.pos)])
	r.buf = r.buf[n:]
	r.pos += int64(n)
	return n, nil
}

func (r *rangeReader) Close() error { return nil }

// alignChunk 返回包含 offset 的 1MB 对齐块起点，以及 offset 在块内的偏移。
func alignChunk(offset int64) (aligned, skip int64) {
	aligned = offset - offset%chunkSize
	return aligned, offset - aligned
}

// NormalizeChannelID 把 Bot API 风格的 -100xxxxxxxxxx 转成 MTProto 使用的裸频道 ID。
func NormalizeChannelID(id int64) int64 {
	if id >= 0 {
		return id
	}
	s := strconv.FormatInt(-id, 10)
	if strings.HasPrefix(s, "100") && len(s) > 3 {
		if n, err := strconv.ParseInt(s[3:], 10, 64); err == nil {
			return n
		}
	}
	return -id
}

// extractMessageID 从 sendMedia 返回的 updates 中找出新消息的 ID。
func extractMessageID(updates tg.UpdatesClass, randomID int64) (int, error) {
	var list []tg.UpdateClass
	switch u := updates.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{u.Update}
	default:
		return 0, fmt.Errorf("unexpected updates type %T", updates)
	}
	for _, upd := range list {
		if u, ok := upd.(*tg.UpdateMessageID); ok && u.RandomID == randomID {
			return u.ID, nil
		}
	}
	for _, upd := range list {
		if u, ok := upd.(*tg.UpdateNewChannelMessage); ok {
			if m, ok := u.Message.(*tg.Message); ok {
				return m.ID, nil
			}
		}
	}
	return 0, errors.New("message id not found in updates")
}

func randomInt64() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}
