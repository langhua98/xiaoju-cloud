package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

const backupBatch = 20

// backupKickDelay：上传了新分块后，等这么久再去备份（文件激活了才备份它的分块）。
var backupKickDelay = 5 * time.Second

type BackupStatus struct {
	Enabled bool   `json:"enabled"`
	Error   string `json:"error,omitempty"`
	meta.BackupStats
}

// BackupStatus 返回备份是否启用以及备份进度。
func (s *Service) BackupStatus(ctx context.Context) (BackupStatus, error) {
	st, err := s.store.BackupStats(ctx)
	return BackupStatus{Enabled: s.backup != nil, Error: s.backupErr, BackupStats: st}, err
}

// RunBackupSync 在后台把还没有备份的分块（包括启用备份之前上传的旧文件、上传时备份失败的分块）
// 从主存储复制到备份存储（备份频道有几个 Bot 就同时复制几个，见 SyncBackupOnce）；直到 ctx 取消才返回。
func (s *Service) RunBackupSync(ctx context.Context, idle time.Duration) {
	if s.backup == nil {
		return
	}
	s.log.Info("backup sync started")
	for {
		copied, err := s.SyncBackupOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Error("backup sync", "err", err)
		}
		if copied > 0 {
			s.log.Info("backup sync progress", "copied_parts", copied)
		}
		// 本轮有进展且没有出错就立即继续，否则等一会儿再查
		if copied == backupBatch && err == nil {
			continue
		}
		select {
		case <-time.After(idle):
		case <-s.backupKick: // 刚传了新分块
			// 等一下再查：分块要等整个文件传完才算数（ListPartsMissingBackup 只看已经激活的文件）
			select {
			case <-time.After(backupKickDelay):
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// SyncBackupOnce 复制一批缺少备份的分块，返回成功复制的数量；某个分块失败时跳过它继续，最后汇总错误。
// 备份频道有几个 Bot 就同时复制几个分块（每个 Bot 各自排队发消息），只有一个 Bot 时一个一个来。
func (s *Service) SyncBackupOnce(ctx context.Context) (int, error) {
	parts, err := s.store.ListPartsMissingBackup(ctx, backupBatch)
	if err != nil {
		return 0, err
	}
	workers := 1
	if p, ok := s.backup.(interface{ Bots() int }); ok {
		workers = max(p.Bots(), 1)
	}
	var (
		mu     sync.Mutex
		copied int
		errs   []error
		wg     sync.WaitGroup
	)
	next := make(chan *meta.Part)
	for range min(workers, len(parts)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range next {
				err := s.copyToBackup(ctx, p)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("file %s part %d: %w", p.FileID, p.PartNo, err))
				} else {
					copied++
				}
				mu.Unlock()
			}
		}()
	}
	for i := range parts {
		if ctx.Err() != nil {
			break
		}
		next <- &parts[i]
	}
	close(next)
	wg.Wait()
	return copied, errors.Join(errs...)
}

// copyToBackup 从主存储流式读取分块密文写入备份存储，并用记录的 sha256 校验写入的内容。
func (s *Service) copyToBackup(ctx context.Context, p *meta.Part) error {
	src, err := s.driver.ReadRange(ctx, storage.Location{ChannelID: p.ChannelID, MessageID: p.MessageID}, 0, p.Size)
	if err != nil {
		return fmt.Errorf("read primary: %w", err)
	}
	defer src.Close()

	h := sha256.New()
	loc, err := s.backup.Put(ctx, storage.PutRequest{
		Name:    partName(p.FileID, p.PartNo),
		Caption: caption(p.FileID, p.PartNo, p.SHA256),
		Body:    io.TeeReader(src, h),
		Size:    p.Size,
	})
	if err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if !bytes.Equal(h.Sum(nil), p.SHA256) {
		// 主存储里的数据和上传时不一致，不能把它当作备份
		if derr := s.backup.Delete(context.WithoutCancel(ctx), []storage.Location{loc}); derr != nil {
			s.log.Error("delete mismatched backup part", "err", derr)
		}
		return fmt.Errorf("primary data does not match recorded sha256")
	}
	return s.store.SetPartBackup(ctx, p.FileID, p.PartNo, loc.ChannelID, loc.MessageID)
}
