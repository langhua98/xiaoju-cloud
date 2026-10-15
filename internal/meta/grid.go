package meta

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetGridRule 返回加密后的格子口令规则；还没开启时返回 ErrNotFound。
func (s *Store) GetGridRule(ctx context.Context) ([]byte, error) {
	var wrapped []byte
	err := s.db.QueryRow(ctx, `SELECT rule_wrapped FROM grid_rule WHERE id = 1`).Scan(&wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return wrapped, err
}

// SetGridRule 保存规则，并用新的一组恢复码替换旧的。
func (s *Store) SetGridRule(ctx context.Context, wrapped []byte, codeHashes [][]byte) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO grid_rule (id, rule_wrapped) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET rule_wrapped = excluded.rule_wrapped, created_at = now()`, wrapped); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes`); err != nil {
			return err
		}
		for _, h := range codeHashes {
			if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (code_hash) VALUES ($1)`, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteGridRule 关闭格子口令，同时作废全部恢复码。
func (s *Store) DeleteGridRule(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM grid_rule`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM recovery_codes`)
		return err
	})
}

// UseRecoveryCode 作废一个未使用的恢复码；不存在或已用过时返回 false。
func (s *Store) UseRecoveryCode(ctx context.Context, hash []byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE recovery_codes SET used_at = now() WHERE code_hash = $1 AND used_at IS NULL`, hash)
	return tag.RowsAffected() == 1, err
}

// CountRecoveryCodes 返回还没用过的恢复码数量。
func (s *Store) CountRecoveryCodes(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM recovery_codes WHERE used_at IS NULL`).Scan(&n)
	return n, err
}
