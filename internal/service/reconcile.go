package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

const (
	// StaleUploadAge 之前开始、至今仍未完成的上传视为已中断（如服务重启），可以清理。
	StaleUploadAge = 6 * time.Hour
	// ResumableUploadTTL：断点续传的上传这么久没收到新数据就清理（不会再接着传了）。
	ResumableUploadTTL = 24 * time.Hour
	// OrphanMinAge 之内的分块不当作残块：它可能属于正在进行的上传或备份，只是还没写进数据库。
	OrphanMinAge = 24 * time.Hour
)

// 本程序上传的分块名：<文件ID>.part0001（见 partName）
var partNameRe = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.part\d{4,}$`)

type ReconcileReport struct {
	Apply bool `json:"apply"`
	// 已中断的上传：dry-run 时是待清理数量，apply 时是已清理数量
	StaleUploads int `json:"stale_uploads"`
	// 主存储和备份都缺失、无法修复的分块（不会做任何改动）
	Lost    []PartIssue    `json:"lost"`
	Primary *ChannelReport `json:"primary"`
	Backup  *ChannelReport `json:"backup,omitempty"`
}

type ChannelReport struct {
	Documents  int `json:"documents"`  // 扫描到的文件消息数
	Referenced int `json:"referenced"` // 其中属于网盘文件的
	// 数据库里有、但这个频道里找不到（或大小不符）、可以从另一边修复的分块
	Missing []PartIssue `json:"missing"`
	// 数据库登记在这个频道、但本次扫描没找到（或大小不符）的分块总数，含无法修复的；
	// 大于 0 时认为扫描结果不可信，不删除残块
	Unseen int `json:"unseen_recorded_parts"`
	// 可以删除的残块（满足全部安全条件）
	Orphans []OrphanPart `json:"orphans"`
	// 不是本程序上传的消息（如手动发的测试消息），永远不碰
	SkippedForeign int `json:"skipped_foreign"`
	// 太新的残块候选（可能正在上传），本次不处理
	SkippedRecent int `json:"skipped_recent"`
	// 文件名指向一个仍然存在的文件、但没有被引用的分块：可疑，保留并报告
	KeptSuspicious []OrphanPart `json:"kept_suspicious"`
	Deleted        int          `json:"orphans_deleted"`
	// 本次没有删除残块的原因
	DeleteSkipped string `json:"delete_skipped,omitempty"`
	Error         string `json:"error,omitempty"`
}

type PartIssue struct {
	FileID      uuid.UUID `json:"file_id"`
	PartNo      int       `json:"part_no"`
	MessageID   int64     `json:"message_id"`
	Reason      string    `json:"reason"`
	Repaired    bool      `json:"repaired,omitempty"`
	RepairError string    `json:"repair_error,omitempty"`
}

type OrphanPart struct {
	MessageID int64     `json:"message_id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
}

