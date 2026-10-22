package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Share 是一个分享链接（见 shares 表）。File 是分享的文件或文件夹。
type Share struct {
	ID           uuid.UUID  `json:"id"`
	UserID       uuid.UUID  `json:"-"`
	FileID       uuid.UUID  `json:"file_id"`
	Token        string     `json:"token"`
	PasswordHash []byte     `json:"-"`
	PasswordSalt []byte     `json:"-"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Views        int64      `json:"views"`
	CreatedAt    time.Time  `json:"created_at"`
	File         *File      `json:"file,omitempty"`
	AgentTokenID *uuid.UUID `json:"agent_token_id,omitempty"` // 哪把 agent 令牌建的（只在创建时写）
}

// HasPassword 报告这个分享要不要密码。
func (s *Share) HasPassword() bool { return len(s.PasswordHash) > 0 }

// Expired 报告分享是不是过期了。
func (s *Share) Expired(now time.Time) bool { return s.ExpiresAt != nil && now.After(*s.ExpiresAt) }

const shareColumns = `s.id, s.user_id, s.file_id, s.token, s.password_hash, s.password_salt, s.expires_at, s.views, s.created_at`

func scanShareRow(row pgx.Row) (*Share, error) {
	var sh Share
	var f File
	err := row.Scan(&sh.ID, &sh.UserID, &sh.FileID, &sh.Token, &sh.PasswordHash, &sh.PasswordSalt, &sh.ExpiresAt, &sh.Views, &sh.CreatedAt,
		&f.ID, &f.UserID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.Mime,
		&f.SHA256, &f.DEKWrapped, &f.Status, &f.TrashedAt, &f.TakenAt, &f.Private, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sh.File = &f
	return &sh, nil
}

// CreateShare 给用户的一个文件或文件夹建分享链接。
func (s *Store) CreateShare(ctx context.Context, sh *Share) (*Share, error) {
	if _, err := s.Get(ctx, sh.UserID, sh.FileID); err != nil {
		return nil, err
	}
	var id uuid.UUID
	if err := s.db.QueryRow(ctx, `
		INSERT INTO shares (user_id, file_id, token, password_hash, password_salt, expires_at, agent_token_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		sh.UserID, sh.FileID, sh.Token, sh.PasswordHash, sh.PasswordSalt, sh.ExpiresAt, sh.AgentTokenID).Scan(&id); err != nil {
		return nil, err
	}
	return s.getShare(ctx, `s.id = $1`, id)
}

func (s *Store) getShare(ctx context.Context, where string, arg any) (*Share, error) {
	return scanShareRow(s.db.QueryRow(ctx, `
		SELECT `+shareColumns+`, `+prefixed("f", fileColumns)+`
		FROM shares s JOIN files f ON f.id = s.file_id
		WHERE `+where, arg))
}

// ShareByToken 按链接里的随机串找分享（不管过没过期、文件在不在回收站，调用方判断）。
func (s *Store) ShareByToken(ctx context.Context, token string) (*Share, error) {
	return s.getShare(ctx, `s.token = $1`, token)
}

// ListShares 列出用户的分享链接，最近建的在前。
func (s *Store) ListShares(ctx context.Context, userID uuid.UUID) ([]*Share, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+shareColumns+`, `+prefixed("f", fileColumns)+`
		FROM shares s JOIN files f ON f.id = s.file_id
		WHERE s.user_id = $1 ORDER BY s.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Share
	for rows.Next() {
		sh, err := scanShareRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// DeleteShare 撤销一个分享链接。
func (s *Store) DeleteShare(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM shares WHERE id = $1 AND user_id = $2`, id, userID)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrNotFound
	}
	return err
}

// CountShareView 记一次打开。
func (s *Store) CountShareView(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.Exec(ctx, `UPDATE shares SET views = views + 1 WHERE id = $1`, id)
	return err
}

// IsInside 报告文件夹 folder 是不是 ancestor 本身或在它里面（任意层）。
func (s *Store) IsInside(ctx context.Context, folder, ancestor uuid.UUID) (bool, error) {
	if folder == ancestor {
		return true, nil
	}
	return s.isInside(ctx, folder, ancestor)
}
