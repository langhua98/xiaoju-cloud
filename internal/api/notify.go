package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 提醒（给「小橘遥控」的移动侦测用）：被控端发现有动静时，经服务端用存储的 Telegram Bot 给主人发一条私信，可以带一张截图。
// 服务端本来就连得上 Telegram：老手机不用自己连 Telegram，手机上也不用存 Bot 令牌。
//
//	POST /api/agent/v1/notify   要 upload 权限
//	{"text": "有动静", "photo": "<JPEG 的 base64，可选>", "chat": 123456（可选）}
//	→ {"ok": true, "chat": 123456}
//
// 走存文件的那条 MTProto 连接（见 telegram.Messenger）：Hugging Face 屏蔽了 Telegram 的域名，Bot API 连不上。
// 发给谁：请求里的 chat → TG_NOTIFY_CHAT_ID → 存储频道的创建者（网盘的主人）。对方都要在存储频道里。
// 找到的 chat 在回复里带回去，被控端记下来以后每次都带上。
// Bot 只能给对它发过 /start 的人发消息（Telegram 的规矩），没发过时回 409，提示给哪个 Bot 发。
// 整个服务端每 10 分钟最多 notifyBurst 条（被控端自己也有冷却），出问题时不会刷屏、也不会让 Bot 被 Telegram 限流。
const (
	notifyBurst    = 30
	notifyWindow   = 10 * time.Minute
	notifyMaxPhoto = 2 << 20
	notifyMaxText  = 1000 // 图片说明最多 1024 个字
	notifyTimeout  = 60 * time.Second
)

// TelegramSender 发 Telegram 私信（main 里用存储 Bot 的 MTProto 连接，见 telegram.Messenger）。
type TelegramSender interface {
	BotName(ctx context.Context) string
	Owner(ctx context.Context) (int64, error)
	Send(ctx context.Context, user int64, text string, photo []byte) error
	// NeedStart：Send 的错误是不是「对方还没给 Bot 发过 /start」
	NeedStart(err error) bool
}

type notifier struct {
	tg   TelegramSender
	chat int64 // TG_NOTIFY_CHAT_ID，0 = 发给存储频道的创建者

	mu   sync.Mutex
	sent []time.Time // 最近发过的时间（限流）
}

func newNotifier(tg TelegramSender, chat int64) *notifier {
	if tg == nil {
		return nil
	}
	return &notifier{tg: tg, chat: chat}
}

// allow 限流：最近 notifyWindow 里发了 notifyBurst 条就先不发。
func (n *notifier) allow(now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	keep := n.sent[:0]
	for _, t := range n.sent {
		if now.Sub(t) < notifyWindow {
			keep = append(keep, t)
		}
	}
	n.sent = keep
	if len(n.sent) >= notifyBurst {
		return false
	}
	n.sent = append(n.sent, now)
	return true
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeUpload) {
		return
	}
	n := s.notifier
	if n == nil {
		agentError(w, http.StatusNotImplemented, "notify_unconfigured", "Telegram is not connected",
			"存储用的是 Telegram 时才能发提醒。")
		return
	}
	var req struct {
		Text  string `json:"text"`
		Photo string `json:"photo"`
		Chat  int64  `json:"chat"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, notifyMaxPhoto*2)).Decode(&req); err != nil {
		agentError(w, http.StatusBadRequest, "invalid_body", "body must be JSON {text, photo?, chat?}", "")
		return
	}
	text := strings.TrimSpace(req.Text)
	if utf8.RuneCountInString(text) > notifyMaxText {
		text = string([]rune(text)[:notifyMaxText])
	}
	var photo []byte
	if req.Photo != "" {
		var err error
		if photo, err = base64.StdEncoding.DecodeString(req.Photo); err != nil || len(photo) == 0 {
			agentError(w, http.StatusBadRequest, "invalid_photo", "photo must be base64 JPEG", "")
			return
		}
		if len(photo) > notifyMaxPhoto {
			agentError(w, http.StatusRequestEntityTooLarge, "photo_too_large", "photo must be at most 2 MB", "")
			return
		}
	}
	if text == "" && photo == nil {
		agentError(w, http.StatusBadRequest, "empty", "text or photo is required", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
	defer cancel()
	chat := req.Chat
	if chat == 0 {
		chat = n.chat
	}
	if chat == 0 {
		var err error
		if chat, err = n.tg.Owner(ctx); err != nil {
			agentError(w, http.StatusBadGateway, "telegram_error", "cannot find the owner of the storage channel: "+err.Error(),
				"设置 TG_NOTIFY_CHAT_ID（你的 Telegram 数字 ID），而且你要在存储频道里。")
			return
		}
	}
	if !n.allow(time.Now()) {
		agentError(w, http.StatusTooManyRequests, "too_many", "too many notifications, try again later", "每 10 分钟最多 30 条。")
		return
	}
	if err := n.tg.Send(ctx, chat, text, photo); err != nil {
		if n.tg.NeedStart(err) {
			hint := "先在 Telegram 里给云盘的 Bot 发一句 /start。"
			if name := n.tg.BotName(ctx); name != "" {
				hint = "先在 Telegram 里给 @" + name + " 发一句 /start。"
			}
			agentError(w, http.StatusConflict, "need_start", "the bot cannot message you yet", hint)
			return
		}
		agentError(w, http.StatusBadGateway, "telegram_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat": chat})
}
