package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Messenger 用存储的 Bot 给人发私信（小橘遥控移动侦测的提醒）。和存文件走同一条 MTProto 连接：
// Hugging Face 上 Telegram 的域名被屏蔽了，Bot API（api.telegram.org）连不上，只有直连数据中心的 MTProto 能用。
//
// 给谁发要知道对方的 access_hash。Bot 没法按用户名、ID 随便找人，所以从存储频道的管理员里找：
// 频道的创建者就是网盘的主人。别的人（TG_NOTIFY_CHAT_ID）也要是这个频道的成员才找得到。
// Bot 只能给对它发过 /start 的人发消息（Telegram 的规矩）。
type Messenger struct {
	api     *tg.Client
	channel *tg.InputChannel

	mu    sync.Mutex
	users map[int64]int64 // 用户 ID → access_hash
	owner int64
	name  string
}

// ErrNeedStart 对方还没给 Bot 发过 /start（或者把 Bot 屏蔽了）。
var ErrNeedStart = errors.New("the user has not started the bot")

// ErrUnknownUser 在频道里找不到这个人。
var ErrUnknownUser = errors.New("user is not a member of the storage channel")

// NeedStart Send 的错误是不是「对方还没给 Bot 发过 /start」。
func (m *Messenger) NeedStart(err error) bool { return errors.Is(err, ErrNeedStart) }

// Messenger 给这个驱动的 Bot 建一个发私信的。
func (d *Driver) Messenger() *Messenger {
	return &Messenger{api: d.api, channel: d.channel, users: map[int64]int64{}}
}

// BotName Bot 的用户名（不带 @），取不到返回空。
func (m *Messenger) BotName(ctx context.Context) string {
	m.mu.Lock()
	name := m.name
	m.mu.Unlock()
	if name != "" {
		return name
	}
	us, err := m.api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil || len(us) == 0 {
		return ""
	}
	if u, ok := us[0].(*tg.User); ok {
		m.mu.Lock()
		m.name = u.Username
		m.mu.Unlock()
		return u.Username
	}
	return ""
}

// Owner 存储频道的创建者（网盘的主人）。
func (m *Messenger) Owner(ctx context.Context) (int64, error) {
	m.mu.Lock()
	owner := m.owner
	m.mu.Unlock()
	if owner != 0 {
		return owner, nil
	}
	if err := m.load(ctx, &tg.ChannelParticipantsAdmins{}); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner == 0 {
		return 0, errors.New("storage channel has no creator visible to the bot")
	}
	return m.owner, nil
}

// load 读频道成员（管理员 / 最近的成员），记下每个人的 access_hash 和创建者。
func (m *Messenger) load(ctx context.Context, filter tg.ChannelParticipantsFilterClass) error {
	res, err := m.api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: m.channel, Filter: filter, Limit: 200,
	})
	if err != nil {
		return fmt.Errorf("list channel members: %w", err)
	}
	ps, ok := res.(*tg.ChannelsChannelParticipants)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range ps.Users {
		if u, ok := u.(*tg.User); ok && !u.Bot {
			if h, ok := u.GetAccessHash(); ok {
				m.users[u.ID] = h
			}
		}
	}
	for _, p := range ps.Participants {
		if c, ok := p.(*tg.ChannelParticipantCreator); ok {
			m.owner = c.UserID
		}
	}
	return nil
}

func (m *Messenger) peer(ctx context.Context, user int64) (*tg.InputPeerUser, error) {
	for _, filter := range []tg.ChannelParticipantsFilterClass{nil, &tg.ChannelParticipantsAdmins{}, &tg.ChannelParticipantsRecent{}} {
		if filter != nil {
			if err := m.load(ctx, filter); err != nil {
				return nil, err
			}
		}
		m.mu.Lock()
		h, ok := m.users[user]
		m.mu.Unlock()
		if ok {
			return &tg.InputPeerUser{UserID: user, AccessHash: h}, nil
		}
	}
	return nil, ErrUnknownUser
}

// Send 给 user 发一条消息；photo 不空时发图片（text 是图片说明）。
func (m *Messenger) Send(ctx context.Context, user int64, text string, photo []byte) error {
	p, err := m.peer(ctx, user)
	if err != nil {
		return err
	}
	randomID, err := randomInt64()
	if err != nil {
		return err
	}
	if len(photo) == 0 {
		_, err = m.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: p, Message: text, RandomID: randomID})
	} else {
		f, uerr := uploader.NewUploader(m.api).Upload(ctx, uploader.NewUpload("snapshot.jpg", bytes.NewReader(photo), int64(len(photo))))
		if uerr != nil {
			return fmt.Errorf("upload photo: %w", uerr)
		}
		_, err = m.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: p, Media: &tg.InputMediaUploadedPhoto{File: f}, Message: text, RandomID: randomID,
		})
	}
	if err != nil {
		// 没发过 /start、把 Bot 屏蔽了：Telegram 不让 Bot 先开口
		if tgerr.Is(err, "USER_IS_BLOCKED", "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED", "USER_BOT_REQUIRED") {
			return ErrNeedStart
		}
		return err
	}
	return nil
}