// Reconcile 对账：扫描主存储和备份存储里实际存在的分块，与数据库逐一核对。
//
// apply=false 只生成报告，不做任何改动。apply=true 时：
//   - 清理中断超过 StaleUploadAge 的上传；
//   - 备份端缺失的分块：清除备份记录，后台同步会从主存储重新复制；
//   - 主存储缺失但备份完好的分块：从备份复制回主存储（校验 sha256）；
//   - 删除残块，只删除同时满足以下条件的：本程序上传的分块（文件名格式），没有任何分块记录引用它，
//     文件名里的文件 ID 在数据库中不存在，早于 OrphanMinAge；并且该频道这次扫描到了数据库里登记的
//     全部分块（否则认为扫描结果不可信，这个频道一个也不删）。
func (s *Service) Reconcile(ctx context.Context, apply bool) (*ReconcileReport, error) {
	rep := &ReconcileReport{Apply: apply, Primary: &ChannelReport{}}
	if s.backup != nil {
		rep.Backup = &ChannelReport{}
	}

	stale, err := s.store.ListStaleUploads(ctx, time.Now().Add(-StaleUploadAge), time.Now().Add(-ResumableUploadTTL))
	if err != nil {
		return nil, err
	}
	rep.StaleUploads = len(stale)
	if apply {
		if rep.StaleUploads, err = s.CleanupStaleUploads(ctx); err != nil {
			return nil, err
		}
	}

	parts, statuses, err := s.store.AllParts(ctx)
	if err != nil {
		return nil, err
	}

	// 扫描。任何一个频道扫描出错都不做任何改动，只返回报告。
	var maxPrimary, maxBackup int64
	for i := range parts {
		maxPrimary = max(maxPrimary, parts[i].MessageID)
		if parts[i].HasBackup() {
			maxBackup = max(maxBackup, *parts[i].BackupMessageID)
		}
	}
	primaryObjs, err := scan(ctx, s.driver, maxPrimary)
	if err != nil {
		rep.Primary.Error = err.Error()
		return rep, nil
	}
	var backupObjs map[int64]storage.Object
	if s.backup != nil {
		if backupObjs, err = scan(ctx, s.backup, maxBackup); err != nil {
			rep.Backup.Error = err.Error()
			return rep, nil
		}
	}
	rep.Primary.Documents = len(primaryObjs)
	if rep.Backup != nil {
		rep.Backup.Documents = len(backupObjs)
	}

	// 核对数据库里的每个分块
	refPrimary := map[int64]bool{}
	refBackup := map[int64]bool{}
	var fixPrimary, fixBackup []*meta.Part
	for i := range parts {
		p := &parts[i]
		refPrimary[p.MessageID] = true
		if p.HasBackup() {
			refBackup[*p.BackupMessageID] = true
		}
		if statuses[i] != meta.StatusActive {
			continue // 上传中的文件不检查（中断的会被上面的清理处理）
		}
		primaryOK, primaryWhy := present(primaryObjs, p.MessageID, p.Size)
		if !primaryOK {
			rep.Primary.Unseen++
		}
		backupOK, backupWhy := false, "no backup"
		if p.HasBackup() && backupObjs != nil {
			if backupOK, backupWhy = present(backupObjs, *p.BackupMessageID, p.Size); !backupOK {
				rep.Backup.Unseen++
			}
		}
		switch {
		case !primaryOK && !backupOK:
			rep.Lost = append(rep.Lost, PartIssue{FileID: p.FileID, PartNo: p.PartNo, MessageID: p.MessageID,
				Reason: "primary " + primaryWhy + "; backup " + backupWhy})
		case !primaryOK:
			rep.Primary.Missing = append(rep.Primary.Missing, PartIssue{FileID: p.FileID, PartNo: p.PartNo, MessageID: p.MessageID, Reason: primaryWhy})
			fixPrimary = append(fixPrimary, p)
		case p.HasBackup() && backupObjs != nil && !backupOK:
			rep.Backup.Missing = append(rep.Backup.Missing, PartIssue{FileID: p.FileID, PartNo: p.PartNo, MessageID: *p.BackupMessageID, Reason: backupWhy})
			fixBackup = append(fixBackup, p)
		}
	}

	// 找残块
	if err := s.findOrphans(ctx, rep.Primary, primaryObjs, refPrimary); err != nil {
		return nil, err
	}
	if rep.Backup != nil {
		if err := s.findOrphans(ctx, rep.Backup, backupObjs, refBackup); err != nil {
			return nil, err
		}
	}

	if !apply {
		return rep, nil
	}

	// 修复缺失的分块
	for i, p := range fixPrimary {
		issue := &rep.Primary.Missing[i]
		if err := s.restorePrimaryFromBackup(ctx, p); err != nil {
			issue.RepairError = err.Error()
			s.log.Error("restore primary part from backup", "file", p.FileID, "part", p.PartNo, "err", err)
		} else {
			issue.Repaired = true
		}
	}
	for i, p := range fixBackup {
		issue := &rep.Backup.Missing[i]
		// 清除备份记录后，后台同步会重新从主存储复制
		if err := s.store.ClearPartBackup(ctx, p.FileID, p.PartNo); err != nil {
			issue.RepairError = err.Error()
		} else {
			issue.Repaired = true
		}
	}

	// 删除残块
	s.deleteOrphans(ctx, s.driver, rep.Primary)
	if rep.Backup != nil {
		s.deleteOrphans(ctx, s.backup, rep.Backup)
	}
	return rep, nil
}

