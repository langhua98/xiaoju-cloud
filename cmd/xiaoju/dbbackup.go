package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/langhua98/xiaoju-cloud/internal/config"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/dbdump"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/telegram"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// dbBackupCmd：xiaoju dbbackup OUT.xjdb，把数据库导出成加密的备份文件。
func dbBackupCmd(ctx context.Context, cfg *config.Config, args []string, log *slog.Logger) error {
	if len(args) != 1 {
		return errors.New("usage: xiaoju dbbackup OUT" + dbdump.FileExt)
	}
	if err := cfg.ValidateDB(); err != nil {
		return err
	}
	if len(cfg.MasterKey) != 32 {
		return errors.New("XIAOJU_MASTER_KEY is required: the backup is encrypted with it")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	f, err := os.Create(args[0])
	if err != nil {
		return err
	}
	info, err := dbdump.WriteBackup(ctx, pool, cfg.MasterKey, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(args[0])
		return err
	}
	log.Info("database backup written", "out", args[0], "migration", info.LastMigration(), "rows", info.Rows)
	return nil
}

// dbRestoreCmd：xiaoju dbrestore [--check] [FILE]。
// 不给 FILE 时连 Telegram，从主频道（不行就从备份频道）的置顶消息下载最新的备份；置顶的不是备份时扫一遍频道找最新的。
// 恢复只往空数据库里写（XIAOJU_DATABASE_URL 指向一个新建的空数据库）。需要原来的 XIAOJU_MASTER_KEY。
func dbRestoreCmd(ctx context.Context, cfg *config.Config, args []string, log *slog.Logger) error {
	check := false
	if len(args) > 0 && args[0] == "--check" {
		check, args = true, args[1:]
	}
	if len(args) > 1 {
		return errors.New("usage: xiaoju dbrestore [--check] [FILE" + dbdump.FileExt + "]")
	}
	if len(cfg.MasterKey) != 32 {
		return errors.New("XIAOJU_MASTER_KEY (the original one) is required to decrypt the backup")
	}
	var path string
	if len(args) == 1 {
		path = args[0]
	} else {
		tmp, err := downloadLatestBackup(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		path = tmp
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := dbdump.VerifyBackup(f, cfg.MasterKey)
	if err != nil {
		return err
	}
	log.Info("backup file is good", "created", info.Created, "migration", info.LastMigration(), "rows", info.Rows)
	if check {
		return nil
	}
	if err := cfg.ValidateDB(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	restored, err := dbdump.RestoreBackup(ctx, pool, migrations.FS, cfg.MasterKey, f)
	if err != nil {
		return err
	}
	if len(restored.Skipped) > 0 {
		log.Warn("skipped empty tables from migrations this program does not have", "tables", restored.Skipped)
	}
	log.Info("database restored", "created", info.Created)
	return nil
}

// downloadLatestBackup 从频道下载最新的数据库备份到临时文件，返回路径。
func downloadLatestBackup(ctx context.Context, cfg *config.Config, log *slog.Logger) (string, error) {
	if err := cfg.ValidateTelegram(); err != nil {
		return "", fmt.Errorf("give a backup file, or configure Telegram to download it from the channel: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := cfg.Telegram
	var errs []error
	channels := []struct {
		name, token, session string
		id                   int64
	}{
		{"main channel", t.BotToken, t.SessionFile, t.ChannelID},
		{"backup channel", t.BackupBotToken, t.BackupSessionFile, t.BackupChannelID},
	}
	for _, c := range channels {
		if c.token == "" || c.id == 0 {
			continue
		}
		path, err := func() (string, error) {
			api, _, err := telegram.Start(ctx, telegram.ClientConfig{
				AppID: t.AppID, AppHash: t.AppHash, BotToken: c.token, SessionFile: c.session, Transport: t.Transport,
			}, log.With("client", c.name))
			if err != nil {
				return "", err
			}
			d, err := telegram.New(ctx, api, c.id)
			if err != nil {
				return "", err
			}
			return downloadFrom(ctx, d, cfg.TempDir, log.With("channel", c.name))
		}()
		if err == nil {
			return path, nil
		}
		log.Warn("cannot get the backup from this channel", "channel", c.name, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
	}
	if len(errs) == 0 {
		return "", errors.New("no Telegram channel configured")
	}
	return "", errors.Join(errs...)
}

func downloadFrom(ctx context.Context, d *telegram.Driver, tempDir string, log *slog.Logger) (string, error) {
	obj, caption, err := d.Pinned(ctx)
	want := ""
	if err == nil {
		if c, ok := service.ParseDBBackupCaption(caption); ok {
			want = c.SHA256
		} else {
			err = errors.New("the pinned message is not a database backup")
		}
	}
	if err != nil {
		// 置顶没成功（Bot 没有置顶权限）或者被换掉了：扫一遍频道，找最新的备份文件
		log.Warn("no pinned backup, scanning the channel for the newest one", "err", err)
		obj = storage.Object{}
		if err := d.List(ctx, 0, func(o storage.Object) error {
			if strings.HasPrefix(o.Name, "xiaoju-db-") && strings.HasSuffix(o.Name, dbdump.FileExt) && o.MessageID > obj.MessageID {
				obj = o
			}
			return nil
		}); err != nil {
			return "", err
		}
		if obj.MessageID == 0 {
			return "", errors.New("no database backup found in the channel")
		}
	}
	log.Info("downloading backup", "name", obj.Name, "message", obj.MessageID, "bytes", obj.Size)
	rd, err := d.ReadRange(ctx, obj.Location, 0, obj.Size)
	if err != nil {
		return "", err
	}
	defer rd.Close()
	tmp, err := os.CreateTemp(tempDir, "xiaoju-restore-*"+dbdump.FileExt)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), rd)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && want != "" && hex.EncodeToString(h.Sum(nil)) != want {
		err = errors.New("downloaded backup does not match its sha256")
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
