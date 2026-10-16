package meta

import (
	"context"

	"github.com/google/uuid"
)

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

// RenameFile 给文件改名（同一文件夹里已有同名的返回 ErrConflict）。
func (s *Store) RenameFile(ctx context.Context, userID, id uuid.UUID, name string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE files SET name = $3, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND type = 'file' AND status = 'active' AND trashed_at IS NULL`, id, userID, name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFileNames 列出目录下所有（未删除的）文件的名字。
func (s *Store) ListFileNames(ctx context.Context, userID, parentID uuid.UUID) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT name FROM files
		WHERE user_id = $1 AND parent_id = $2 AND type = 'file' AND status = 'active' AND trashed_at IS NULL`, userID, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FindFileByName 找目录下同名（不分大小写）的文件；没有时返回 ErrNotFound。
func (s *Store) FindFileByName(ctx context.Context, userID, parentID uuid.UUID, name string) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id = $2 AND lower(name) = lower($3)
		  AND type = 'file' AND status = 'active' AND trashed_at IS NULL
		LIMIT 1`, userID, parentID, name))
}
