// Package dbdump 把整个数据库导出成一个文件、再从这个文件恢复（不依赖 pg_dump，镜像里不用装 PostgreSQL 客户端）。
//
// 格式（gzip 压缩后再用 Seal 加密，见 seal.go）：
//
//	第一行：Header 的 JSON
//	每张表：一行 {"table":"files","columns":[...]}，接着是 COPY ... TO STDOUT 的文本输出，最后一行 \.
//
// COPY 文本格式里换行、反斜杠都转义了，所以数据里不会出现单独一行的 \.。
//
// 恢复只往空数据库里恢复：先执行迁移到导出时的版本建好表，删掉外键、整表导入、再把外键加回来
// （表之间、files 自己的 parent_id 都有外键，按什么顺序导入都会违反；托管数据库不给超级用户，
// 不能用 session_replication_role 关掉检查），最后执行导出之后新加的迁移。
package dbdump

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/langhua98/xiaoju-cloud/internal/db"
)

const formatName = "xiaoju-db-dump"

// Header 是导出文件的第一行。
type Header struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	Created    time.Time `json:"created"`
	Migrations []string  `json:"migrations"` // 导出时已经执行的迁移（按版本排序）
}

type tableHeader struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

// summary 是文件的最后一行：每张表的行数。恢复时核对，少一行都算文件坏了。
type summary struct {
	End  bool           `json:"end"`
	Rows map[string]int `json:"rows"`
}

// Info 是一次导出或恢复的结果。
type Info struct {
	Created    time.Time      `json:"created"`
	Migrations []string       `json:"migrations"`
	Rows       map[string]int `json:"rows"`
}

// LastMigration 返回导出时最新的迁移版本。
func (i Info) LastMigration() string {
	if len(i.Migrations) == 0 {
		return ""
	}
	return i.Migrations[len(i.Migrations)-1]
}

// Dump 在一个只读的快照事务里导出 public 下的所有表，写成 gzip 压缩的流。
func Dump(ctx context.Context, pool *pgxpool.Pool, w io.Writer) (Info, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Info{}, err
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Info{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	info := Info{Created: time.Now().UTC(), Rows: map[string]int{}}
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return Info{}, err
	}
	if info.Migrations, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return Info{}, err
	}
	tables, err := listTables(ctx, tx)
	if err != nil {
		return Info{}, err
	}

	zw := gzip.NewWriter(w)
	bw := bufio.NewWriterSize(zw, 1<<20)
	if err := writeJSONLine(bw, Header{Format: formatName, Version: 1, Created: info.Created, Migrations: info.Migrations}); err != nil {
		return Info{}, err
	}
	for _, t := range tables {
		if err := writeJSONLine(bw, t); err != nil {
			return Info{}, err
		}
		cw := &lineCounter{w: bw}
		sql := fmt.Sprintf("COPY %s (%s) TO STDOUT", pgx.Identifier{t.Table}.Sanitize(), quoteColumns(t.Columns))
		if _, err := tx.Conn().PgConn().CopyTo(ctx, cw, sql); err != nil {
			return Info{}, fmt.Errorf("dump %s: %w", t.Table, err)
		}
		if cw.partial {
			return Info{}, fmt.Errorf("dump %s: COPY output does not end with a newline", t.Table)
		}
		info.Rows[t.Table] = cw.lines
		if _, err := bw.WriteString("\\.\n"); err != nil {
			return Info{}, err
		}
	}
	if err := writeJSONLine(bw, summary{End: true, Rows: info.Rows}); err != nil {
		return Info{}, err
	}
	if err := bw.Flush(); err != nil {
		return Info{}, err
	}
	if err := zw.Close(); err != nil {
		return Info{}, err
	}
	return info, nil
}

