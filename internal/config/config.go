// Package config 从环境变量加载配置。
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
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
}

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
		},
	}

	var err error
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
	return nil
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
