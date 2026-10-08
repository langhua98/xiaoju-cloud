// Package telegram 是以 Telegram 私有频道为后端的存储驱动，走 MTProto（gotd/td），
// 不受 HTTP Bot API 50MB/20MB 的限制，单块最大 2GB，且支持按偏移读取。
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type ClientConfig struct {
	AppID       int
	AppHash     string
	BotToken    string
	SessionFile string
	// Transport 为 "websocket" 时经 wss://*.web.telegram.org 连接（走 443 端口和域名），
	// 适合禁止直连 Telegram IP、但允许访问网页版域名的环境；默认直连 TCP。
	Transport string
}

// Start 启动 MTProto 客户端并以 Bot 身份登录，返回可用的 API。
// 客户端在后台运行直到 ctx 取消；done 在客户端退出时收到其错误。
func Start(ctx context.Context, cfg ClientConfig, log *slog.Logger) (api *tg.Client, done <-chan error, err error) {
	if err := os.MkdirAll(filepath.Dir(cfg.SessionFile), 0o700); err != nil {
		return nil, nil, err
	}
	opts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionFile},
		Middlewares:    []telegram.Middleware{floodWait(log, 3, time.Minute)},
	}
	if cfg.Transport == "websocket" {
		opts.Resolver = dcs.Websocket(dcs.WebsocketOptions{})
	}
	client := telegram.NewClient(cfg.AppID, cfg.AppHash, opts)

	ready := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("auth status: %w", err)
			}
			if !status.Authorized {
				if _, err := client.Auth().Bot(ctx, cfg.BotToken); err != nil {
					return fmt.Errorf("bot login: %w", err)
				}
			}
			close(ready)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	log.Info("connecting to telegram", "transport", transportName(cfg.Transport))
	select {
	case <-ready:
		log.Info("telegram connected")
		return client.API(), errc, nil
	case err := <-errc:
		return nil, nil, fmt.Errorf("telegram client: %w", err)
	case <-time.After(connectTimeout):
		return nil, nil, fmt.Errorf("telegram: not connected after %s; the network may block Telegram (try TG_TRANSPORT=websocket)", connectTimeout)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

const connectTimeout = 90 * time.Second

func transportName(t string) string {
	if t == "" {
		return "tcp"
	}
	return t
}

// floodWait 在遇到 FLOOD_WAIT_X 时等待 X 秒后重试，最多 retries 次；等待时间超过 maxWait 直接返回错误。
func floodWait(log *slog.Logger, retries int, maxWait time.Duration) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for attempt := 0; ; attempt++ {
				err := next.Invoke(ctx, input, output)
				d, ok := tgerr.AsFloodWait(err)
				if !ok || attempt >= retries || d > maxWait {
					return err
				}
				log.Warn("telegram flood wait", "wait", d, "attempt", attempt+1)
				select {
				case <-time.After(d + time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	})
}
