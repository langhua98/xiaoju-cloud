package service

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// FileReader 把多个加密分块拼成一个可 Seek 的明文流，供 http.ServeContent 处理 Range 请求。
// 只有真正 Read 时才会去存储端拉取数据，Seek 本身不产生网络请求。
type FileReader struct {
	ctx    context.Context
	driver storage.Driver
	backup storage.Driver // 可选：主存储读取失败时改读备份
	log    *slog.Logger
	dek    []byte
	parts  []meta.Part
	starts []int64 // 每个分块在整个文件中的起始偏移
	size   int64

	useBackup []bool // 哪些分块已经切换到备份读取

	pos    int64
	cur    io.ReadCloser // 当前分块从 curPos 开始的解密流
	curIdx int
	curPos int64
	curEnd int64 // 当前分块在文件中的结束偏移
}

func NewFileReader(ctx context.Context, driver storage.Driver, dek []byte, parts []meta.Part) *FileReader {
	r := &FileReader{
		ctx: ctx, driver: driver, dek: dek, parts: parts,
		starts: make([]int64, len(parts)), useBackup: make([]bool, len(parts)),
	}
	for i, p := range parts {
		r.starts[i] = r.size
		r.size += p.Size
	}
	return r
}

// fallback 在分块 i 读取失败时切换到备份；返回 false 表示没有可用的备份（或请求已取消）。
func (r *FileReader) fallback(i int, cause error) bool {
	if r.backup == nil || r.useBackup[i] || !r.parts[i].HasBackup() || r.ctx.Err() != nil {
		return false
	}
	r.useBackup[i] = true
	if r.log != nil {
		r.log.Warn("primary storage read failed; reading from backup",
			"file", r.parts[i].FileID, "part", r.parts[i].PartNo, "err", cause)
	}
	return true
}

func (r *FileReader) Size() int64 { return r.size }

func (r *FileReader) Read(p []byte) (int, error) {
	for {
		if r.pos >= r.size {
			return 0, io.EOF
		}
		if r.cur == nil || r.curPos != r.pos {
			if err := r.open(); err != nil {
				if r.fallback(r.partIndex(r.pos), err) {
					continue
				}
				return 0, err
			}
		}
		n, err := r.cur.Read(p)
		r.pos += int64(n)
		r.curPos = r.pos
		if errors.Is(err, io.EOF) {
			r.closeCur()
			if r.pos < r.curEnd {
				err = io.ErrUnexpectedEOF // 存储端返回的数据比记录的分块短
			} else {
				err = nil
			}
		}
		if err != nil && r.fallback(r.curIdx, err) {
			// 已读到的数据是正确的明文，交给调用方；之后从当前位置改读备份
			r.closeCur()
			if n > 0 {
				return n, nil
			}
			continue
		}
		if n > 0 || err != nil {
			return n, err
		}
		// n == 0 且分块恰好读完：继续读下一块
	}
}

func (r *FileReader) open() error {
	r.closeCur()
	i := r.partIndex(r.pos)
	part := &r.parts[i]
	off := r.pos - r.starts[i]
	driver, loc := r.driver, storage.Location{ChannelID: part.ChannelID, MessageID: part.MessageID}
	if r.useBackup[i] {
		driver, loc = r.backup, backupLocation(part)
	}
	rc, err := driver.ReadRange(r.ctx, loc, off, part.Size-off)
	if err != nil {
		return fmt.Errorf("read part %d: %w", part.PartNo, err)
	}
	stream, err := crypto.NewCTRAt(r.dek, part.IV, off)
	if err != nil {
		rc.Close()
		return err
	}
	r.cur = struct {
		io.Reader
		io.Closer
	}{&cipher.StreamReader{S: stream, R: rc}, rc}
	r.curIdx = i
	r.curPos = r.pos
	r.curEnd = r.starts[i] + part.Size
	return nil
}

func (r *FileReader) partIndex(pos int64) int {
	// 分块数量很少，线性查找即可
	for i := len(r.starts) - 1; i >= 0; i-- {
		if pos >= r.starts[i] {
			return i
		}
	}
	return 0
}

func (r *FileReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = abs
	return abs, nil
}

func (r *FileReader) closeCur() {
	if r.cur != nil {
		r.cur.Close()
		r.cur = nil
	}
}

func (r *FileReader) Close() error {
	r.closeCur()
	return nil
}
