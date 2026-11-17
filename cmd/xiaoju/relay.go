package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/config"
	"github.com/langhua98/xiaoju-cloud/internal/relay"
)

// serveRelay：XIAOJU_RELAY_UPSTREAM 设了时只做视频中转（见 internal/relay）。
// 不连数据库、不跑后台任务、不往 Telegram 写任何东西；只连上 Telegram 读分块。
func serveRelay(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.AdminToken == "" {
		return errors.New("XIAOJU_ADMIN_TOKEN is required (same as on the home server)")
	}
	if len(cfg.MasterKey) != 32 {
		return errors.New("XIAOJU_MASTER_KEY must be the same 64 hex chars as on the home server")
	}
	if cfg.Storage == "telegram" {
		if err := cfg.ValidateTelegram(); err != nil {
			return err
		}
	}
	driver, tgDone, _, err := newDriver(ctx, cfg, log)
	if err != nil {
		return err
	}
	backup, backupDone, err := newBackupDriver(ctx, cfg, log)
	if err != nil {
		log.Warn("relay: backup channel unavailable, reading the main channel only", "err", err)
		backup = nil
	}
	rs := relay.New(relay.Options{
		Upstream: cfg.RelayUpstream, AdminToken: cfg.AdminToken, MasterKey: cfg.MasterKey,
		Driver: driver, Backup: backup, Log: log.With("component", "relay"),
	})
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: rs.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("video relay listening", "addr", cfg.HTTPAddr, "upstream", cfg.RelayUpstream, "storage", driver.Name())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case err := <-tgDone:
		return fmt.Errorf("telegram client stopped: %w", err)
	case err := <-backupDone:
		return fmt.Errorf("backup telegram client stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