func scan(ctx context.Context, d storage.Driver, minID int64) (map[int64]storage.Object, error) {
	l, ok := d.(storage.Lister)
	if !ok {
		return nil, fmt.Errorf("storage %q cannot list its parts", d.Name())
	}
	objs := map[int64]storage.Object{}
	err := l.List(ctx, minID, func(o storage.Object) error {
		objs[o.MessageID] = o
		return nil
	})
	return objs, err
}

func present(objs map[int64]storage.Object, id, size int64) (bool, string) {
	o, ok := objs[id]
	switch {
	case !ok:
		return false, "not found"
	case o.Size != size:
		return false, fmt.Sprintf("size %d, expected %d", o.Size, size)
	}
	return true, ""
}

func (s *Service) findOrphans(ctx context.Context, rep *ChannelReport, objs map[int64]storage.Object, referenced map[int64]bool) error {
	cutoff := time.Now().Add(-OrphanMinAge)
	known := map[string]bool{}
	for id, o := range objs {
		if referenced[id] {
			rep.Referenced++
			continue
		}
		m := partNameRe.FindStringSubmatch(o.Name)
		if m == nil {
			rep.SkippedForeign++
			continue
		}
		if o.Created.After(cutoff) {
			rep.SkippedRecent++
			continue
		}
		exists, seen := known[m[1]]
		if !seen {
			fileID, _ := uuid.Parse(m[1])
			var err error
			if exists, err = s.store.FileExists(ctx, fileID); err != nil {
				return err
			}
			known[m[1]] = exists
		}
		op := OrphanPart{MessageID: id, Name: o.Name, Size: o.Size, Created: o.Created}
		if exists {
			rep.KeptSuspicious = append(rep.KeptSuspicious, op)
		} else {
			rep.Orphans = append(rep.Orphans, op)
		}
	}
	return nil
}

func (s *Service) deleteOrphans(ctx context.Context, d storage.Driver, rep *ChannelReport) {
	if len(rep.Orphans) == 0 {
		return
	}
	if rep.Unseen > 0 {
		// 有登记过的分块没扫到：扫描结果可能不完整，为安全起见这个频道不删
		rep.DeleteSkipped = "some recorded parts were not found in this scan; run reconcile again after the repair"
		return
	}
	locs := make([]storage.Location, len(rep.Orphans))
	for i, o := range rep.Orphans {
		locs[i] = storage.Location{MessageID: o.MessageID}
	}
	if err := d.Delete(ctx, locs); err != nil {
		rep.DeleteSkipped = "delete failed: " + err.Error()
		return
	}
	rep.Deleted = len(locs)
	s.log.Info("deleted orphan parts", "storage", d.Name(), "count", len(locs))
}

// restorePrimaryFromBackup 把备份里的分块复制回主存储，校验 sha256 后更新位置。
func (s *Service) restorePrimaryFromBackup(ctx context.Context, p *meta.Part) error {
	src, err := s.backup.ReadRange(ctx, backupLocation(p), 0, p.Size)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	defer src.Close()
	h := sha256.New()
	loc, err := s.driver.Put(ctx, storage.PutRequest{
		Name:    partName(p.FileID, p.PartNo),
		Caption: caption(p.FileID, p.PartNo, p.SHA256),
		Body:    io.TeeReader(src, h),
		Size:    p.Size,
	})
	if err != nil {
		return fmt.Errorf("write primary: %w", err)
	}
	if !bytes.Equal(h.Sum(nil), p.SHA256) {
		s.driver.Delete(context.WithoutCancel(ctx), []storage.Location{loc})
		return errors.New("backup data does not match recorded sha256")
	}
	return s.store.SetPartPrimary(ctx, p.FileID, p.PartNo, loc.ChannelID, loc.MessageID)
}

