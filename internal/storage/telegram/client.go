// Package telegram 是以 Telegram 私有频道为后端的存储驱动，走 MTProto（gotd/td），
// 不受 HTTP Bot API 50MB/20MB 的限制，单块最大 2GB，且支持按偏移读取。
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/time/rate"
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
		Middlewares:    []telegram.Middleware{floodWait(log, 3, time.Minute), pace(paceEvery, paceBurst)},
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
// 请求经 Pool 发出、还有别的 Bot 能接手时（见 withHandoff），等待超过 handoff 的也直接返回，由 Pool 换个 Bot 做。
func floodWait(log *slog.Logger, retries int, maxWait time.Duration) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for attempt := 0; ; attempt++ {
				err := next.Invoke(ctx, input, output)
				d, ok := tgerr.AsFloodWait(err)
				if !ok {
					return err
				}
				handoff, pooled := ctx.Value(handoffKey{}).(time.Duration)
				giveUp := attempt >= retries || d > maxWait || (pooled && d > handoff)
				log.Warn("telegram flood wait", "method", method(input), "wait", d, "attempt", attempt+1, "give_up", giveUp)
				if giveUp {
					return err
				}
				select {
				case <-time.After(d + time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	})
}

type handoffKey struct{}

// withHandoff 让这次请求遇到超过 d 的 FLOOD_WAIT 时不在原地等，直接把错误交回给 Pool。
func withHandoff(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, handoffKey{}, d)
}

// method 是请求的名字（比如 MessagesSendMedia），写进日志，看是哪种请求被限速。
func method(input bin.Encoder) string {
	return strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%T", input), "*tg."), "Request")
}

// 频道消息类请求的节奏：平均每 100ms 一个，最多连着 5 个（Teldrive 1.x 默认的 gotd ratelimit 设置）。
const (
	paceEvery = 100 * time.Millisecond
	paceBurst = 5
)

// 发消息（每个分块一条）另外再排一次队：Telegram 对 Bot 往同一个群发消息的建议是每分钟不超过 20 条
// （core.telegram.org/bots/faq），平均每 3 秒一条、最多连着 3 条，在本地排着比撞上 FLOOD_WAIT 再等更快，
// 反复撞限速还会让 Telegram 要求等的时间越来越长。每个 Bot 各排各的，Bot 多了总的发送速度跟着涨。
const (
	sendEvery = 3 * time.Second
	sendBurst = 3
)

// pace 主动控制请求节奏，不等 Telegram 回 FLOOD_WAIT 才停：发消息、取消息、删消息这类请求排匀了再发
// （对账扫描几千个消息 ID、批量删除时最容易一下子发太密）。上传、下载文件分片的请求不限，它们靠并发提速，
// Telegram 对它们的限制也宽得多。发消息再按 sendEvery 单独排队。
func pace(every time.Duration, burst int) telegram.Middleware {
	lim := rate.NewLimiter(rate.Every(every), burst)
	send := rate.NewLimiter(rate.Every(sendEvery), sendBurst)
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			if _, ok := input.(*tg.MessagesSendMediaRequest); ok {
				if err := send.Wait(ctx); err != nil {
					return err
				}
			}
			if !isFileTransfer(input) {
				if err := lim.Wait(ctx); err != nil {
					return err
				}
			}
			return next.Invoke(ctx, input, output)
		}
	})
}

func isFileTransfer(input bin.Encoder) bool {
	switch input.(type) {
	case *tg.UploadGetFileRequest, *tg.UploadSaveFilePartRequest, *tg.UploadSaveBigFilePartRequest:
		return true
	}
	return false
}
