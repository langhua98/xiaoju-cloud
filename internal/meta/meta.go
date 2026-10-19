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
	TakenAt    *time.Time `json:"taken_at,omitempty"` // 拍摄时间（照片、视频里读出来的；没有时为 nil）
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
	// 备份频道中的位置；nil 表示还没有备份。
	BackupChannelID *int64
	BackupMessageID *int64
}

// HasBackup 报告该分块是否已经有备份。
func (p *Part) HasBackup() bool { return p.BackupMessageID != nil }

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

const fileColumns = `id, user_id, parent_id, name, type, size, mime, sha256, dek_wrapped, status, trashed_at, taken_at, created_at, updated_at`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	err := row.Scan(&f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
		&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.TakenAt, &f.CreatedAt, &f.UpdatedAt)
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
		INSERT INTO file_parts (file_id, part_no, storage, channel_id, message_id, size, iv, sha256,
		                        backup_channel_id, backup_message_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		p.FileID, p.PartNo, p.Storage, p.ChannelID, p.MessageID, p.Size, p.IV, p.SHA256,
		p.BackupChannelID, p.BackupMessageID)
	return err
}

// SetPartBackup 记录分块在备份频道中的位置。
func (s *Store) SetPartBackup(ctx context.Context, fileID uuid.UUID, partNo int, channelID, messageID int64) error {
	_, err := s.db.Exec(ctx, `
		UPDATE file_parts SET backup_channel_id = $3, backup_message_id = $4
		WHERE file_id = $1 AND part_no = $2`, fileID, partNo, channelID, messageID)
	return err
}

// SetPartPrimary 更新分块在主存储中的位置（从备份修复主存储后使用）。
func (s *Store) SetPartPrimary(ctx context.Context, fileID uuid.UUID, partNo int, channelID, messageID int64) error {
	_, err := s.db.Exec(ctx, `
		UPDATE file_parts SET channel_id = $3, message_id = $4
		WHERE file_id = $1 AND part_no = $2`, fileID, partNo, channelID, messageID)
	return err
}

// ClearPartBackup 清除分块的备份位置（备份端数据丢失时），后台同步会重新复制。
func (s *Store) ClearPartBackup(ctx context.Context, fileID uuid.UUID, partNo int) error {
	_, err := s.db.Exec(ctx, `
		UPDATE file_parts SET backup_channel_id = NULL, backup_message_id = NULL
		WHERE file_id = $1 AND part_no = $2`, fileID, partNo)
	return err
}

// AllParts 返回所有分块（任何状态的文件，包括上传中和回收站里的），以及所属文件的状态。
func (s *Store) AllParts(ctx context.Context) ([]Part, []string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+partColumns+`, f.status FROM file_parts p JOIN files f ON f.id = p.file_id
		ORDER BY p.file_id, p.part_no`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var parts []Part
	var statuses []string
	for rows.Next() {
		var p Part
		var status string
		if err := rows.Scan(&p.FileID, &p.PartNo, &p.Storage, &p.ChannelID, &p.MessageID, &p.Size, &p.IV, &p.SHA256,
			&p.BackupChannelID, &p.BackupMessageID, &status); err != nil {
			return nil, nil, err
		}
		parts = append(parts, p)
		statuses = append(statuses, status)
	}
	return parts, statuses, rows.Err()
}

// FileExists 报告某个 ID 的文件记录是否存在（任何状态）。
func (s *Store) FileExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM files WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

// ListStaleUploads 返回创建时间早于 before 仍处于上传中的文件（上传被中断，如服务重启）。
func (s *Store) ListStaleUploads(ctx context.Context, before time.Time) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id FROM files WHERE status = 'uploading' AND created_at < $1 ORDER BY created_at`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ListPartsMissingBackup 返回最早的一批还没有备份的分块（只看已上传完成的文件）。
func (s *Store) ListPartsMissingBackup(ctx context.Context, limit int) ([]Part, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+partColumns+` FROM file_parts p
		JOIN files f ON f.id = p.file_id AND f.status = 'active'
		WHERE p.backup_message_id IS NULL
		ORDER BY p.created_at, p.file_id, p.part_no
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPart)
}

// DriveStats 是整个网盘的文件统计，不含回收站里的条目及被移入回收站的文件夹里的内容。
type DriveStats struct {
	Folders       int64 `json:"folders"`
	Files         int64 `json:"files"`
	Photos        int64 `json:"photos"`
	Videos        int64 `json:"videos"`
	Others        int64 `json:"others"`
	Bytes         int64 `json:"bytes"`
	PhotoBytes    int64 `json:"photo_bytes"`
	VideoBytes    int64 `json:"video_bytes"`
	BackedUpFiles int64 `json:"backed_up_files"` // 所有分块都已备份的文件数（空文件算已备份）
}

// 按 MIME 类型识别，类型缺失或不准时（如 curl 上传、部分手机格式）再按扩展名识别
const (
	isPhotoSQL = `(mime LIKE 'image/%' OR name ~* '\.(jpe?g|png|gif|webp|heic|heif|bmp|avif|tiff?|dng|raw)$')`
	isVideoSQL = `(mime LIKE 'video/%' OR name ~* '\.(mp4|mov|m4v|avi|mkv|webm|3gp|wmv|flv|mts)$')`
)

// FolderCounts 是一个文件夹里（连同子文件夹）的照片、视频、其他文件各有几个。
type FolderCounts struct {
	Photos int64 `json:"photos"`
	Videos int64 `json:"videos"`
	Others int64 `json:"others"`
}

