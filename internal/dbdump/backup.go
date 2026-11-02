package dbdump

import (
	"context"
	"io"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FileExt 是备份文件的扩展名。
const FileExt = ".xjdb"

// WriteBackup 导出整个数据库，用主密钥派生的密钥加密后写到 w。
func WriteBackup(ctx context.Context, pool *pgxpool.Pool, master []byte, w io.Writer) (Info, error) {
	sw, err := NewSealWriter(w, BackupKey(master))
	if err != nil {
		return Info{}, err
	}
	info, err := Dump(ctx, pool, sw)
	if err != nil {
		return Info{}, err
	}
	return info, sw.Close()
}

// VerifyBackup 解密并完整读一遍备份文件，不写数据库。
func VerifyBackup(r io.Reader, master []byte) (Info, error) {
	or, err := NewOpenReader(r, BackupKey(master))
	if err != nil {
		return Info{}, err
	}
	return Verify(or)
}

// RestoreBackup 解密备份文件并恢复进空数据库。
func RestoreBackup(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS, master []byte, r io.Reader) (Info, error) {
	or, err := NewOpenReader(r, BackupKey(master))
	if err != nil {
		return Info{}, err
	}
	return Restore(ctx, pool, migrations, or)
}
