package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 谷歌云盘同步的状态（见 internal/gdrive 和迁移 0018）。

// GDriveAccount 是连接的谷歌账号。RefreshSealed 是用主密钥加密的 refresh token。
type GDriveAccount struct {
	UserID        uuid.UUID
	Email         string
	RefreshSealed []byte
	PageToken     string
	Error         string
	CreatedAt     time.Time
}

func (s *Store) GetGDriveAccount(ctx context.Context, userID uuid.UUID) (*GDriveAccount, error) {
	var a GDriveAccount
	err := s.db.QueryRow(ctx, `
		SELECT user_id, email, refresh_sealed, page_token, error, created_at FROM gdrive_accounts WHERE user_id = $1`, userID).
		Scan(&a.UserID, &a.Email, &a.RefreshSealed, &a.PageToken, &a.Error, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// SaveGDriveAccount 记下（或换掉）谷歌账号的授权；换了授权就从头看变化。
func (s *Store) SaveGDriveAccount(ctx context.Context, userID uuid.UUID, email string, sealed []byte) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO gdrive_accounts (user_id, email, refresh_sealed) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET email = EXCLUDED.email, refresh_sealed = EXCLUDED.refresh_sealed,
		  page_token = '', error = '', created_at = now()`, userID, email, sealed)
	return err
}

func (s *Store) SetGDrivePageToken(ctx context.Context, userID uuid.UUID, token string) error {
	_, err := s.db.Exec(ctx, `UPDATE gdrive_accounts SET page_token = $2 WHERE user_id = $1`, userID, token)
	return err
}

func (s *Store) SetGDriveAccountError(ctx context.Context, userID uuid.UUID, msg string) error {
	_, err := s.db.Exec(ctx, `UPDATE gdrive_accounts SET error = $2 WHERE user_id = $1`, userID, msg)
	return err
}

// DeleteGDriveAccount 断开谷歌账号，连同所有同步设置（两边的文件都不动）。
func (s *Store) DeleteGDriveAccount(ctx context.Context, userID uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM gdrive_links WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM gdrive_accounts WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GDriveLink 是一对同步的文件夹。
type GDriveLink struct {
	ID          uuid.UUID       `json:"id"`
	UserID      uuid.UUID       `json:"-"`
	GDriveID    string          `json:"gdrive_id"`
	GDriveName  string          `json:"gdrive_name"`
	FolderID    uuid.UUID       `json:"folder_id"`
	Paused      bool            `json:"paused"`
	LastSyncAt  *time.Time      `json:"last_sync_at"`
	LastError   string          `json:"last_error"`
	LastResult  json.RawMessage `json:"last_result"`
	Fingerprint string          `json:"-"`
	CreatedAt   time.Time       `json:"created_at"`
}

const linkColumns = `id, user_id, gdrive_id, gdrive_name, folder_id, paused, last_sync_at, last_error, last_result, fingerprint, created_at`

func scanLink(r pgx.Row) (*GDriveLink, error) {
	var l GDriveLink
	err := r.Scan(&l.ID, &l.UserID, &l.GDriveID, &l.GDriveName, &l.FolderID, &l.Paused, &l.LastSyncAt, &l.LastError,
		&l.LastResult, &l.Fingerprint, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &l, mapErr(err)
}

func (s *Store) CreateGDriveLink(ctx context.Context, userID uuid.UUID, gdriveID, gdriveName string, folderID uuid.UUID) (*GDriveLink, error) {
	return scanLink(s.db.QueryRow(ctx, `
		INSERT INTO gdrive_links (user_id, gdrive_id, gdrive_name, folder_id) VALUES ($1, $2, $3, $4)
		RETURNING `+linkColumns, userID, gdriveID, gdriveName, folderID))
}

func (s *Store) ListGDriveLinks(ctx context.Context, userID uuid.UUID) ([]*GDriveLink, error) {
	rows, err := s.db.Query(ctx, `SELECT `+linkColumns+` FROM gdrive_links WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GDriveLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetGDriveLink(ctx context.Context, userID, id uuid.UUID) (*GDriveLink, error) {
	return scanLink(s.db.QueryRow(ctx, `SELECT `+linkColumns+` FROM gdrive_links WHERE id = $1 AND user_id = $2`, id, userID))
}

func (s *Store) DeleteGDriveLink(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM gdrive_links WHERE id = $1 AND user_id = $2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

func (s *Store) SetGDriveLinkPaused(ctx context.Context, userID, id uuid.UUID, paused bool, msg string) error {
	tag, err := s.db.Exec(ctx, `UPDATE gdrive_links SET paused = $3, last_error = $4 WHERE id = $1 AND user_id = $2`, id, userID, paused, msg)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

// FinishGDriveSync 记下这次同步的结果。fingerprint 为空时不改（出错的时候下次还要重新对）。
func (s *Store) FinishGDriveSync(ctx context.Context, id uuid.UUID, result any, syncErr, fingerprint string) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		UPDATE gdrive_links SET last_sync_at = now(), last_result = $2, last_error = $3,
		  fingerprint = CASE WHEN $4 = '' THEN fingerprint ELSE $4 END
		WHERE id = $1`, id, b, syncErr, fingerprint)
	return err
}

// ResetGDriveFingerprint 让下次一定整个对一遍。
func (s *Store) ResetGDriveFingerprint(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE gdrive_links SET fingerprint = '' WHERE id = $1`, id)
	return err
}

// GDriveItem 是上次同步完时两边对应的一个条目。
type GDriveItem struct {
	GDriveID string
	FileID   uuid.UUID
	IsFolder bool
	GName    string
	GParent  string
	GMD5     string
	XName    string
	XParent  uuid.UUID
	XSHA256  []byte
}

func (s *Store) ListGDriveItems(ctx context.Context, linkID uuid.UUID) ([]*GDriveItem, error) {
	rows, err := s.db.Query(ctx, `
		SELECT gdrive_id, file_id, is_folder, g_name, g_parent, g_md5, x_name, x_parent, x_sha256
		FROM gdrive_items WHERE link_id = $1`, linkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GDriveItem
	for rows.Next() {
		var it GDriveItem
		if err := rows.Scan(&it.GDriveID, &it.FileID, &it.IsFolder, &it.GName, &it.GParent, &it.GMD5, &it.XName, &it.XParent, &it.XSHA256); err != nil {
			return nil, err
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// PutGDriveItem 记下（或更新）一对条目。同一个网盘文件以前对应别的谷歌文件时，先把旧的那条删掉。
func (s *Store) PutGDriveItem(ctx context.Context, linkID uuid.UUID, it *GDriveItem) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM gdrive_items WHERE link_id = $1 AND file_id = $2 AND gdrive_id <> $3`,
		linkID, it.FileID, it.GDriveID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO gdrive_items (link_id, gdrive_id, file_id, is_folder, g_name, g_parent, g_md5, x_name, x_parent, x_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (link_id, gdrive_id) DO UPDATE SET file_id = EXCLUDED.file_id, is_folder = EXCLUDED.is_folder,
		  g_name = EXCLUDED.g_name, g_parent = EXCLUDED.g_parent, g_md5 = EXCLUDED.g_md5,
		  x_name = EXCLUDED.x_name, x_parent = EXCLUDED.x_parent, x_sha256 = EXCLUDED.x_sha256`,
		linkID, it.GDriveID, it.FileID, it.IsFolder, it.GName, it.GParent, it.GMD5, it.XName, it.XParent, it.XSHA256); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteGDriveItem(ctx context.Context, linkID uuid.UUID, gdriveID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM gdrive_items WHERE link_id = $1 AND gdrive_id = $2`, linkID, gdriveID)
	return err
}

// Subtree 列出文件夹 root 下面（不含 root 本身）所有不在回收站里的条目，上级在前。进了回收站的文件夹里面的不算。
func (s *Store) Subtree(ctx context.Context, userID, root uuid.UUID) ([]*File, error) {
	rows, err := s.db.Query(ctx, `
		WITH RECURSIVE t AS (
			SELECT f.*, 1 AS depth FROM files f
			WHERE f.parent_id = $2 AND f.user_id = $1 AND f.status = 'active' AND f.trashed_at IS NULL
			UNION ALL
			SELECT f.*, t.depth + 1 FROM files f JOIN t ON f.parent_id = t.id
			WHERE t.type = 'folder' AND f.status = 'active' AND f.trashed_at IS NULL
		)
		SELECT `+fileColumns+` FROM t ORDER BY depth, type DESC, name`, userID, root)
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

// SubtreeFingerprint 是文件夹 root 下面现在的样子：所有条目（含回收站里的、上传中的）的个数和最后修改时间。
// 和上次一样就说明网盘这边没动过。
func (s *Store) SubtreeFingerprint(ctx context.Context, userID, root uuid.UUID) (string, error) {
	var n int64
	var last *time.Time
	err := s.db.QueryRow(ctx, `
		WITH RECURSIVE t AS (
			SELECT f.id, f.type, f.updated_at FROM files f WHERE f.parent_id = $2 AND f.user_id = $1
			UNION ALL
			SELECT f.id, f.type, f.updated_at FROM files f JOIN t ON f.parent_id = t.id WHERE t.type = 'folder'
		)
		SELECT count(*), max(updated_at) FROM t`, userID, root).Scan(&n, &last)
	if err != nil {
		return "", err
	}
	fp := fmt.Sprintf("%d", n)
	if last != nil {
		fp += "@" + last.UTC().Format(time.RFC3339Nano)
	}
	return fp, nil
}
