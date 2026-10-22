package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DropLink 是一个上传链接（见 drop_links 表）：知道链接的人能往 Folder 里传文件。
type DropLink struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"-"`
	FolderID   uuid.UUID  `json:"folder_id"`
	Name       string     `json:"name"`
	Token      string     `json:"token"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Uploads    int64      `json:"uploads"`
	Bytes      int64      `json:"bytes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	Folder     *File      `json:"folder,omitempty"`
}

// Expired 报告链接是不是过期了。
func (d *DropLink) Expired(now time.Time) bool { return d.ExpiresAt != nil && now.After(*d.ExpiresAt) }

const dropColumns = `d.id, d.user_id, d.folder_id, d.name, d.token, d.expires_at, d.uploads, d.bytes, d.last_used_at, d.created_at`

func scanDropRow(row pgx.Row) (*DropLink, error) {
	var d DropLink
	var f File
	err := row.Scan(&d.ID, &d.UserID, &d.FolderID, &d.Name, &d.Token, &d.ExpiresAt, &d.Uploads, &d.Bytes, &d.LastUsedAt, &d.CreatedAt,
		&f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
		&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.TakenAt, &f.Private, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Folder = &f
	return &d, nil
}

// CreateDropLink 建一个上传链接。FolderID 必须是用户的文件夹。
func (s *Store) CreateDropLink(ctx context.Context, d *DropLink) (*DropLink, error) {
	f, err := s.Get(ctx, d.UserID, d.FolderID)
	if err != nil {
		return nil, err
	}
	if f.Type != TypeFolder {
		return nil, ErrNotFolder
	}
	var id uuid.UUID
	if err := s.db.QueryRow(ctx, `
		INSERT INTO drop_links (user_id, folder_id, name, token, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		d.UserID, d.FolderID, d.Name, d.Token, d.ExpiresAt).Scan(&id); err != nil {
		return nil, err
	}
	return s.getDropLink(ctx, `d.id = $1`, id)
}

func (s *Store) getDropLink(ctx context.Context, where string, arg any) (*DropLink, error) {
	return scanDropRow(s.db.QueryRow(ctx, `
		SELECT `+dropColumns+`, `+prefixed("f", fileColumns)+`
		FROM drop_links d JOIN files f ON f.id = d.folder_id
		WHERE `+where, arg))
}

// DropLinkByToken 按链接里的随机串找上传链接（不管过没过期、文件夹在不在回收站，调用方判断）。
func (s *Store) DropLinkByToken(ctx context.Context, token string) (*DropLink, error) {
	return s.getDropLink(ctx, `d.token = $1`, token)
}

// ListDropLinks 列出用户的上传链接，最近建的在前。
func (s *Store) ListDropLinks(ctx context.Context, userID uuid.UUID) ([]*DropLink, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+dropColumns+`, `+prefixed("f", fileColumns)+`
		FROM drop_links d JOIN files f ON f.id = d.folder_id
		WHERE d.user_id = $1 ORDER BY d.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DropLink
	for rows.Next() {
		d, err := scanDropRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDropLink 撤销一个上传链接。
func (s *Store) DeleteDropLink(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM drop_links WHERE id = $1 AND user_id = $2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

// CountDropUpload 记一次通过链接上传。
func (s *Store) CountDropUpload(ctx context.Context, id uuid.UUID, size int64) error {
	_, err := s.db.Exec(ctx, `UPDATE drop_links SET uploads = uploads + 1, bytes = bytes + $2, last_used_at = now() WHERE id = $1`, id, size)
	return err
}
