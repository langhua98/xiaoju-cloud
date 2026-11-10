package dbdump

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

func TestSealRoundTrip(t *testing.T) {
	key := BackupKey([]byte("0123456789abcdef0123456789abcdef"))
	for _, n := range []int{0, 1, segmentSize - 1, segmentSize, segmentSize + 1, 3*segmentSize + 17} {
		plain := make([]byte, n)
		rand.Read(plain)
		var buf bytes.Buffer
		w, err := NewSealWriter(&buf, key)
		if err != nil {
			t.Fatal(err)
		}
		// 分几次写，跨过分段边界
		for p := plain; len(p) > 0; {
			k := min(len(p), 300_000)
			if _, err := w.Write(p[:k]); err != nil {
				t.Fatal(err)
			}
			p = p[k:]
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		sealed := buf.Bytes()
		if bytes.Contains(sealed, plain) && n > 64 {
			t.Fatal("plaintext visible in sealed output")
		}
		r, err := NewOpenReader(bytes.NewReader(sealed), key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip mismatch", n)
		}

		// 截断：少了最后一段一定要报错
		if n > segmentSize {
			r, _ := NewOpenReader(bytes.NewReader(sealed[:len(sealed)-100]), key)
			if _, err := io.ReadAll(r); err == nil {
				t.Fatalf("size %d: truncated file accepted", n)
			}
			cut := len(sealMagic) + prefixSize + 5 + segmentSize + 16 // 正好在第一段后面截断
			r, _ = NewOpenReader(bytes.NewReader(sealed[:cut]), key)
			if _, err := io.ReadAll(r); err == nil {
				t.Fatalf("size %d: file cut at a segment boundary accepted", n)
			}
		}
		// 密钥不对
		r, _ = NewOpenReader(bytes.NewReader(sealed), BackupKey([]byte("another master key, 32 bytes!!!!")))
		if _, err := io.ReadAll(r); !errors.Is(err, ErrWrongKey) {
			t.Fatalf("size %d: wrong key: %v", n, err)
		}
	}
}

// 集成测试：导出 XIAOJU_TEST_DATABASE_URL 的数据库，恢复进一个新建的数据库，比较每张表。
func TestDumpRestore(t *testing.T) {
	src := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if src == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	// 各种类型都放一点：bytea、数组、jsonb、换行和反斜杠、自引用外键、bigserial
	var userID, folderID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('dump-test-' || gen_random_uuid()) RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO files (user_id, name, type, status) VALUES ($1, E'a\\b\nc', 'folder', 'active') RETURNING id`, userID).Scan(&folderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO files (user_id, parent_id, name, type, size, mime, sha256, dek_wrapped, status)
		VALUES ($1, $2, '\.', 'file', 3, 'text/plain', '\x0102'::bytea, '\x00ff'::bytea, 'active')`, userID, folderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_audit (user_id, action, paths, detail, result) VALUES ($1, 'x', '{"/a","b c"}', '{"k":"v\n"}', 'ok')`, userID); err != nil {
		t.Fatal(err)
	}

	// 新建一个空数据库恢复进去
	name := "xiaoju_restore_test"
	pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skip("cannot create a database for the restore test:", err)
	}
	defer pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	u, err := url.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dst, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	master := []byte("0123456789abcdef0123456789abcdef")
	var buf bytes.Buffer
	info, err := WriteBackup(ctx, pool, master, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if info.Rows["files"] < 2 || info.LastMigration() == "" {
		t.Fatalf("info: %+v", info)
	}
	if strings.Contains(buf.String(), "dump-test-") {
		t.Fatal("backup is not encrypted")
	}
	if _, err := VerifyBackup(bytes.NewReader(buf.Bytes()), master); err != nil {
		t.Fatal(err)
	}
	got, err := RestoreBackup(ctx, dst, migrations.FS, master, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for table, n := range info.Rows {
		if got.Rows[table] != n {
			t.Errorf("%s: restored %d rows, dumped %d", table, got.Rows[table], n)
		}
	}
	// 内容逐表比较：两边各导出一次文本，去掉时间戳行以外完全一样
	// （别的包的测试同时在用这个数据库，只比较这个测试自己写的行）
	for _, q := range []string{
		`SELECT string_agg(f::text, '|' ORDER BY id) FROM files f WHERE user_id = $1`,
		`SELECT string_agg(a::text, '|' ORDER BY id) FROM agent_audit a WHERE user_id = $1`,
		`SELECT string_agg(version, ',' ORDER BY version) FROM schema_migrations WHERE $1::uuid IS NOT NULL`,
	} {
		var a, b string
		if err := pool.QueryRow(ctx, q, userID).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := dst.QueryRow(ctx, q, userID).Scan(&b); err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("%s differs after restore:\n%s\n%s", q, a, b)
		}
	}
	// 外键加回来了：删用户会级联删掉文件
	var fks int
	dst.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`).Scan(&fks)
	var srcFKs int
	pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'`).Scan(&srcFKs)
	if fks != srcFKs || fks == 0 {
		t.Fatalf("foreign keys: %d, expected %d", fks, srcFKs)
	}
	// 自增序列接着往后
	if _, err := dst.Exec(ctx, `INSERT INTO agent_audit (user_id, action, result) VALUES ($1, 'y', 'ok')`, userID); err != nil {
		t.Fatal("insert after restore:", err)
	}
	// 不是空数据库：拒绝
	if _, err := RestoreBackup(ctx, dst, migrations.FS, master, bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("restore into non-empty database: %v", err)
	}
}

