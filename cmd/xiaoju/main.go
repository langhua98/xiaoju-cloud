// xiaoju 是 xiaoju-cloud 的服务端入口。
//
//	xiaoju migrate [up|down]   执行 / 回滚数据库迁移
//	xiaoju serve               启动 HTTP 服务（未配置数据库时进入 Telegram 连通性诊断模式）
//	xiaoju check               只检查 Telegram 登录与频道访问
//	xiaoju thumb IN OUT.jpg    给本地的照片 / 视频生成缩略图（检查镜像里的 vips、ffmpeg 是否可用）
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // 内置时区数据：格子口令按 XIAOJU_TIMEZONE 判断星期，不依赖系统是否装了 tzdata

	"github.com/langhua98/xiaoju-cloud/internal/api"
	"github.com/langhua98/xiaoju-cloud/internal/auth"
	"github.com/langhua98/xiaoju-cloud/internal/config"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/internal/storage/telegram"
	"github.com/langhua98/xiaoju-cloud/internal/thumb"
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
		return errors.New("usage: xiaoju <serve|migrate [up|down]|check|thumb IN OUT|preview IN OUT>")
	}
	if args[0] == "thumb" || args[0] == "preview" { // 不需要任何配置
		return makeThumb(ctx, args[0], args[1:], log)
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
		if cfg.DatabaseURL == "" && cfg.Storage == "telegram" {
			// 还没配置数据库时进入诊断模式，先验证运行环境能否连上 Telegram。
			return diagnose(ctx, cfg, log)
		}
		return serve(ctx, cfg, log)
	case "check":
		return checkTelegram(ctx, cfg, log)
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
	// 备份频道连不上时主服务照常运行，原因通过 /api/backup/status 和日志暴露
	var backupErr string
	backup, backupDone, err := newBackupDriver(ctx, cfg, log)
	if err != nil {
		log.Error("backup disabled: cannot connect backup channel", "err", err)
		backupErr = err.Error()
	}

	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "admin")
	if err != nil {
		return err
	}
	thumbs := thumb.New(cfg.TempDir)
	thumbs.Location = cfg.Location
	log.Info("thumbnails", "tools", thumbs.Tools())
	svc := service.New(store, driver, service.Options{
		MasterKey: cfg.MasterKey, PartSize: cfg.PartSize, TempDir: cfg.TempDir, Log: log,
		Backup: backup, BackupError: backupErr, Thumbs: thumbs,
	})
	if backup != nil {
		go svc.RunBackupSync(ctx, 5*time.Minute)
	}
	go svc.RunMaintenance(ctx, time.Hour)
	// 定期检查（启动 10 分钟后开始，查完一轮每小时看一次有没有到期要查的）
	go svc.RunScrub(ctx, 10*time.Minute, time.Hour)
	go svc.BackfillThumbnails(ctx)

	passkeys, err := auth.NewPasskeys(ctx, store, userID, auth.PasskeyConfig{RPID: cfg.WebAuthnRPID, Origins: cfg.WebAuthnOrigins})
	if err != nil {
		return err
	}
	grid, err := auth.NewGrid(ctx, store, cfg.MasterKey, cfg.Location)
	if err != nil {
		return err
	}
	if cfg.PasskeyReset {
		n, err := passkeys.ResetAll(ctx)
		if err != nil {
			return err
		}
		if err := grid.Disable(ctx); err != nil {
			return err
		}
		log.Warn("XIAOJU_PASSKEY_RESET is set: removed all face / fingerprint devices and the grid passcode; unset it after re-registering", "removed", n)
	}
	log.Info("grid passcode", "enabled", grid.Enabled(), "timezone", cfg.Location.String())
	if passkeys.Available() {
		log.Info("face / fingerprint login available", "rp_id", cfg.WebAuthnRPID, "origins", cfg.WebAuthnOrigins, "required", passkeys.Required())
	} else {
		log.Warn("face / fingerprint login not configured: set XIAOJU_CORS_ORIGINS (or XIAOJU_WEBAUTHN_RP_ID / XIAOJU_WEBAUTHN_ORIGINS)")
	}

	sessions, err := auth.NewSessionsWithStore(ctx, cfg.AdminToken, store)
	if err != nil {
		return err
	}
	handler := api.New(svc, api.Options{
		UserID: userID, Token: cfg.AdminToken, CORSOrigins: cfg.CORSOrigins, Log: log,
		Passkeys: passkeys, Grid: grid, Sessions: sessions, Location: cfg.Location,
	}).Handler()
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
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
	case err := <-backupDone:
		return fmt.Errorf("backup telegram client stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// checkTelegram 连接 Telegram、以 Bot 登录并解析存储频道，不需要数据库。
func checkTelegram(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if err := cfg.ValidateTelegram(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := cfg.Telegram
	tgAPI, _, err := telegram.Start(ctx, telegram.ClientConfig{
		AppID: t.AppID, AppHash: t.AppHash, BotToken: t.BotToken, SessionFile: t.SessionFile,
		Transport: t.Transport,
	}, log)
	if err != nil {
		return err
	}
	if _, err := telegram.New(ctx, tgAPI, t.ChannelID); err != nil {
		return err
	}
	log.Info("telegram check passed: bot logged in and storage channel is accessible")
	if t.BackupEnabled() {
		if _, _, err := newBackupDriver(ctx, cfg, log); err != nil {
			return err
		}
		log.Info("telegram backup check passed: backup bot logged in and backup channel is accessible")
	}
	return nil
}

// diagnose 是没有数据库时的诊断模式：后台检查 Telegram 连接，同时在 HTTP 端口上展示结果，
// 让 PaaS 平台（如 Hugging Face Spaces）认为服务已启动，方便查看日志和结果。
func diagnose(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Warn("XIAOJU_DATABASE_URL is not set: running in diagnostic mode (telegram connectivity check only)")
	var mu sync.Mutex
	status := "telegram check: running...\n"
	go func() {
		result := "telegram check: OK\n"
		if err := checkTelegram(ctx, cfg, log); err != nil {
			log.Error("telegram check failed", "err", err)
			result = "telegram check: FAILED: " + err.Error() + "\n"
		}
		mu.Lock()
		status = result
		mu.Unlock()
	}()

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprint(w, "xiaoju-cloud diagnostic mode (XIAOJU_DATABASE_URL not set)\n"+status)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("listening (diagnostic mode)", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
	}, log.With("bot", botID(t.BotToken)))
	if err != nil {
		return nil, nil, err
	}
	d, err := telegram.New(ctx, tgAPI, t.ChannelID)
	if err != nil {
		return nil, nil, err
	}
	return withExtraBots(ctx, cfg, d, t.ChannelID, t.SessionFile, t.ExtraBotTokens, log), done, nil
}

// newBackupDriver 用另一个账号的 Bot 连接备份频道；未配置备份时返回 nil。
func newBackupDriver(ctx context.Context, cfg *config.Config, log *slog.Logger) (storage.Driver, <-chan error, error) {
	t := cfg.Telegram
	if cfg.Storage != "telegram" || !t.BackupEnabled() {
		return nil, nil, nil
	}
	if telegram.NormalizeChannelID(t.BackupChannelID) == telegram.NormalizeChannelID(t.ChannelID) {
		return nil, nil, errors.New("TG_BACKUP_CHANNEL_ID must be a different channel from TG_CHANNEL_ID")
	}
	blog := log.With("client", "backup")
	tgAPI, done, err := telegram.Start(ctx, telegram.ClientConfig{
		AppID: t.AppID, AppHash: t.AppHash, BotToken: t.BackupBotToken, SessionFile: t.BackupSessionFile,
		Transport: t.Transport,
	}, blog.With("bot", botID(t.BackupBotToken)))
	if err != nil {
		return nil, nil, fmt.Errorf("backup: %w", err)
	}
	d, err := telegram.New(ctx, tgAPI, t.BackupChannelID)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: %w", err)
	}
	log.Info("backup channel connected")
	return withExtraBots(ctx, cfg, d, t.BackupChannelID, t.BackupSessionFile, t.BackupExtraBotTokens, blog), done, nil
}