// listTables 列出 public 下的表和要导出的列（生成列不能导入，跳过）。
func listTables(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]tableHeader, error) {
	rows, err := q.Query(ctx, `
		SELECT c.table_name, array_agg(c.column_name::text ORDER BY c.ordinal_position)
		FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = 'public' AND t.table_type = 'BASE TABLE' AND c.is_generated = 'NEVER'
		GROUP BY c.table_name ORDER BY c.table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableHeader
	for rows.Next() {
		var t tableHeader
		if err := rows.Scan(&t.Table, &t.Columns); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func quoteColumns(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = pgx.Identifier{c}.Sanitize()
	}
	return strings.Join(q, ", ")
}

func writeJSONLine(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// lineCounter 数 COPY 输出了几行（每行一条记录）。
type lineCounter struct {
	w       io.Writer
	lines   int
	partial bool // 最后写的不是换行
}

func (c *lineCounter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.lines += bytes.Count(p, []byte{'\n'})
		c.partial = p[len(p)-1] != '\n'
	}
	return c.w.Write(p)
}

// ReadHeader 只读出导出文件的第一行（检查文件、显示信息用）。r 是 gzip 压缩的流（已经解密）。
func ReadHeader(r io.Reader) (Header, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Header{}, notDump(err)
	}
	return readHeader(bufio.NewReader(zr))
}

// notDump 包装 gzip 打不开的错误；解密失败（主密钥不对）原样返回。
func notDump(err error) error {
	if errors.Is(err, ErrWrongKey) {
		return err
	}
	return fmt.Errorf("not a database dump: %w", err)
}

func readHeader(br *bufio.Reader) (Header, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return Header{}, fmt.Errorf("read header: %w", err)
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil || h.Format != formatName {
		return Header{}, errors.New("not a xiaoju database dump")
	}
	if h.Version != 1 {
		return Header{}, fmt.Errorf("unsupported dump version %d", h.Version)
	}
	return h, nil
}

// Verify 把整个导出文件读一遍，核对格式和每张表的行数，不写数据库。
func Verify(r io.Reader) (Info, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Info{}, notDump(err)
	}
	br := bufio.NewReaderSize(zr, 1<<20)
	h, err := readHeader(br)
	if err != nil {
		return Info{}, err
	}
	info := Info{Created: h.Created, Migrations: h.Migrations, Rows: map[string]int{}}
	err = eachTable(br, func(_ tableHeader, data io.Reader) error {
		_, err := io.Copy(io.Discard, data)
		return err
	}, info.Rows)
	return info, err
}

// eachTable 依次把每张表的 COPY 数据交给 fn（fn 必须读完 data），最后核对 summary 里的行数。
func eachTable(br *bufio.Reader, fn func(t tableHeader, data io.Reader) error, rows map[string]int) error {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("dump is truncated: %w", err)
		}
		var probe struct {
			Table string         `json:"table"`
			End   bool           `json:"end"`
			Rows  map[string]int `json:"rows"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return fmt.Errorf("dump is damaged: %w", err)
		}
		if probe.End {
			for t, n := range probe.Rows {
				if rows[t] != n {
					return fmt.Errorf("dump is damaged: table %s has %d rows, expected %d", t, rows[t], n)
				}
			}
			if len(probe.Rows) != len(rows) {
				return errors.New("dump is damaged: table list does not match the summary")
			}
			return nil
		}
		var t tableHeader
		if err := json.Unmarshal(line, &t); err != nil || t.Table == "" || len(t.Columns) == 0 {
			return errors.New("dump is damaged: bad table header")
		}
		tr := &tableReader{br: br}
		if err := fn(t, tr); err != nil {
			return fmt.Errorf("table %s: %w", t.Table, err)
		}
		if !tr.done {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return fmt.Errorf("table %s: %w", t.Table, err)
			}
		}
		rows[t.Table] = tr.lines
	}
}

// tableReader 读一张表的 COPY 数据，到单独一行 \. 为止（不含这一行）。
type tableReader struct {
	br    *bufio.Reader
	buf   []byte
	lines int
	done  bool
	err   error
}

