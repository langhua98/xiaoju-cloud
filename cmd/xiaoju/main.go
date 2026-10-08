// xiaoju 是 xiaoju-cloud 的服务端入口。
//
//	xiaoju migrate [up|down]   执行 / 回滚数据库迁移
//	xiaoju serve               启动 HTTP 服务
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/api"
	"github.com/langhua98/xiaoju-cloud/internal/config"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/internal/storage/telegram"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("usage: xiaoju <serve|migrate [up|down]>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch args[0] {
	case "migrate":
		dir := "up"
		if len(args) > 1 {
			dir = args[1]
		}
		return migrate(ctx, cfg, dir, log)
	case "serve":
		return serve(ctx, cfg, log)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func migrate(ctx context.Context, cfg *config.Config, dir string, log *slog.Logger) error {
	if err := cfg.ValidateDB(); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch dir {
	case "up":
		done, err := db.MigrateUp(ctx, pool, migrations.FS)
		log.Info("migrate up", "applied", done)
		return err
	case "down":
		v, err := db.MigrateDown(ctx, pool, migrations.FS)
		log.Info("migrate down", "rolled_back", v)
		return err
	default:
		return fmt.Errorf("unknown migrate direction %q", dir)
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if done, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		return err
	} else if len(done) > 0 {
		log.Info("applied migrations", "versions", done)
	}

	driver, tgDone, err := newDriver(ctx, cfg, log)
	if err != nil {
		return err
	}

	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "admin")
	if err != nil {
		return err
	}
	svc := service.New(store, driver, service.Options{
		MasterKey: cfg.MasterKey, PartSize: cfg.PartSize, TempDir: cfg.TempDir, Log: log,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(svc, userID, cfg.AdminToken, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "storage", driver.Name())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case err := <-tgDone:
		return fmt.Errorf("telegram client stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newDriver 创建存储驱动。telegram 驱动额外返回客户端退出通知；local 驱动返回 nil channel（永不触发）。
func newDriver(ctx context.Context, cfg *config.Config, log *slog.Logger) (storage.Driver, <-chan error, error) {
	if cfg.Storage == "local" {
		d, err := local.New(cfg.LocalDir)
		return d, nil, err
	}
	t := cfg.Telegram
	tgAPI, done, err := telegram.Start(ctx, telegram.ClientConfig{
		AppID: t.AppID, AppHash: t.AppHash, BotToken: t.BotToken, SessionFile: t.SessionFile,
		Transport: t.Transport,
	}, log)
	if err != nil {
		return nil, nil, err
	}
	d, err := telegram.New(ctx, tgAPI, t.ChannelID)
	if err != nil {
		return nil, nil, err
	}
	return d, done, nil
}