// CountInFolders 统计每个文件夹里（递归）的照片、视频和其他文件数，跳过回收站里的条目及其子项。
// 照片、视频的判断和 DriveStats 一样。没有文件的文件夹不在结果里。
func (s *Store) CountInFolders(ctx context.Context, userID uuid.UUID, folderIDs []uuid.UUID) (map[uuid.UUID]FolderCounts, error) {
	out := map[uuid.UUID]FolderCounts{}
	if len(folderIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT c.parent_id AS root, c.id, c.type, c.mime, c.name FROM files c
			WHERE c.user_id = $1 AND c.parent_id = ANY($2) AND c.status = 'active' AND c.trashed_at IS NULL
			UNION ALL
			SELECT t.root, c.id, c.type, c.mime, c.name FROM files c
			JOIN tree t ON c.parent_id = t.id
			WHERE c.status = 'active' AND c.trashed_at IS NULL
		), kinds AS (
			SELECT root, `+isPhotoSQL+` AS photo, NOT `+isPhotoSQL+` AND `+isVideoSQL+` AS video
			FROM tree WHERE type = 'file'
		)
		SELECT root, count(*) FILTER (WHERE photo), count(*) FILTER (WHERE video), count(*) FILTER (WHERE NOT photo AND NOT video)
		FROM kinds GROUP BY root`, userID, folderIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c FolderCounts
		if err := rows.Scan(&id, &c.Photos, &c.Videos, &c.Others); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// DriveStats 统计用户网盘里的文件数量和大小。从根目录递归，跳过回收站里的条目及其子项。
func (s *Store) DriveStats(ctx context.Context, userID uuid.UUID) (DriveStats, error) {
	return s.FolderStats(ctx, userID, nil)
}

// FolderStats 统计一个文件夹里（连同子文件夹，不算它自己）的文件数量和大小；folderID 为 nil 时统计整个网盘。
// 文件夹不存在或在回收站里时返回 ErrNotFound。
func (s *Store) FolderStats(ctx context.Context, userID uuid.UUID, folderID *uuid.UUID) (DriveStats, error) {
	var st DriveStats
	if err := s.checkParent(ctx, userID, folderID); err != nil {
		return st, err
	}
	err := s.db.QueryRow(ctx, `
		WITH RECURSIVE live AS (
			SELECT id, type, mime, name, size FROM files
			WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND status = 'active' AND trashed_at IS NULL
			UNION ALL
			SELECT c.id, c.type, c.mime, c.name, c.size FROM files c
			JOIN live p ON c.parent_id = p.id
			WHERE c.status = 'active' AND c.trashed_at IS NULL
		), kinds AS (
			SELECT *, type = 'file' AND `+isPhotoSQL+` AS photo,
			          type = 'file' AND NOT `+isPhotoSQL+` AND `+isVideoSQL+` AS video
			FROM live
		)
		SELECT count(*) FILTER (WHERE type = 'folder'),
		       count(*) FILTER (WHERE type = 'file'),
		       count(*) FILTER (WHERE photo),
		       count(*) FILTER (WHERE video),
		       COALESCE(sum(size) FILTER (WHERE type = 'file'), 0),
		       COALESCE(sum(size) FILTER (WHERE photo), 0),
		       COALESCE(sum(size) FILTER (WHERE video), 0),
		       count(*) FILTER (WHERE type = 'file' AND NOT EXISTS (
		           SELECT 1 FROM file_parts p WHERE p.file_id = kinds.id AND p.backup_message_id IS NULL))
		FROM kinds`, userID, folderID).
		Scan(&st.Folders, &st.Files, &st.Photos, &st.Videos, &st.Bytes, &st.PhotoBytes, &st.VideoBytes, &st.BackedUpFiles)
	st.Others = st.Files - st.Photos - st.Videos
	return st, err
}

type BackupStats struct {
	Parts       int64 `json:"parts"`
	BackedUp    int64 `json:"backed_up"`
	Bytes       int64 `json:"bytes"`
	BackedBytes int64 `json:"backed_up_bytes"`
}

// BackupStats 统计已上传完成文件的分块备份进度。
func (s *Store) BackupStats(ctx context.Context) (BackupStats, error) {
	var st BackupStats
	err := s.db.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE p.backup_message_id IS NOT NULL),
		       COALESCE(sum(p.size), 0),
		       COALESCE(sum(p.size) FILTER (WHERE p.backup_message_id IS NOT NULL), 0)
		FROM file_parts p JOIN files f ON f.id = p.file_id AND f.status = 'active'`).
		Scan(&st.Parts, &st.BackedUp, &st.Bytes, &st.BackedBytes)
	return st, err
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

// ListChildren 列出目录下未删除的条目：目录在前、按名字排；文件按拍摄时间（没有时用上传时间）排，新的在前。
func (s *Store) ListChildren(ctx context.Context, userID uuid.UUID, parentID *uuid.UUID) ([]*File, error) {
	if err := s.checkParent(ctx, userID, parentID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE user_id = $1 AND parent_id IS NOT DISTINCT FROM $2
		  AND status = 'active' AND trashed_at IS NULL
		ORDER BY type DESC, CASE WHEN type = 'folder' THEN name END, COALESCE(taken_at, created_at) DESC, name`, userID, parentID)
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

const partColumns = `p.file_id, p.part_no, p.storage, p.channel_id, p.message_id, p.size, p.iv, p.sha256,
	p.backup_channel_id, p.backup_message_id`

func scanPart(r pgx.CollectableRow) (Part, error) {
	var p Part
	err := r.Scan(&p.FileID, &p.PartNo, &p.Storage, &p.ChannelID, &p.MessageID, &p.Size, &p.IV, &p.SHA256,
		&p.BackupChannelID, &p.BackupMessageID)
	return p, err
}

func (s *Store) ListParts(ctx context.Context, fileID uuid.UUID) ([]Part, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+partColumns+` FROM file_parts p WHERE p.file_id = $1 ORDER BY p.part_no`, fileID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPart)
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
