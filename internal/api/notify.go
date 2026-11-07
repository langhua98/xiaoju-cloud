package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 提醒（给「小橘遥控」的移动侦测用）：被控端发现有动静时，经服务端用 Telegram 机器人给主人发一条消息，可以带一张截图。
// 服务端本来就连得上 Telegram：老手机不用自己连 Telegram，手机上也不用存机器人令牌。
//
//	POST /api/agent/v1/notify   要 upload 权限
//	{"text": "有动静", "photo": "<JPEG 的 base64，可选>", "chat": 123456（可选）}
//	→ {"ok": true, "chat": 123456}
//
// 用哪个机器人：TG_NOTIFY_BOT_TOKEN（专门发提醒的），没设就用存文件的 TG_BOT_TOKEN。走 Bot API（HTTPS），不碰存储用的连接。
// 发给谁：请求里的 chat → TG_NOTIFY_CHAT_ID → 最近给这个机器人发过消息（比如 /start）的私聊。
// 找到的 chat 在回复里带回去，被控端记下来以后每次都带上（服务端重启了、Telegram 那边的消息过期了也不怕）。
// 机器人只能给对它发过 /start 的人发消息（Telegram 的规矩），所以 chat 填成别人的也发不出去。
// 整个服务端每 10 分钟最多发 notifyBurst 条（被控端自己也有冷却），出问题时不会刷屏、也不会让机器人被 Telegram 限流。
const (
	notifyBurst     = 30
	notifyWindow    = 10 * time.Minute
	notifyMaxPhoto  = 2 << 20
	notifyMaxText   = 1000 // 图片说明最多 1024 个字
	notifyTelegram  = "https://api.telegram.org"
	notifyCallLimit = 30 * time.Second
)

type notifier struct {
	api    string // Bot API 地址（测试时换成假的）
	token  string
	chat   int64 // TG_NOTIFY_CHAT_ID，0 = 没设
	client *http.Client

	mu    sync.Mutex
	found int64       // 从 getUpdates 找到的私聊
	bot   string      // 机器人的用户名（提示「给 @xxx 发 /start」用）
	sent  []time.Time // 最近发过的时间（限流）
}

func newNotifier(token string, chat int64) *notifier {
	if token == "" {
		return nil
	}
	return &notifier{api: notifyTelegram, token: token, chat: chat, client: &http.Client{Timeout: notifyCallLimit}}
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

// tgResult 是 Bot API 的回复。
type tgResult struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (n *notifier) call(ctx context.Context, method, contentType string, body io.Reader) (json.RawMessage, error) {
	u := n.api + "/bot" + n.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// 错误信息里有完整网址（带令牌），不能原样传出去
		return nil, errors.New("连不上 Telegram")
	}
	defer resp.Body.Close()
	var r tgResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("Telegram 回了看不懂的内容（HTTP %d）", resp.StatusCode)
	}
	if !r.OK {
		return nil, fmt.Errorf("Telegram：%s", r.Description)
	}
	return r.Result, nil
}

// botName 机器人的用户名，取一次记住；取不到返回空。
func (n *notifier) botName(ctx context.Context) string {
	n.mu.Lock()
	name := n.bot
	n.mu.Unlock()
	if name != "" {
		return name
	}
	raw, err := n.call(ctx, "getMe", "", nil)
	if err != nil {
		return ""
	}
	var me struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(raw, &me) != nil || me.Username == "" {
		return ""
	}
	n.mu.Lock()
	n.bot = me.Username
	n.mu.Unlock()
	return me.Username
}

// target 发给谁。0 = 还没人给机器人发过消息。
func (n *notifier) target(ctx context.Context, want int64) (int64, error) {
	if want != 0 {
		return want, nil
	}
	if n.chat != 0 {
		return n.chat, nil
	}
	n.mu.Lock()
	found := n.found
	n.mu.Unlock()
	if found != 0 {
		return found, nil
	}
	// 不带 offset：只看不确认，别的程序（如果有）照样收得到这些消息
	raw, err := n.call(ctx, "getUpdates", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"allowed_updates": {`["message"]`}, "timeout": {"0"}}.Encode()))
	if err != nil {
		return 0, err
	}
	var ups []struct {
		Message *struct {
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &ups); err != nil {
		return 0, errors.New("Telegram 回了看不懂的内容")
	}
	for i := len(ups) - 1; i >= 0; i-- { // 最近的那个私聊
		if m := ups[i].Message; m != nil && m.Chat.Type == "private" {
			n.mu.Lock()
			n.found = m.Chat.ID
			n.mu.Unlock()
			return m.Chat.ID, nil
		}
	}
	return 0, nil
}

func (n *notifier) send(ctx context.Context, chat int64, text string, photo []byte) error {
	id := strconv.FormatInt(chat, 10)
	if len(photo) == 0 {
		_, err := n.call(ctx, "sendMessage", "application/x-www-form-urlencoded",
			strings.NewReader(url.Values{"chat_id": {id}, "text": {text}}.Encode()))
		return err
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("chat_id", id)
	if text != "" {
		mw.WriteField("caption", text)
	}
	fw, err := mw.CreateFormFile("photo", "snapshot.jpg")
	if err != nil {
		return err
	}
	fw.Write(photo)
	mw.Close()
	_, err = n.call(ctx, "sendPhoto", mw.FormDataContentType(), &b)
	return err
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	if !s.need(w, r, ScopeUpload) {
		return
	}
	n := s.notifier
	if n == nil {
		agentError(w, http.StatusNotImplemented, "notify_unconfigured", "no Telegram bot is configured",
			"服务端要有 TG_NOTIFY_BOT_TOKEN 或 TG_BOT_TOKEN。")
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
	for utf8.RuneCountInString(text) > notifyMaxText {
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
	ctx := r.Context()
	chat, err := n.target(ctx, req.Chat)
	if err != nil {
		agentError(w, http.StatusBadGateway, "telegram_error", err.Error(), "")
		return
	}
	if chat == 0 {
		hint := "先在 Telegram 里给云盘的机器人发一句 /start。"
		if name := n.botName(ctx); name != "" {
			hint = "先在 Telegram 里给 @" + name + " 发一句 /start。"
		}
		agentError(w, http.StatusConflict, "need_start", "nobody has messaged the bot yet", hint)
		return
	}
	if !n.allow(time.Now()) {
		agentError(w, http.StatusTooManyRequests, "too_many", "too many notifications, try again later",
			"每 10 分钟最多 30 条。")
		return
	}
	if err := n.send(ctx, chat, text, photo); err != nil {
		agentError(w, http.StatusBadGateway, "telegram_error", err.Error(),
			"机器人只能给对它发过 /start 的人发消息。")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat": chat})
}
