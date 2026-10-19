package meta

import (
	"context"
	"encoding/hex"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SameContent 按内容找已经在网盘里的文件：sums 是整个文件明文的 SHA-256，返回 十六进制摘要 → 文件。
// 只算正常的文件（不在回收站里，所在的文件夹也不在）。needPrivate 时只找私密文件夹里的：
// 隐藏相册里的照片不能因为“相册”里有一份就不传，那样它就一直露在外面。同样内容有好几个时优先 prefer 文件夹里的。
func (s *Store) SameContent(ctx context.Context, userID uuid.UUID, sums [][]byte, needPrivate bool, prefer *uuid.UUID) (map[string]*File, error) {
	out := map[string]*File{}
	if len(sums) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id AS at, false AS trashed, false AS private FROM files
			WHERE user_id = $1 AND type = 'file' AND status = 'active' AND trashed_at IS NULL AND sha256 = ANY($2)
			UNION ALL
			SELECT up.id, f.parent_id, f.trashed_at IS NOT NULL, f.private FROM up JOIN files f ON f.id = up.at
		), ok AS (
			SELECT id FROM up GROUP BY id HAVING NOT bool_or(trashed) AND (NOT $3 OR bool_or(private))
		)
		SELECT DISTINCT ON (sha256) `+fileColumns+` FROM files JOIN ok USING (id)
		ORDER BY sha256, (parent_id IS NOT DISTINCT FROM $4) DESC, created_at, id`, userID, sums, needPrivate, prefer)
	if err != nil {
		return nil, err
	}
	files, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*File, error) { return scanFile(r) })
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		out[hex.EncodeToString(f.SHA256)] = f
	}
	return out, nil
}

// InPrivateFolder 报告这个文件夹是不是私密的（它自己或上面任何一层设了私密）；nil（根目录）不是。
func (s *Store) InPrivateFolder(ctx context.Context, userID uuid.UUID, folderID *uuid.UUID) (bool, error) {
	if folderID == nil {
		return false, nil
	}
	var private bool
	err := s.db.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT parent_id, private FROM files WHERE id = $2 AND user_id = $1
			UNION ALL
			SELECT f.parent_id, f.private FROM up JOIN files f ON f.id = up.parent_id
		)
		SELECT COALESCE(bool_or(private), false) FROM up`, userID, *folderID).Scan(&private)
	return private, err
}

// RecordAlias 记下：快捷指令传进 folderID 的这个名字、大小的文件，内容和 fileID 一样，没有再存（见 content_aliases）。
func (s *Store) RecordAlias(ctx context.Context, folderID uuid.UUID, name string, size int64, fileID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO content_aliases (folder_id, name, size, file_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (folder_id, name, size) DO UPDATE SET file_id = EXCLUDED.file_id, created_at = now()`,
		folderID, name, size, fileID)
	return err
}

// ListAliasNames 列出文件夹里大小为 size 的别名（内容相同、没有再存的那些名字）；对应的文件进了回收站的不算。
func (s *Store) ListAliasNames(ctx context.Context, folderID uuid.UUID, size int64) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.name FROM content_aliases a JOIN files f ON f.id = a.file_id
		WHERE a.folder_id = $1 AND a.size = $2 AND f.status = 'active' AND f.trashed_at IS NULL
		ORDER BY a.name`, folderID, size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