// withExtraBots 让额外的 Bot 和主 Bot 轮流用（见 telegram.Pool）；没配置时原样返回 d。
// 额外的 Bot 在后台一个个连接，能登录、能访问频道的才加进来；连不上、不是频道管理员的只记日志，不影响服务。
func withExtraBots(ctx context.Context, cfg *config.Config, d *telegram.Driver, channelID int64, sessionFile string, tokens []string, log *slog.Logger) storage.Driver {
	if len(tokens) == 0 {
		return d
	}
	pool := telegram.NewPool(d, log)
	t := cfg.Telegram
	go func() {
		seen := map[string]bool{botID(t.BotToken): true, botID(t.BackupBotToken): true}
		for _, token := range tokens {
			id := botID(token)
			blog := log.With("bot", id) // Bot 的数字 ID 不是秘密（令牌冒号后面那段才是）
			if id == "" || seen[id] {
				blog.Warn("extra bot skipped: not a bot token, or the same bot as another one")
				continue
			}
			seen[id] = true
			api, _, err := telegram.Start(ctx, telegram.ClientConfig{
				AppID: t.AppID, AppHash: t.AppHash, BotToken: token, Transport: t.Transport,
				SessionFile: filepath.Join(filepath.Dir(sessionFile), "tg-session-bot"+id+".json"),
			}, blog)
			if err == nil {
				var extra *telegram.Driver
				if extra, err = telegram.New(ctx, api, channelID); err == nil {
					pool.Add(extra)
					blog.Info("extra bot added", "bots", pool.Bots())
					continue
				}
			}
			if ctx.Err() != nil {
				return
			}
			blog.Warn("extra bot skipped", "err", err)
		}
	}()
	return pool
}

// botID 取 Bot 令牌冒号前面的数字（Bot 的用户 ID）；不像令牌时返回空。
func botID(token string) string {
	id, _, ok := strings.Cut(token, ":")
	if !ok || id == "" || strings.Trim(id, "0123456789") != "" {
		return ""
	}
	return id
}

// makeThumb 给本地文件生成缩略图（xiaoju thumb IN OUT.jpg）或大图预览（xiaoju preview IN OUT.jpg）。
func makeThumb(ctx context.Context, kind string, args []string, log *slog.Logger) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: xiaoju %s IN OUT.jpg", kind)
	}
	g := thumb.New("")
	log.Info("thumbnail tools", "found", g.Tools())
	src := func(context.Context) (io.ReadSeekCloser, error) { return os.Open(args[0]) }
	if kind == "preview" {
		b, err := g.Preview(ctx, src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[1], b, 0o644); err != nil {
			return err
		}
		log.Info("preview written", "out", args[1], "bytes", len(b))
		return nil
	}
	res, err := g.Generate(ctx, src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(args[1], res.JPEG, 0o644); err != nil {
		return err
	}
	log.Info("thumbnail written", "out", args[1], "bytes", len(res.JPEG), "duration", res.Duration, "taken", res.Taken)
	return nil
}
