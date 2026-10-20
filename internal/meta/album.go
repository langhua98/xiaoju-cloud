package meta

import (
	"context"
	"time"

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

// FindChild 找目录下同名的（未删除）条目，文件、文件夹都算；没有时返回 ErrNotFound。
func (s *Store) FindChild(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND name = $3
		  AND status = 'active' AND trashed_at IS NULL
		ORDER BY type DESC
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

// NameSize 是一个文件的名字、大小和拍摄时间（生成缩略图时从文件里读出来，不知道时为 nil）。
type NameSize struct {
	ID      uuid.UUID // 文件 ID；别名（Alias）时是内容相同的那个文件
	Name    string
	Size    int64
	TakenAt *time.Time
	Alias   bool // 按内容认出来、没有再存的名字（content_aliases），不是真的文件名
}

// ListNameSizes 列出目录下所有（未删除的）文件的名字、大小和拍摄时间，
// 加上按内容认出来、没有再存的别名（content_aliases，拍摄时间取内容相同的那个文件的）。
func (s *Store) ListNameSizes(ctx context.Context, userID, parentID uuid.UUID) ([]NameSize, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, size, taken_at, false FROM files
		WHERE user_id = $1 AND parent_id = $2 AND type = 'file' AND status = 'active' AND trashed_at IS NULL
		UNION ALL
		SELECT f.id, a.name, a.size, f.taken_at, true FROM content_aliases a JOIN files f ON f.id = a.file_id
		WHERE a.folder_id = $2 AND f.user_id = $1 AND f.status = 'active' AND f.trashed_at IS NULL`, userID, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameSize
	for rows.Next() {
		var f NameSize
		if err := rows.Scan(&f.ID, &f.Name, &f.Size, &f.TakenAt, &f.Alias); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
