package meta

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RevokeSession 记下被强制退出的会话，顺便清掉已经过期的记录。
func (s *Store) RevokeSession(ctx context.Context, id string, expires time.Time) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM revoked_sessions WHERE expires_at < now()`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO revoked_sessions (id, expires_at) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, expires)
		return err
	})
}

// ListRevokedSessions 返回还没过期的被强制退出的会话：ID → 过期时间。
func (s *Store) ListRevokedSessions(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT id, expires_at FROM revoked_sessions WHERE expires_at > now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var exp time.Time
		if err := rows.Scan(&id, &exp); err != nil {
			return nil, err
		}
		out[id] = exp
	}
	return out, rows.Err()
}
