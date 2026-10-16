package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Thumbnail 是一个文件的缩略图。Data 是加密后的 JPEG。
type Thumbnail struct {
	FileID   uuid.UUID
	Data     []byte
	Duration time.Duration
}

func (s *Store) GetThumbnail(ctx context.Context, fileID uuid.UUID) (*Thumbnail, error) {
	t := &Thumbnail{FileID: fileID}
	var ms int64
	err := s.db.QueryRow(ctx, `SELECT data, duration_ms FROM thumbnails WHERE file_id = $1`, fileID).Scan(&t.Data, &ms)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Duration = time.Duration(ms) * time.Millisecond
	return t, nil
}

// PutThumbnail 保存缩略图（已有的覆盖）。文件在这期间被永久删除时什么也不做。
func (s *Store) PutThumbnail(ctx context.Context, t *Thumbnail) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO thumbnails (file_id, data, duration_ms)
		SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM files WHERE id = $1)
		ON CONFLICT (file_id) DO UPDATE SET data = EXCLUDED.data, duration_ms = EXCLUDED.duration_ms, created_at = now()`,
		t.FileID, t.Data, t.Duration.Milliseconds())
	return err
}

// ThumbnailDurations 返回这些文件里已经生成了缩略图的，以及各自的视频时长（照片为 0）。
func (s *Store) ThumbnailDurations(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Duration, error) {
	out := make(map[uuid.UUID]time.Duration)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT file_id, duration_ms FROM thumbnails WHERE file_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var ms int64
		if err := rows.Scan(&id, &ms); err != nil {
			return nil, err
		}
		out[id] = time.Duration(ms) * time.Millisecond
	}
	return out, rows.Err()
}
