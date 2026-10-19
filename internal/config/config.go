// Package config 从环境变量加载配置。
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	MiB = 1 << 20
	// Telegram 单文件上限 2GB（4000 × 512KB），留一点余量。
	MaxTelegramPartSize = 2000 * MiB
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	AdminToken  string
	MasterKey   []byte // 32 字节，用于包裹每个文件的 DEK
	Storage     string // telegram | local
	PartSize    int64
	TempDir     string
	// CORSOrigins 允许跨域访问 API 的网页来源，如 https://user.github.io；"*" 表示任意来源。
	CORSOrigins []string

	// 刷脸（通行密钥）：绑定网页所在的域名。默认取 CORSOrigins 里第一个具体来源。
	WebAuthnRPID    string
	WebAuthnOrigins []string
	// PasskeyReset 为 true 时启动时清空所有已注册设备和格子口令（全部丢失时的紧急恢复）。
	PasskeyReset bool
	// Location 是格子口令判断“出题时是星期几”用的时区（XIAOJU_TIMEZONE，默认 Asia/Shanghai）。
	// 照片的 EXIF 没写时区时，拍摄时间也按这个时区算。
	Location *time.Location

	LocalDir string

	Telegram Telegram
}

type Telegram struct {
	AppID       int
	AppHash     string
	BotToken    string
	ChannelID   int64
	SessionFile string
	Transport   string // tcp | websocket

	// 备份：另一个账号的 Bot 和频道。两项都设置时启用双备份。
	BackupBotToken    string
	BackupChannelID   int64
	BackupSessionFile string

	// 额外的 Bot（可选）：也是同一个频道的管理员，和主 Bot 轮流上传、下载，不容易被 Telegram 限速。
	// TG_EXTRA_BOT_TOKENS 给存储频道，TG_BACKUP_EXTRA_BOT_TOKENS 给备份频道；逗号、空格或换行分隔。
	ExtraBotTokens       []string
	BackupExtraBotTokens []string
}

// BackupEnabled 报告是否配置了备份频道。
func (t Telegram) BackupEnabled() bool { return t.BackupBotToken != "" || t.BackupChannelID != 0 }

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    env("XIAOJU_HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("XIAOJU_DATABASE_URL"),
		AdminToken:  os.Getenv("XIAOJU_ADMIN_TOKEN"),
		Storage:     env("XIAOJU_STORAGE", "telegram"),
		TempDir:     env("XIAOJU_TMP_DIR", os.TempDir()),
		LocalDir:    env("XIAOJU_LOCAL_DIR", "./data/blobs"),
		Telegram: Telegram{
			AppHash:     os.Getenv("TG_APP_HASH"),
			BotToken:    os.Getenv("TG_BOT_TOKEN"),
			SessionFile: env("TG_SESSION_FILE", "./data/tg-session.json"),
			Transport:   env("TG_TRANSPORT", "tcp"),

			BackupBotToken:    os.Getenv("TG_BACKUP_BOT_TOKEN"),
			BackupSessionFile: env("TG_BACKUP_SESSION_FILE", "./data/tg-backup-session.json"),

			ExtraBotTokens:       strings.Fields(strings.ReplaceAll(os.Getenv("TG_EXTRA_BOT_TOKENS"), ",", " ")),
			BackupExtraBotTokens: strings.Fields(strings.ReplaceAll(os.Getenv("TG_BACKUP_EXTRA_BOT_TOKENS"), ",", " ")),
		},
	}

	c.CORSOrigins = splitList(os.Getenv("XIAOJU_CORS_ORIGINS"))

	c.WebAuthnOrigins = splitList(os.Getenv("XIAOJU_WEBAUTHN_ORIGINS"))
	if len(c.WebAuthnOrigins) == 0 {
		for _, o := range c.CORSOrigins {
			if o != "*" {
				c.WebAuthnOrigins = append(c.WebAuthnOrigins, o)
			}
		}
	}
	c.WebAuthnRPID = os.Getenv("XIAOJU_WEBAUTHN_RP_ID")
	if c.WebAuthnRPID == "" && len(c.WebAuthnOrigins) > 0 {
		if u, err := url.Parse(c.WebAuthnOrigins[0]); err == nil {
			c.WebAuthnRPID = u.Hostname()
		}
	}
	c.PasskeyReset, _ = strconv.ParseBool(os.Getenv("XIAOJU_PASSKEY_RESET"))

	var err error
	tz := env("XIAOJU_TIMEZONE", "Asia/Shanghai")
	if c.Location, err = time.LoadLocation(tz); err != nil {
		return nil, fmt.Errorf("XIAOJU_TIMEZONE: %w", err)
	}
	if c.PartSize, err = envInt64("XIAOJU_PART_SIZE", 256*MiB); err != nil {
		return nil, err
	}
	if v := os.Getenv("TG_APP_ID"); v != "" {
		if c.Telegram.AppID, err = strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("TG_APP_ID: %w", err)
		}
	}
	if v := os.Getenv("TG_CHANNEL_ID"); v != "" {
		if c.Telegram.ChannelID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return nil, fmt.Errorf("TG_CHANNEL_ID: %w", err)
		}
	}
	if v := os.Getenv("TG_BACKUP_CHANNEL_ID"); v != "" {
		if c.Telegram.BackupChannelID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return nil, fmt.Errorf("TG_BACKUP_CHANNEL_ID: %w", err)
		}
	}
	if v := os.Getenv("XIAOJU_MASTER_KEY"); v != "" {
		if c.MasterKey, err = hex.DecodeString(v); err != nil {
			return nil, fmt.Errorf("XIAOJU_MASTER_KEY: %w", err)
		}
	}
	return c, nil
}

