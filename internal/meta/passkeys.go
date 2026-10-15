package meta

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Passkey 是一台已注册的 Face ID / 指纹设备。
type Passkey struct {
	ID         []byte
	Name       string
	Credential json.RawMessage
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

func (s *Store) ListPasskeys(ctx context.Context) ([]Passkey, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, credential, created_at, last_used_at FROM passkeys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Passkey, error) {
		var p Passkey
		err := r.Scan(&p.ID, &p.Name, &p.Credential, &p.CreatedAt, &p.LastUsedAt)
		return p, err
	})
}

func (s *Store) CountPasskeys(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM passkeys`).Scan(&n)
	return n, err
}

func (s *Store) AddPasskey(ctx context.Context, id []byte, name string, credential json.RawMessage) error {
	_, err := s.db.Exec(ctx, `INSERT INTO passkeys (id, name, credential) VALUES ($1, $2, $3)`, id, name, credential)
	return mapErr(err)
}

// TouchPasskey 登录成功后更新凭据（签名计数等）和最近使用时间。
func (s *Store) TouchPasskey(ctx context.Context, id []byte, credential json.RawMessage) error {
	_, err := s.db.Exec(ctx, `UPDATE passkeys SET credential = $2, last_used_at = now() WHERE id = $1`, id, credential)
	return err
}

func (s *Store) DeletePasskey(ctx context.Context, id []byte) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM passkeys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAllPasskeys 清空所有已注册设备（所有设备都丢失时的紧急恢复）。
func (s *Store) DeleteAllPasskeys(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM passkeys`)
	return tag.RowsAffected(), err
}
