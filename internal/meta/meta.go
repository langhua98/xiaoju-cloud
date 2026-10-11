// Package meta 是元数据仓储层：目录树、文件、分块映射都在 PostgreSQL 里。
package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("name already exists in this folder")
)

const (
	TypeFile   = "file"
	TypeFolder = "folder"

	StatusUploading = "uploading"
	StatusActive    = "active"
)

type File struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"-"`
	ParentID   *uuid.UUID `json:"parent_id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Size       int64      `json:"size"`
	Mime       string     `json:"mime"`
	SHA256     []byte     `json:"-"`
	DEKWrapped []byte     `json:"-"`
	Status     string     `json:"-"`
	TrashedAt  *time.Time `json:"trashed_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type Part struct {
	FileID    uuid.UUID
	PartNo    int
	Storage   string
	ChannelID int64
	MessageID int64
	Size      int64
	IV        []byte
	SHA256    []byte
}

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

const fileColumns = `id, user_id, parent_id, name, type, size, mime, sha256, dek_wrapped, status, trashed_at, created_at, updated_at`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	err := row.Scan(&f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
		&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

// EnsureUser 返回指定用户名的用户 ID，不存在则创建。
func (s *Store) EnsureUser(ctx context.Context, username string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO users (username) VALUES ($1)
		ON CONFLICT (username) DO UPDATE SET username = EXCLUDED.username
		RETURNING id`, username).Scan(&id)
	return id, err
}

// CreateFolder 创建目录。parentID 为 nil 表示根目录。
func (s *Store) CreateFolder(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (*File, error) {
	if err := s.checkParent(ctx, userID, parentID); err != nil {
		return nil, err
	}
	f, err := scanFile(s.db.QueryRow(ctx, `
		INSERT INTO files (user_id, parent_id, name, type) VALUES ($1, $2, $3, 'folder')
		RETURNING `+fileColumns, userID, parentID, name))
	return f, mapErr(err)
}

// CreateUpload 创建一个 uploading 状态的文件记录，上传完成后调用 ActivateFile。
func (s *Store) CreateUpload(ctx context.Context, f *File) (*File, error) {
	if err := s.checkParent(ctx, f.UserID, f.ParentID); err != nil {
		return nil, err
	}
	if exists, err := s.nameExists(ctx, f.UserID, f.ParentID, f.Name); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrConflict
	}
	out, err := scanFile(s.db.QueryRow(ctx, `
		INSERT INTO files (user_id, parent_id, name, type, mime, dek_wrapped, status)
		VALUES ($1, $2, $3, 'file', $4, $5, 'uploading')
		RETURNING `+fileColumns, f.UserID, f.ParentID, f.Name, f.Mime, f.DEKWrapped))
	return out, mapErr(err)
}

func (s *Store) AddPart(ctx context.Context, p *Part) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO file_parts (file_id, part_no, storage, channel_id, message_id, size, iv, sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.FileID, p.PartNo, p.Storage, p.ChannelID, p.MessageID, p.Size, p.IV, p.SHA256)
	return err
}

func (s *Store) ActivateFile(ctx context.Context, id uuid.UUID, size int64, sha []byte) (*File, error) {
	f, err := scanFile(s.db.QueryRow(ctx, `
		UPDATE files SET status = 'active', size = $2, sha256 = $3, updated_at = now()
		WHERE id = $1 RETURNING `+fileColumns, id, size, sha))
	return f, mapErr(err)
}

// DeleteRow 直接删除记录（分块记录级联删除）。调用方需先清理存储端数据。
func (s *Store) DeleteRow(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM files WHERE id = $1`, id)
	return err
}

// Get 返回用户的一个正常（非上传中）条目，包括回收站中的。
func (s *Store) Get(ctx context.Context, userID, id uuid.UUID) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE id = $1 AND user_id = $2 AND status = 'active'`, id, userID))
}

// ListChildren 列出目录下未删除的条目，目录在前。
func (s *Store) ListChildren(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID) ([]*File, error) {
	if err := s.checkParent(ctx, userID, parentID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2
		  AND status = 'active' AND trashed_at IS NULL
		ORDER BY type DESC, name`, userID, parentID)
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

// FindFolder 返回目录下指定名称的未删除子目录。
func (s *Store) FindFolder(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (*File, error) {
	return scanFile(s.db.QueryRow(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND name = $3
		  AND type = 'folder' AND status = 'active' AND trashed_at IS NULL`, userID, parentID, name))
}

// StemExists 判断目录下是否已有主文件名（去掉扩展名）为 stem 的文件，回收站中的也算，
// 这样自动同步不会把用户删掉的照片再传回来。
func (s *Store) StemExists(ctx context.Context, userID, parentID uuid.UUID, stem string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM files WHERE user_id = $1 AND parent_id = $2
		  AND type = 'file' AND status = 'active'
		  AND (name = $3 OR left(name, length($3) + 1) = $3 || '.'))`, userID, parentID, stem).Scan(&ok)
	return ok, err
}

func (s *Store) ListParts(ctx context.Context, fileID uuid.UUID) ([]Part, error) {
	rows, err := s.db.Query(ctx, `
		SELECT file_id, part_no, storage, channel_id, message_id, size, iv, sha256
		FROM file_parts WHERE file_id = $1 ORDER BY part_no`, fileID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Part, error) {
		var p Part
		err := r.Scan(&p.FileID, &p.PartNo, &p.Storage, &p.ChannelID, &p.MessageID, &p.Size, &p.IV, &p.SHA256)
		return p, err
	})
}

// Trash 把条目移入回收站（软删除）。
func (s *Store) Trash(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE files SET trashed_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status = 'active' AND trashed_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) checkParent(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID) error {
	if parentID == nil {
		return nil
	}
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM files WHERE id = $1 AND user_id = $2
		  AND type = 'folder' AND status = 'active' AND trashed_at IS NULL)`, *parentID, userID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func (s *Store) nameExists(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID, name string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM files WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2
		  AND name = $3 AND status = 'active' AND trashed_at IS NULL)`, userID, parentID, name).Scan(&ok)
	return ok, err
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
