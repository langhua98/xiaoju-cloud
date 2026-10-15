package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UploadKey 是一个只能上传的密钥（不含密钥本身，只存哈希）。
type UploadKey struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Uploads    int64      `json:"uploads"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

const uploadKeyColumns = `id, name, uploads, created_at, last_used_at`

func scanUploadKey(r pgx.Row) (*UploadKey, error) {
	var k UploadKey
	if err := r.Scan(&k.ID, &k.Name, &k.Uploads, &k.CreatedAt, &k.LastUsedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

func (s *Store) CreateUploadKey(ctx context.Context, name string, hash []byte) (*UploadKey, error) {
	return scanUploadKey(s.db.QueryRow(ctx,
		`INSERT INTO upload_keys (name, key_hash) VALUES ($1, $2) RETURNING `+uploadKeyColumns, name, hash))
}

func (s *Store) ListUploadKeys(ctx context.Context) ([]*UploadKey, error) {
	rows, err := s.db.Query(ctx, `SELECT `+uploadKeyColumns+` FROM upload_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*UploadKey{}
	for rows.Next() {
		k, err := scanUploadKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FindUploadKey 按哈希找密钥；不存在（或已作废）时返回 ErrNotFound。
func (s *Store) FindUploadKey(ctx context.Context, hash []byte) (*UploadKey, error) {
	return scanUploadKey(s.db.QueryRow(ctx, `SELECT `+uploadKeyColumns+` FROM upload_keys WHERE key_hash = $1`, hash))
}

// TouchUploadKey 记录一次使用；uploaded 为 true 时上传计数加一。
func (s *Store) TouchUploadKey(ctx context.Context, id uuid.UUID, uploaded bool) error {
	n := 0
	if uploaded {
		n = 1
	}
	_, err := s.db.Exec(ctx, `UPDATE upload_keys SET last_used_at = now(), uploads = uploads + $2 WHERE id = $1`, id, n)
	return err
}

func (s *Store) DeleteUploadKey(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM upload_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindFolder 找目录下同名的（未删除）文件夹；没有时返回 ErrNotFound。
func (s *Store) FindFolder(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND name = $3
		  AND type = 'folder' AND status = 'active' AND trashed_at IS NULL
		LIMIT 1`, userID, parentID, name))
}

// ListFilesBySize 列出目录下大小正好为 size 的文件（判断是否已经传过）。
func (s *Store) ListFilesBySize(ctx context.Context, userID, parentID uuid.UUID, size int64) ([]*File, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id = $2 AND type = 'file' AND size = $3
		  AND status = 'active' AND trashed_at IS NULL`, userID, parentID, size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
