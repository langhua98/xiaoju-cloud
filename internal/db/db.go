// Package db 提供 PostgreSQL 连接池与内嵌迁移执行器。
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

const ensureTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    text PRIMARY KEY,
	applied_at timestamptz NOT NULL DEFAULT now()
)`

// MigrateUp 依次执行所有未执行过的 *.up.sql，每个文件一个事务。返回本次执行的版本。
func MigrateUp(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) ([]string, error) {
	if _, err := pool.Exec(ctx, ensureTable); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}
	versions, err := listVersions(migrations)
	if err != nil {
		return nil, err
	}

	var done []string
	for _, v := range versions {
		if applied[v] {
			continue
		}
		sql, err := fs.ReadFile(migrations, v+".up.sql")
		if err != nil {
			return done, err
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("migration %s: %w", v, err)
		}
		done = append(done, v)
	}
	return done, nil
}

// MigrateDown 回滚最近一次迁移。没有可回滚的迁移时返回空字符串。
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) (string, error) {
	if _, err := pool.Exec(ctx, ensureTable); err != nil {
		return "", err
	}
	var v string
	err := pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&v)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sql, err := fs.ReadFile(migrations, v+".down.sql")
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, v)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("rollback %s: %w", v, err)
	}
	return v, nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	vs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m, nil
}

func listVersions(migrations fs.FS) ([]string, error) {
	names, err := fs.Glob(migrations, "*.up.sql")
	if err != nil {
		return nil, err
	}
	vs := make([]string, 0, len(names))
	for _, n := range names {
		vs = append(vs, strings.TrimSuffix(n, ".up.sql"))
	}
	sort.Strings(vs)
	return vs, nil
}