// ValidateDB 只校验数据库相关配置（migrate 命令用）。
func (c *Config) ValidateDB() error {
	if c.DatabaseURL == "" {
		return errors.New("XIAOJU_DATABASE_URL is required")
	}
	return nil
}

// Validate 校验 serve 命令所需的全部配置。
func (c *Config) Validate() error {
	if err := c.ValidateDB(); err != nil {
		return err
	}
	if c.AdminToken == "" {
		return errors.New("XIAOJU_ADMIN_TOKEN is required")
	}
	if len(c.MasterKey) != 32 {
		return errors.New("XIAOJU_MASTER_KEY must be 64 hex chars (32 bytes), e.g. `openssl rand -hex 32`")
	}
	if c.PartSize <= 0 {
		return errors.New("XIAOJU_PART_SIZE must be positive")
	}
	switch c.Storage {
	case "local":
	case "telegram":
		if c.PartSize > MaxTelegramPartSize {
			return fmt.Errorf("XIAOJU_PART_SIZE must be <= %d for telegram storage", int64(MaxTelegramPartSize))
		}
		return c.ValidateTelegram()
	default:
		return fmt.Errorf("unknown XIAOJU_STORAGE %q (want telegram or local)", c.Storage)
	}
	return nil
}

// ValidateTelegram 校验 Telegram 连接所需配置（check 命令用）。
func (c *Config) ValidateTelegram() error {
	t := c.Telegram
	if t.AppID == 0 || t.AppHash == "" || t.BotToken == "" || t.ChannelID == 0 {
		return errors.New("TG_APP_ID, TG_APP_HASH, TG_BOT_TOKEN and TG_CHANNEL_ID are required for telegram storage")
	}
	if t.Transport != "tcp" && t.Transport != "websocket" {
		return fmt.Errorf("unknown TG_TRANSPORT %q (want tcp or websocket)", t.Transport)
	}
	if t.BackupEnabled() {
		if t.BackupBotToken == "" || t.BackupChannelID == 0 {
			return errors.New("TG_BACKUP_BOT_TOKEN and TG_BACKUP_CHANNEL_ID must be set together")
		}
		if t.BackupBotToken == t.BotToken {
			return errors.New("TG_BACKUP_BOT_TOKEN must be a different bot (created by another Telegram account)")
		}
	}
	return nil
}

// splitList 解析逗号分隔的网址列表，去掉空白和结尾的 /
func splitList(v string) []string {
	var out []string
	for _, o := range strings.Split(v, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