// 导出文件里有程序没有的迁移（以前试验分支部署过一次留下的，比如 0002_album_cursor）：
// 它建的表是空的就跳过照样恢复，有数据就拒绝。
func TestRestoreUnknownMigration(t *testing.T) {
	src := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if src == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	u, err := url.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	newDB := func(name string) *pgxpool.Pool {
		admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Skip("cannot create a database for the restore test:", err)
		}
		t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
		v := *u
		v.Path = "/" + name
		p, err := db.Connect(ctx, v.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}

	from := newDB("xiaoju_unknown_src_test")
	if _, err := db.MigrateUp(ctx, from, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err := from.Exec(ctx, `CREATE TABLE album_cursors (id bigint PRIMARY KEY);
		INSERT INTO schema_migrations (version) VALUES ('0002_album_cursor')`); err != nil {
		t.Fatal(err)
	}
	master := []byte("0123456789abcdef0123456789abcdef")
	backup := func() []byte {
		var buf bytes.Buffer
		if _, err := WriteBackup(ctx, from, master, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	// 表是空的：跳过，其余照常恢复，schema_migrations 里那一行也恢复（线上就是带着它运行的）
	dst := newDB("xiaoju_unknown_dst_test")
	got, err := RestoreBackup(ctx, dst, migrations.FS, master, bytes.NewReader(backup()))
	if err != nil {
		t.Fatal("restore with an empty unknown table:", err)
	}
	if len(got.Skipped) != 1 || got.Skipped[0] != "album_cursors" {
		t.Fatalf("skipped: %v", got.Skipped)
	}
	var n int
	dst.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '0002_album_cursor'`).Scan(&n)
	if n != 1 {
		t.Fatalf("schema_migrations row for the unknown migration: %d", n)
	}
	if _, err := db.MigrateUp(ctx, dst, migrations.FS); err != nil {
		t.Fatal("migrate after restore:", err)
	}

	// 表里有数据：拒绝，不能悄悄丢数据
	if _, err := from.Exec(ctx, `INSERT INTO album_cursors (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	dst2 := newDB("xiaoju_unknown_dst2_test")
	_, err = RestoreBackup(ctx, dst2, migrations.FS, master, bytes.NewReader(backup()))
	if err == nil || !strings.Contains(err.Error(), "0002_album_cursor") {
		t.Fatalf("restore with data in an unknown table: %v", err)
	}
}