func (r *tableReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if r.err != nil {
			return 0, r.err
		}
		line, err := r.br.ReadBytes('\n')
		if err != nil {
			r.err = fmt.Errorf("dump is truncated: %w", err)
			if errors.Is(err, io.EOF) {
				r.err = errors.New("dump is truncated")
			}
			return 0, r.err
		}
		if string(line) == "\\.\n" {
			r.done = true
			return 0, io.EOF
		}
		r.lines++
		r.buf = line
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// ErrNotEmpty 表示要恢复的数据库里已经有数据。
var ErrNotEmpty = errors.New("target database is not empty: restore only into an empty database")

// Restore 把导出文件恢复进一个空数据库（r 是 gzip 压缩的流，已经解密），然后执行之后新加的迁移。
// 程序里没有导出时用过的某个迁移（程序比导出文件旧）时报错，什么都不写。
func Restore(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS, r io.Reader) (Info, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Info{}, notDump(err)
	}
	br := bufio.NewReaderSize(zr, 1<<20)
	h, err := readHeader(br)
	if err != nil {
		return Info{}, err
	}
	known, err := db.Versions(migrations)
	if err != nil {
		return Info{}, err
	}
	for _, v := range h.Migrations {
		if !containsString(known, v) {
			return Info{}, fmt.Errorf("the dump uses migration %s which this program does not have: use a newer build", v)
		}
	}

	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'schema_migrations'`).Scan(&tables); err != nil {
		return Info{}, err
	}
	if tables > 0 {
		return Info{}, ErrNotEmpty
	}
	// 建表：执行到导出时的版本
	if _, err := db.MigrateUpTo(ctx, pool, migrations, h.Migrations); err != nil {
		return Info{}, err
	}

	info := Info{Created: h.Created, Migrations: h.Migrations, Rows: map[string]int{}}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		fks, err := dropForeignKeys(ctx, tx)
		if err != nil {
			return err
		}
		existing, err := listTables(ctx, tx)
		if err != nil {
			return err
		}
		// 迁移里可能插入了默认数据（schema_migrations 本身也是）：清空，以导出文件为准
		names := make([]string, len(existing))
		for i, t := range existing {
			names[i] = pgx.Identifier{t.Table}.Sanitize()
		}
		if len(names) > 0 {
			if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")); err != nil {
				return err
			}
		}
		err = eachTable(br, func(t tableHeader, data io.Reader) error {
			sql := fmt.Sprintf("COPY %s (%s) FROM STDIN", pgx.Identifier{t.Table}.Sanitize(), quoteColumns(t.Columns))
			_, err := tx.Conn().PgConn().CopyFrom(ctx, data, sql)
			return err
		}, info.Rows)
		if err != nil {
			return err
		}
		for _, fk := range fks {
			if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
				fk.table, pgx.Identifier{fk.name}.Sanitize(), fk.def)); err != nil {
				return fmt.Errorf("restore foreign key %s: %w", fk.name, err)
			}
		}
		return resetSequences(ctx, tx)
	})
	if err != nil {
		return Info{}, err
	}
	// 导出之后新加的迁移
	if _, err := db.MigrateUp(ctx, pool, migrations); err != nil {
		return info, fmt.Errorf("restored, but newer migrations failed: %w", err)
	}
	return info, nil
}

type foreignKey struct{ table, name, def string }

func dropForeignKeys(ctx context.Context, tx pgx.Tx) ([]foreignKey, error) {
	rows, err := tx.Query(ctx, `SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE contype = 'f' AND connamespace = 'public'::regnamespace ORDER BY conname`)
	if err != nil {
		return nil, err
	}
	var fks []foreignKey
	for rows.Next() {
		var fk foreignKey
		if err := rows.Scan(&fk.table, &fk.name, &fk.def); err != nil {
			rows.Close()
			return nil, err
		}
		fks = append(fks, fk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, fk := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", fk.table, pgx.Identifier{fk.name}.Sanitize())); err != nil {
			return nil, err
		}
	}
	return fks, nil
}

// resetSequences 把自增列的序列调到表里最大值之后（导入时不经过序列）。
func resetSequences(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT c.table_name, c.column_name, pg_get_serial_sequence(quote_ident(c.table_name), c.column_name)
		FROM information_schema.columns c
		WHERE c.table_schema = 'public' AND pg_get_serial_sequence(quote_ident(c.table_name), c.column_name) IS NOT NULL`)
	if err != nil {
		return err
	}
	type seq struct{ table, column, name string }
	var seqs []seq
	for rows.Next() {
		var s seq
		if err := rows.Scan(&s.table, &s.column, &s.name); err != nil {
			rows.Close()
			return err
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i].name < seqs[j].name })
	for _, s := range seqs {
		sql := fmt.Sprintf("SELECT setval($1, COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)",
			pgx.Identifier{s.column}.Sanitize(), pgx.Identifier{s.table}.Sanitize())
		if _, err := tx.Exec(ctx, sql, s.name); err != nil {
			return fmt.Errorf("reset sequence %s: %w", s.name, err)
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
