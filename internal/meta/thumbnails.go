package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ThumbnailVersion 是现在的程序生成的缩略图版本：1 起生成时也读拍摄时间。
// 旧版本的缩略图照样能用，服务启动后在后台重新生成（BackfillThumbnails）。
const ThumbnailVersion = 1

// Thumbnail 是一个文件的缩略图。Data 是加密后的 JPEG。
type Thumbnail struct {
	FileID   uuid.UUID
	Data     []byte
	Duration time.Duration
	Version  int
	Taken    *time.Time // 只写：生成时读出的拍摄时间，存进 files.taken_at
}

func (s *Store) GetThumbnail(ctx context.Context, fileID uuid.UUID) (*Thumbnail, error) {
	t := &Thumbnail{FileID: fileID}
	var ms int64
	err := s.db.QueryRow(ctx, `SELECT data, duration_ms, version FROM thumbnails WHERE file_id = $1`, fileID).Scan(&t.Data, &ms, &t.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Duration = time.Duration(ms) * time.Millisecond
	return t, nil
}

// PutThumbnail 保存缩略图（已有的覆盖），版本记为 ThumbnailVersion，同时记下拍摄时间。
// 文件里读不到拍摄时间（微信存的图、没有 EXIF 的截图）时保留原来的：手机上传时报的照片创建时间（FillTakenAt）。
// 文件在这期间被永久删除时什么也不做。
func (s *Store) PutThumbnail(ctx context.Context, t *Thumbnail) error {
	_, err := s.db.Exec(ctx, `
		WITH f AS (UPDATE files SET taken_at = COALESCE($4, taken_at) WHERE id = $1 RETURNING id)
		INSERT INTO thumbnails (file_id, data, duration_ms, version)
		SELECT id, $2, $3, $5 FROM f
		ON CONFLICT (file_id) DO UPDATE SET data = EXCLUDED.data, duration_ms = EXCLUDED.duration_ms,
		                                    version = EXCLUDED.version, created_at = now()`,
		t.FileID, t.Data, t.Duration.Milliseconds(), t.Taken, ThumbnailVersion)
	return err
}

// FillTakenAt 给还不知道拍摄时间的文件记上手机报的时间（照片在手机相册里的创建时间）。已经有的不改：
// 文件里读出来的 EXIF 优先。返回改了几个。
func (s *Store) FillTakenAt(ctx context.Context, ids []uuid.UUID, times []time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE files f SET taken_at = v.t
		FROM unnest($1::uuid[], $2::timestamptz[]) AS v(id, t)
		WHERE f.id = v.id AND f.taken_at IS NULL`, ids, times)
	return tag.RowsAffected(), err
}

// ListThumbnailBackfill 按上传先后列出需要（重新）生成缩略图的照片、视频：还没有缩略图，或者是旧版本生成的。
// 从 (afterCreated, afterID) 之后开始，最多 limit 个。
func (s *Store) ListThumbnailBackfill(ctx context.Context, afterCreated time.Time, afterID uuid.UUID, limit int) ([]*File, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+fileColumns+` FROM files f
		WHERE type = 'file' AND status = 'active' AND trashed_at IS NULL
		  AND (mime LIKE 'image/%' OR mime LIKE 'video/%')
		  AND NOT EXISTS (SELECT 1 FROM thumbnails t WHERE t.file_id = f.id AND t.version >= $1)
		  AND (created_at, id) > ($2, $3)
		ORDER BY created_at, id
		LIMIT $4`, ThumbnailVersion, afterCreated, afterID, limit)
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
