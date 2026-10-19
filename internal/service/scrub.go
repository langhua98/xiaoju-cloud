package service

// 定期检查：后台慢慢把每个分块从主频道、备份频道各下载一遍，核对密文的 sha256（上传时记下的）。
//   - 主频道的坏了或没了、备份是好的：从备份复制回主频道（restorePrimaryFromBackup）；
//   - 备份的坏了或没了：清掉备份记录，后台备份同步会从主频道重新复制一份；
//   - 两份都坏了：记下来（file_parts.verify_error），备份状态里显示，没法自动修。
// 每个分块 ScrubEvery 检查一次；一次只查一块，查完歇一会儿，不影响正常的上传下载，也不容易被 Telegram 限流。
// 读的时候出了说不清的错（网络断了、Telegram 一时不可用），不算坏，这一轮先停下，过一会儿再查。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

const (
	ScrubEvery = 30 * 24 * time.Hour
	scrubBatch = 20
)

// 查完一块歇多久：至少 scrubPause，再按大小每 scrubRate 字节歇一秒，平均下来每秒只读几 MB
// （第一次所有分块都没查过，要把整个网盘读两遍；不能抢了看视频、上传的带宽）。测试里改短。
var (
	scrubPause       = 2 * time.Second
	scrubRate  int64 = 4 << 20
)

// RunScrub 一直做定期检查，直到 ctx 取消。启动后先等 start，查完一轮（都查过了）等 idle 再看。
func (s *Service) RunScrub(ctx context.Context, start, idle time.Duration) {
	select {
	case <-time.After(start):
	case <-ctx.Done():
		return
	}
	for {
		n, err := s.ScrubOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("part check paused", "err", err)
		}
		if n == scrubBatch && err == nil {
			continue
		}
		select {
		case <-time.After(idle):
		case <-ctx.Done():
			return
		}
	}
}

// ScrubOnce 检查一批最久没查过的分块，返回查了几块。
func (s *Service) ScrubOnce(ctx context.Context) (int, error) {
	parts, err := s.store.ListPartsToVerify(ctx, time.Now().Add(-ScrubEvery), scrubBatch)
	if err != nil {
		return 0, err
	}
	checked := 0
	for i := range parts {
		p := &parts[i]
		problem, err := s.verifyPart(ctx, p)
		if err != nil {
			return checked, err // 说不清是不是坏了：先停下
		}
		if problem != "" {
			s.log.Error("part check: damaged part could not be repaired", "file", p.FileID, "part", p.PartNo, "problem", problem)
		}
		if err := s.store.SetPartVerified(ctx, p.FileID, p.PartNo, problem); err != nil {
			return checked, err
		}
		checked++
		pause := scrubPause
		if scrubRate > 0 {
			pause += time.Duration(p.Size) * time.Second / time.Duration(scrubRate)
		}
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return checked, ctx.Err()
		}
	}
	return checked, nil
}

type partState int

const (
	partOK partState = iota
	partBad
	partUnknown
)

// checkPart 下载一份分块，核对 sha256：对得上 partOK；没了、短了、对不上 partBad；读的时候出了别的错 partUnknown。
func checkPart(ctx context.Context, d storage.Driver, loc storage.Location, p *meta.Part) (partState, error) {
	rc, err := d.ReadRange(ctx, loc, 0, p.Size)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return partBad, err
		}
		return partUnknown, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	switch {
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, io.ErrUnexpectedEOF):
		return partBad, err
	case err != nil:
		return partUnknown, err
	case n != p.Size:
		return partBad, fmt.Errorf("got %d of %d bytes", n, p.Size)
	case !bytes.Equal(h.Sum(nil), p.SHA256):
		return partBad, errors.New("sha256 does not match")
	}
	return partOK, nil
}

// verifyPart 查一个分块的两份，坏了的能修就修。返回没修好的问题（空表示好了）；说不清是不是坏了时返回错误。
func (s *Service) verifyPart(ctx context.Context, p *meta.Part) (string, error) {
	primary, perr := checkPart(ctx, s.driver, storage.Location{ChannelID: p.ChannelID, MessageID: p.MessageID}, p)
	if primary == partUnknown {
		return "", fmt.Errorf("read primary part: %w", perr)
	}
	backup, berr := partOK, error(nil)
	hasBackup := s.backup != nil && p.HasBackup()
	if hasBackup {
		if backup, berr = checkPart(ctx, s.backup, backupLocation(p), p); backup == partUnknown {
			return "", fmt.Errorf("read backup part: %w", berr)
		}
	}
	switch {
	case primary == partOK && backup == partOK:
		return "", nil
	case primary == partOK: // 备份坏了：重新复制
		s.log.Warn("part check: backup copy is damaged; it will be copied again", "file", p.FileID, "part", p.PartNo, "err", berr)
		return "", s.store.ClearPartBackup(ctx, p.FileID, p.PartNo)
	case hasBackup && backup == partOK: // 主频道的坏了：从备份修
		if err := s.restorePrimaryFromBackup(ctx, p); err != nil {
			return fmt.Sprintf("主频道的这块坏了（%v），从备份修复失败：%v", perr, err), nil
		}
		s.log.Warn("part check: repaired a damaged part from the backup", "file", p.FileID, "part", p.PartNo, "err", perr)
		return "", nil
	case hasBackup:
		return fmt.Sprintf("两个频道里的这块都坏了：%v；%v", perr, berr), nil
	default:
		return fmt.Sprintf("这块坏了，也没有备份：%v", perr), nil
	}
}
