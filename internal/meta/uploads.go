package meta

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UploadSession 是一个断点续传中的上传（见 upload_sessions）。文件本身是 status = 'uploading' 的那条记录。
type UploadSession struct {
	FileID    uuid.UUID
	Size      int64
	Probe     string
	Dedupe    bool
	Committed int64
	HashState []byte
	UpdatedAt time.Time
}

const sessionColumns = `u.file_id, u.size, u.probe, u.dedupe, u.committed, u.hash_state, u.updated_at`

func scanSession(row pgx.Row, f *File) (*UploadSession, error) {
	var u UploadSession
	dest := []any{&u.FileID, &u.Size, &u.Probe, &u.Dedupe, &u.Committed, &u.HashState, &u.UpdatedAt}
	if f != nil {
		dest = append(dest, &f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
			&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.TakenAt, &f.Private, &f.CreatedAt, &f.UpdatedAt)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// CreateUploadSession 给刚建好的上传中文件记录（CreateUpload）开一个断点续传会话。
func (s *Store) CreateUploadSession(ctx context.Context, u *UploadSession) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO upload_sessions (file_id, size, probe, dedupe) VALUES ($1, $2, $3, $4)`,
		u.FileID, u.Size, u.Probe, u.Dedupe)
	return err
}

// FindUploadSession 找同一个文件夹里、同名同大小的未完成上传（最近动过的那个）。
func (s *Store) FindUploadSession(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string, size int64) (*UploadSession, *File, error) {
	var f File
	u, err := scanSession(s.db.QueryRow(ctx, `
		SELECT `+sessionColumns+`, `+prefixed("f", fileColumns)+`
		FROM upload_sessions u JOIN files f ON f.id = u.file_id
		WHERE f.user_id = $1 AND f.parent_id IS NOT DISTINCT FROM $2 AND f.name = $3 AND u.size = $4 AND f.status = 'uploading'
		ORDER BY u.updated_at DESC LIMIT 1`, userID, parentID, name, size), &f)
	if err != nil {
		return nil, nil, err
	}
	return u, &f, nil
}

// GetUploadSession 返回用户的一个未完成上传和它的文件记录。
func (s *Store) GetUploadSession(ctx context.Context, userID, fileID uuid.UUID) (*UploadSession, *File, error) {
	var f File
	u, err := scanSession(s.db.QueryRow(ctx, `
		SELECT `+sessionColumns+`, `+prefixed("f", fileColumns)+`
		FROM upload_sessions u JOIN files f ON f.id = u.file_id
		WHERE u.file_id = $2 AND f.user_id = $1 AND f.status = 'uploading'`, userID, fileID), &f)
	if err != nil {
		return nil, nil, err
	}
	return u, &f, nil
}

// CommitUploadPart 记下又一个写好的分块，以及读到 committed 为止的 SHA-256 中间状态（同一个事务）。
func (s *Store) CommitUploadPart(ctx context.Context, p *Part, committed int64, hashState []byte) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO file_parts (file_id, part_no, storage, channel_id, message_id, size, iv, sha256,
			                        backup_channel_id, backup_message_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			p.FileID, p.PartNo, p.Storage, p.ChannelID, p.MessageID, p.Size, p.IV, p.SHA256,
			p.BackupChannelID, p.BackupMessageID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE upload_sessions SET committed = $2, hash_state = $3, updated_at = now() WHERE file_id = $1`,
			p.FileID, committed, hashState)
		if err == nil && tag.RowsAffected() == 0 {
			err = ErrNotFound // 会话已经被清理了
		}
		return err
	})
}

// TouchUploadSession 更新会话的最后活动时间（每次收到数据时调用；24 小时没动静的会被清理）。
func (s *Store) TouchUploadSession(ctx context.Context, fileID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE upload_sessions SET updated_at = now() WHERE file_id = $1`, fileID)
	return err
}

// FinishUpload 激活传完的文件，删掉会话。同一个文件夹里已经有同名的返回 ErrConflict（文件仍是上传中）。
func (s *Store) FinishUpload(ctx context.Context, id uuid.UUID, size int64, sha []byte) (*File, error) {
	var out *File
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		f, err := scanFile(tx.QueryRow(ctx, `
			UPDATE files SET status = 'active', size = $2, sha256 = $3, updated_at = now()
			WHERE id = $1 AND status = 'uploading' RETURNING `+fileColumns, id, size, sha))
		if err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM upload_sessions WHERE file_id = $1`, id); err != nil {
			return err
		}
		out = f
		return nil
	})
	return out, err
}

// prefixed 给逗号分隔的列名加上表别名：prefixed("f", "id, name") == "f.id, f.name"。
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