// CleanupStaleUploads 清理中断超过 StaleUploadAge 的上传（断点续传的是 ResumableUploadTTL 没动静的）：
// 删除已经传上去的分块和记录。某个文件的分块删除失败时保留它的记录，下次再试。返回清理掉的文件数。
func (s *Service) CleanupStaleUploads(ctx context.Context) (int, error) {
	ids, err := s.store.ListStaleUploads(ctx, time.Now().Add(-StaleUploadAge), time.Now().Add(-ResumableUploadTTL))
	if err != nil {
		return 0, err
	}
	cleaned := map[uuid.UUID]bool{}
	for _, id := range ids {
		if err := s.discardUpload(ctx, id); err != nil {
			if errors.Is(err, errKeepRecord) {
				continue
			}
			return len(cleaned), err
		}
		cleaned[id] = true
	}
	s.evictIdleResumables(0, cleaned)
	if len(cleaned) > 0 {
		s.log.Info("cleaned up interrupted uploads", "files", len(cleaned))
	}
	return len(cleaned), nil
}

// errKeepRecord：存储端的分块没删掉，记录先留着（下次再删）。
var errKeepRecord = errors.New("parts not deleted; keeping the record")

// discardUpload 删掉一个没传完的上传：存储端（含备份）已经传上去的分块，和数据库记录。
func (s *Service) discardUpload(ctx context.Context, id uuid.UUID) error {
	parts, err := s.store.ListParts(ctx, id)
	if err != nil {
		return err
	}
	var locs, backupLocs []storage.Location
	for i := range parts {
		locs = append(locs, storage.Location{ChannelID: parts[i].ChannelID, MessageID: parts[i].MessageID})
		if parts[i].HasBackup() {
			backupLocs = append(backupLocs, backupLocation(&parts[i]))
		}
	}
	if len(locs) > 0 {
		if err := s.driver.Delete(ctx, locs); err != nil {
			s.log.Warn("discard upload: delete parts", "file", id, "err", err)
			return errKeepRecord
		}
	}
	if len(backupLocs) > 0 && s.backup != nil {
		if err := s.backup.Delete(ctx, backupLocs); err != nil {
			s.log.Warn("discard upload: delete backup parts", "file", id, "err", err)
			return errKeepRecord
		}
	}
	return s.store.DeleteRow(ctx, id)
}

// TrashKeep：回收站里的东西放这么久后自动永久删除。
const TrashKeep = 30 * 24 * time.Hour

// PurgeExpiredTrash 永久删除在回收站里放了超过 TrashKeep 的条目（文件夹连同里面的文件）。删不掉的跳过，下次再试。
func (s *Service) PurgeExpiredTrash(ctx context.Context) (int, error) {
	items, err := s.store.ListExpiredTrash(ctx, time.Now().Add(-TrashKeep))
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, f := range items {
		if err := s.Purge(ctx, f.UserID, f.ID); err != nil {
			if ctx.Err() != nil {
				return purged, ctx.Err()
			}
			s.log.Warn("trash: could not delete an expired item", "file", f.ID, "err", err)
			continue
		}
		purged++
	}
	if purged > 0 {
		s.log.Info("trash: deleted items kept for 30 days", "items", purged)
	}
	return purged, nil
}

// RunMaintenance 定期清理中断的上传、回收站里放满 30 天的东西，直到 ctx 取消。
func (s *Service) RunMaintenance(ctx context.Context, every time.Duration) {
	for {
		if _, err := s.CleanupStaleUploads(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("cleanup stale uploads", "err", err)
		}
		if _, err := s.PurgeExpiredTrash(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("purge expired trash", "err", err)
		}
		s.evictIdleResumables(resumableIdle, nil)
		select {
		case <-time.After(every):
		case <-ctx.Done():
			return
		}
	}
}
