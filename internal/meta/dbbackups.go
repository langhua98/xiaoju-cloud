package meta

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool 返回数据库连接池（导出整个数据库用，见 dbdump）。
func (s *Store) Pool() *pgxpool.Pool { return s.db }

// DBBackup 是一次数据库备份（db_backups 的一行）。
type DBBackup struct {
	ID            uuid.UUID  `json:"id"`
	CreatedAt     time.Time  `json:"created_at"`
	Size          int64      `json:"size"`
	SHA256        []byte     `json:"-"`
	Migration     string     `json:"migration"`
	FileID        *uuid.UUID `json:"file_id,omitempty"`
	MainMessage   *int64     `json:"main_message,omitempty"`
	BackupMessage *int64     `json:"backup_message,omitempty"`
	Error         string     `json:"error,omitempty"`
}

const dbBackupColumns = `id, created_at, size, sha256, migration, file_id, main_message, backup_message, error`

func (s *Store) AddDBBackup(ctx context.Context, b *DBBackup) error {
	return s.db.QueryRow(ctx, `INSERT INTO db_backups (size, sha256, migration, file_id, main_message, backup_message, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`,
		b.Size, b.SHA256, b.Migration, b.FileID, b.MainMessage, b.BackupMessage, b.Error).Scan(&b.ID, &b.CreatedAt)
}

// ListDBBackups 返回最近的备份，新的在前。
func (s *Store) ListDBBackups(ctx context.Context, limit int) ([]DBBackup, error) {
	rows, err := s.db.Query(ctx, `SELECT `+dbBackupColumns+` FROM db_backups ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DBBackup, error) {
		var b DBBackup
		err := r.Scan(&b.ID, &b.CreatedAt, &b.Size, &b.SHA256, &b.Migration, &b.FileID, &b.MainMessage, &b.BackupMessage, &b.Error)
		return b, err
	})
}

// ForgetDBBackupCopies 记下这次备份在网盘、两个频道里的副本已经删了。
func (s *Store) ForgetDBBackupCopies(ctx context.Context, id uuid.UUID, file, main, backup bool) error {
	_, err := s.db.Exec(ctx, `UPDATE db_backups SET
		file_id = CASE WHEN $2 THEN NULL ELSE file_id END,
		main_message = CASE WHEN $3 THEN NULL ELSE main_message END,
		backup_message = CASE WHEN $4 THEN NULL ELSE backup_message END
		WHERE id = $1`, id, file, main, backup)
	return err
}

// DeleteOldDBBackups 删掉 before 之前、已经没有任何副本的记录。
func (s *Store) DeleteOldDBBackups(ctx context.Context, before time.Time) error {
	_, err := s.db.Exec(ctx, `DELETE FROM db_backups WHERE created_at < $1
		AND file_id IS NULL AND main_message IS NULL AND backup_message IS NULL`, before)
	return err
}
