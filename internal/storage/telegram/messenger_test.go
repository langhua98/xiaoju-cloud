package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// fakeTG 假的 MTProto：频道里有主人 555（创建者）、管理员 777、Bot 999；只有 started 里的人能收到私信。
type fakeTG struct {
	mu       sync.Mutex
	started  map[int64]bool
	sent     []string // "to:类型:文字"
	listings int
	parts    int
}

func user(id int64, hash int64, bot bool) *tg.User {
	u := &tg.User{ID: id, Bot: bot, Username: fmt.Sprintf("u%d", id)}
	u.SetAccessHash(hash)
	return u
}

func reply(output bin.Decoder, v bin.Encoder) error {
	var b bin.Buffer
	if err := v.Encode(&b); err != nil {
		return err
	}
	return output.Decode(&b)
}

func (f *fakeTG) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r := input.(type) {
	case *tg.ChannelsGetParticipantsRequest:
		f.listings++
		return reply(output, &tg.ChannelsChannelParticipants{
			Count: 3,
			Participants: []tg.ChannelParticipantClass{
				&tg.ChannelParticipantCreator{UserID: 555},
				&tg.ChannelParticipantAdmin{UserID: 777},
				&tg.ChannelParticipantAdmin{UserID: 999},
			},
			Users: []tg.UserClass{user(555, 11, false), user(777, 22, false), user(999, 33, true)},
		})
	case *tg.UsersGetUsersRequest:
		return reply(output, &tg.UserClassVector{Elems: []tg.UserClass{user(999, 33, true)}})
	case *tg.UploadSaveFilePartRequest:
		f.parts++
		return reply(output, &tg.BoolTrue{})
	case *tg.MessagesSendMessageRequest:
		p := r.Peer.(*tg.InputPeerUser)
		if !f.started[p.UserID] {
			return tgerr.New(400, "PEER_ID_INVALID")
		}
		f.sent = append(f.sent, fmt.Sprintf("%d/%d:text:%s", p.UserID, p.AccessHash, r.Message))
		return reply(output, &tg.Updates{})
	case *tg.MessagesSendMediaRequest:
		p := r.Peer.(*tg.InputPeerUser)
		if !f.started[p.UserID] {
			return tgerr.New(403, "USER_IS_BLOCKED")
		}
		if _, ok := r.Media.(*tg.InputMediaUploadedPhoto); !ok {
			return fmt.Errorf("want an uploaded photo, got %T", r.Media)
		}
		f.sent = append(f.sent, fmt.Sprintf("%d/%d:photo:%s", p.UserID, p.AccessHash, r.Message))
		return reply(output, &tg.Updates{})
	}
	return fmt.Errorf("unexpected %T", input)
}

func TestMessengerFindsOwnerAndSends(t *testing.T) {
	f := &fakeTG{started: map[int64]bool{}}
	m := (&Driver{api: tg.NewClient(f), channel: &tg.InputChannel{ChannelID: 1, AccessHash: 2}}).Messenger()
	ctx := context.Background()

	owner, err := m.Owner(ctx)
	if err != nil || owner != 555 {
		t.Fatalf("owner: %d %v", owner, err)
	}
	if name := m.BotName(ctx); name != "u999" {
		t.Fatalf("bot name: %q", name)
	}
	// 还没 /start：两种错误都算「要先 /start」
	if err := m.Send(ctx, 555, "hi", nil); !m.NeedStart(err) {
		t.Fatalf("text before start: %v", err)
	}
	if err := m.Send(ctx, 555, "hi", []byte{1, 2, 3}); !m.NeedStart(err) {
		t.Fatalf("photo before start: %v", err)
	}
	f.started[555] = true
	f.started[777] = true
	if err := m.Send(ctx, 555, "有动静", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(ctx, 777, "客厅", []byte{0xff, 0xd8, 9}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"555/11:text:有动静", "777/22:photo:客厅"}; fmt.Sprint(f.sent) != fmt.Sprint(want) {
		t.Fatalf("sent %v, want %v", f.sent, want)
	}
	if f.parts == 0 {
		t.Fatal("photo was not uploaded")
	}
	// 频道里没有的人、Bot 自己：找不到
	if err := m.Send(ctx, 12345, "x", nil); err != ErrUnknownUser {
		t.Fatalf("stranger: %v", err)
	}
	if err := m.Send(ctx, 999, "x", nil); err != ErrUnknownUser {
		t.Fatalf("bot itself: %v", err)
	}
	// access_hash 记住了：发给已知的人不用再列成员
	before := f.listings
	m.Send(ctx, 555, "again", nil)
	if f.listings != before {
		t.Fatal("members listed again for a known user")
	}
}
