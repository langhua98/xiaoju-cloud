package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/dbdump"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
)

// 数据库自动备份：定期把整个数据库导出、用主密钥派生的密钥加密，存三份：
//
//  1. 网盘根目录的「数据库备份」文件夹（网页上能看到、能下载；这个文件夹配对了谷歌云盘同步的话，谷歌云盘里也有一份）；
//  2. 直接作为一个文件发进主频道并置顶；
//  3. 同样发进备份频道（另一个账号）并置顶。
//
// 网盘里那份要靠数据库才能找到、解开（文件密钥包在数据库里），所以数据库丢了时用不上；
// 频道里那两份不靠数据库：置顶消息就是最新的备份，`xiaoju dbrestore` 直接从置顶消息下载、恢复。
// 所有副本里都没有主密钥（主密钥只在环境变量里），没有主密钥打不开。

// DBBackupFolder 是网盘根目录里放数据库备份的文件夹。
const DBBackupFolder = "数据库备份"

// DBBackupCaptionPrefix 是频道里备份消息说明的开头。
const DBBackupCaptionPrefix = "xiaoju-db-backup "

// DBBackupCaption 是频道里备份消息说明的 JSON 部分。
type DBBackupCaption struct {
	Created   time.Time `json:"created"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Migration string    `json:"migration"`
}

// ParseDBBackupCaption 解析频道消息的说明；不是数据库备份时 ok 为 false。
func ParseDBBackupCaption(s string) (c DBBackupCaption, ok bool) {
	line, _, _ := strings.Cut(s, "\n")
	rest, found := strings.CutPrefix(line, DBBackupCaptionPrefix)
	if !found || json.Unmarshal([]byte(rest), &c) != nil {
		return DBBackupCaption{}, false
	}
	return c, true
}

type DBBackupOptions struct {
	UserID   uuid.UUID
	Every    time.Duration  // 多久备份一次
	Location *time.Location // 文件名里的时间按这个时区
	// KeepFiles 是网盘文件夹里留几份，KeepMessages 是每个频道里留几份（更旧的删掉）。
	KeepFiles    int
	KeepMessages int
}

// DBBackupStatus 是数据库备份的状态（放在 /api/backup/status 的 db 字段里）。
type DBBackupStatus struct {
	Enabled bool             `json:"enabled"`
	Running bool             `json:"running"`
	Every   string           `json:"every,omitempty"`
	Folder  string           `json:"folder"`
	Last    *meta.DBBackup   `json:"last,omitempty"`
	Recent  []*meta.DBBackup `json:"recent,omitempty"`
	Error   string           `json:"error,omitempty"` // 最近一次失败的原因（成功后清空）
}

type dbBackups struct {
	opt     DBBackupOptions
	run     sync.Mutex // 同时只备份一次
	mu      sync.Mutex
	running bool
	lastErr string
	kick    chan struct{}
}

// ErrDBBackupRunning 表示已经在备份了。
var ErrDBBackupRunning = errors.New("a database backup is already running")

// EnableDBBackup 打开数据库自动备份（RunDBBackup 之前调用）。
func (s *Service) EnableDBBackup(opt DBBackupOptions) {
	if opt.Every <= 0 {
		opt.Every = 24 * time.Hour
	}
	if opt.Location == nil {
		opt.Location = time.UTC
	}
	if opt.KeepFiles <= 0 {
		opt.KeepFiles = 14
	}
	if opt.KeepMessages <= 0 {
		opt.KeepMessages = 30
	}
	s.dbBackup = &dbBackups{opt: opt, kick: make(chan struct{}, 1)}
}

// DBBackupStatus 返回数据库备份的状态。
func (s *Service) DBBackupStatus(ctx context.Context) (*DBBackupStatus, error) {
	st := &DBBackupStatus{Folder: DBBackupFolder}
	b := s.dbBackup
	if b == nil {
		return st, nil
	}
	b.mu.Lock()
	st.Enabled, st.Running, st.Error, st.Every = true, b.running, b.lastErr, b.opt.Every.String()
	b.mu.Unlock()
	list, err := s.store.ListDBBackups(ctx, 5)
	if err != nil {
		return nil, err
	}
	for i := range list {
		st.Recent = append(st.Recent, &list[i])
	}
	if len(st.Recent) > 0 {
		st.Last = st.Recent[0]
	}
	return st, nil
}

// StartDBBackup 让后台马上备份一次（网页上的「立即备份」）。已经在备份时返回 ErrDBBackupRunning。
func (s *Service) StartDBBackup() error {
	b := s.dbBackup
	if b == nil {
		return errors.New("database backup is not enabled")
	}
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if running {
		return ErrDBBackupRunning
	}
	select {
	case b.kick <- struct{}{}:
	default:
	}
	return nil
}

// RunDBBackup 在后台定期备份数据库：启动 start 之后看一次，上次备份已经超过 Every 就备份；之后每小时看一次。
func (s *Service) RunDBBackup(ctx context.Context, start time.Duration) {
	b := s.dbBackup
	if b == nil {
		return
	}
	wait := start
	for {
		forced := false
		select {
		case <-time.After(wait):
		case <-b.kick:
			forced = true
		case <-ctx.Done():
			return
		}
		wait = time.Hour
		if !forced {
			if last, err := s.store.ListDBBackups(ctx, 1); err != nil {
				s.log.Error("db backup: read last backup", "err", err)
				continue
			} else if len(last) > 0 && last[0].Error == "" && time.Since(last[0].CreatedAt) < b.opt.Every-time.Minute {
				wait = min(time.Hour, time.Until(last[0].CreatedAt.Add(b.opt.Every)))
				continue
			}
		}
		if _, err := s.BackupDatabase(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("db backup failed", "err", err)
		}
	}
}

// BackupDatabase 立刻备份一次数据库，存进网盘文件夹和两个频道。三处有一处存成功就算成功，没存成的写在记录的 error 里。
func (s *Service) BackupDatabase(ctx context.Context) (*meta.DBBackup, error) {
	b := s.dbBackup
	if b == nil {
		return nil, errors.New("database backup is not enabled")
	}
	if !b.run.TryLock() {
		return nil, ErrDBBackupRunning
	}
	defer b.run.Unlock()
	b.setRunning(true, "")
	rec, err := s.backupDatabase(ctx, b)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b.setRunning(false, msg)
	return rec, err
}

func (b *dbBackups) setRunning(running bool, lastErr string) {
	b.mu.Lock()
	b.running = running
	if !running {
		b.lastErr = lastErr
	}
	b.mu.Unlock()
}

func (s *Service) backupDatabase(ctx context.Context, b *dbBackups) (*meta.DBBackup, error) {
	started := time.Now()
	tmp, err := os.CreateTemp(s.tempDir, "xiaoju-db-*"+dbdump.FileExt)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	info, err := dbdump.WriteBackup(ctx, s.store.Pool(), s.masterKey, io.MultiWriter(tmp, h))
	if err != nil {
		return nil, fmt.Errorf("dump database: %w", err)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	// 先读回来解一遍，确认备份文件是好的再存
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := dbdump.VerifyBackup(tmp, s.masterKey); err != nil {
		return nil, fmt.Errorf("verify backup: %w", err)
	}
	sum := h.Sum(nil)
	name := "xiaoju-db-" + info.Created.In(b.opt.Location).Format("2006-01-02_150405") + dbdump.FileExt
	caption, _ := json.Marshal(DBBackupCaption{Created: info.Created, Size: size, SHA256: hex.EncodeToString(sum), Migration: info.LastMigration()})
	captionText := DBBackupCaptionPrefix + string(caption) + "\n小橘云盘数据库备份（用主密钥加密，不含主密钥）。恢复：xiaoju dbrestore"

	prev, err := s.store.ListDBBackups(ctx, 1)
	if err != nil {
		return nil, err
	}
	rec := &meta.DBBackup{Size: size, SHA256: sum, Migration: info.LastMigration()}
	var problems []string
	body := func() io.Reader { return io.NewSectionReader(tmp, 0, size) }

	// 两个频道：发文件、置顶，取消上一份的置顶（只留最新的一条置顶）
	post := func(d storage.Driver, which string, prevMsg *int64) *int64 {
		if d == nil {
			return nil
		}
		pin, ok := d.(storage.Pinner)
		if !ok {
			return nil // 本地存储（开发用）没有频道
		}
		loc, err := d.Put(ctx, storage.PutRequest{Name: name, Caption: captionText, Body: body(), Size: size})
		if err != nil {
			problems = append(problems, which+": "+err.Error())
			return nil
		}
		if err := pin.Pin(ctx, loc, false); err != nil {
			problems = append(problems, which+" pin: "+err.Error())
		} else if prevMsg != nil && *prevMsg != loc.MessageID {
			if err := pin.Pin(ctx, storage.Location{MessageID: *prevMsg}, true); err != nil {
				s.log.Warn("db backup: unpin previous backup", "channel", which, "err", err)
			}
		}
		id := loc.MessageID
		return &id
	}
	var prevMain, prevBackup *int64
	if len(prev) > 0 {
		prevMain, prevBackup = prev[0].MainMessage, prev[0].BackupMessage
	}
	rec.MainMessage = post(s.driver, "main channel", prevMain)
	if s.backup != nil {
		rec.BackupMessage = post(s.backup, "backup channel", prevBackup)
	}

	// 网盘文件夹
	if f, err := s.saveDBBackupFile(ctx, b.opt.UserID, name, body); err != nil {
		problems = append(problems, "drive folder: "+err.Error())
	} else {
		rec.FileID = &f.ID
	}

	rec.Error = strings.Join(problems, "; ")
	if rec.FileID == nil && rec.MainMessage == nil && rec.BackupMessage == nil {
		return nil, fmt.Errorf("backup not stored anywhere: %s", rec.Error)
	}
	if err := s.store.AddDBBackup(ctx, rec); err != nil {
		return nil, err
	}
	s.log.Info("db backup done", "name", name, "bytes", size, "rows", info.Rows["files"], "took", time.Since(started).Round(time.Second),
		"drive", rec.FileID != nil, "main_channel", rec.MainMessage != nil, "backup_channel", rec.BackupMessage != nil, "problems", rec.Error)
	s.pruneDBBackups(ctx, b)
	if rec.Error != "" {
		return rec, errors.New("backup stored only partly: " + rec.Error)
	}
	return rec, nil
}

// saveDBBackupFile 把备份存进网盘根目录的「数据库备份」文件夹（没有就建）。
func (s *Service) saveDBBackupFile(ctx context.Context, userID uuid.UUID, name string, body func() io.Reader) (*meta.File, error) {
	root, err := s.store.ListChildren(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	var folder *meta.File
	for _, f := range root {
		if f.Type == meta.TypeFolder && f.Name == DBBackupFolder {
			folder = f
			break
		}
	}
	if folder == nil {
		if folder, err = s.store.CreateFolder(ctx, userID, nil, DBBackupFolder); err != nil {
			return nil, err
		}
	}
	// 同一秒备份了两次（手动点了「立即备份」）时名字后面加 -2、-3
	base := strings.TrimSuffix(name, dbdump.FileExt)
	for i := 1; ; i++ {
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", base, i, dbdump.FileExt)
		}
		f, err := s.Upload(ctx, UploadRequest{UserID: userID, ParentID: &folder.ID, Name: name, Mime: "application/octet-stream", Body: body()})
		if !errors.Is(err, meta.ErrConflict) || i >= 10 {
			return f, err
		}
	}
}

// pruneDBBackups 删掉旧的副本：网盘里只留最近 KeepFiles 份，每个频道只留最近 KeepMessages 份；一年前的空记录删掉。
func (s *Service) pruneDBBackups(ctx context.Context, b *dbBackups) {
	list, err := s.store.ListDBBackups(ctx, 1000)
	if err != nil {
		s.log.Warn("db backup prune", "err", err)
		return
	}
	for i, r := range list {
		dropFile := i >= b.opt.KeepFiles && r.FileID != nil
		dropMain := i >= b.opt.KeepMessages && r.MainMessage != nil
		dropBackup := i >= b.opt.KeepMessages && r.BackupMessage != nil && s.backup != nil
		if !dropFile && !dropMain && !dropBackup {
			continue
		}
		if dropFile {
			// 用户自己删了、移进回收站了也算删掉
			if err := s.Purge(ctx, b.opt.UserID, *r.FileID); err != nil && !errors.Is(err, meta.ErrNotFound) {
				s.log.Warn("db backup prune: delete drive file", "err", err)
				dropFile = false
			}
		}
		if dropMain {
			if err := s.driver.Delete(ctx, []storage.Location{{MessageID: *r.MainMessage}}); err != nil {
				s.log.Warn("db backup prune: delete main channel message", "err", err)
				dropMain = false
			}
		}
		if dropBackup {
			if err := s.backup.Delete(ctx, []storage.Location{{MessageID: *r.BackupMessage}}); err != nil {
				s.log.Warn("db backup prune: delete backup channel message", "err", err)
				dropBackup = false
			}
		}
		if err := s.store.ForgetDBBackupCopies(ctx, r.ID, dropFile, dropMain, dropBackup); err != nil {
			s.log.Warn("db backup prune", "err", err)
		}
	}
	if err := s.store.DeleteOldDBBackups(ctx, time.Now().AddDate(-1, 0, 0)); err != nil {
		s.log.Warn("db backup prune", "err", err)
	}
}
