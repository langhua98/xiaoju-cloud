package telegram

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestNormalizeChannelID(t *testing.T) {
	cases := map[int64]int64{
		-1001234567890: 1234567890,
		1234567890:     1234567890,
		-1234567:       1234567,
	}
	for in, want := range cases {
		if got := NormalizeChannelID(in); got != want {
			t.Errorf("NormalizeChannelID(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestAlignChunk(t *testing.T) {
	cases := []struct{ off, aligned, skip int64 }{
		{0, 0, 0},
		{1, 0, 1},
		{chunkSize - 1, 0, chunkSize - 1},
		{chunkSize, chunkSize, 0},
		{3*chunkSize + 4097, 3 * chunkSize, 4097},
	}
	for _, c := range cases {
		a, s := alignChunk(c.off)
		if a != c.aligned || s != c.skip {
			t.Errorf("alignChunk(%d) = (%d,%d), want (%d,%d)", c.off, a, s, c.aligned, c.skip)
		}
		if a%chunkSize != 0 {
			t.Errorf("aligned offset %d not 1MB aligned", a)
		}
	}
}

func TestExtractMessageID(t *testing.T) {
	upd := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 99}},
		&tg.UpdateMessageID{ID: 42, RandomID: 7},
	}}
	if id, err := extractMessageID(upd, 7); err != nil || id != 42 {
		t.Fatalf("got %d, %v; want 42", id, err)
	}
	// 没有匹配 random_id 时退回到新频道消息
	if id, err := extractMessageID(upd, 8); err != nil || id != 99 {
		t.Fatalf("got %d, %v; want 99", id, err)
	}
	if _, err := extractMessageID(&tg.Updates{}, 1); err == nil {
		t.Fatal("expected error for empty updates")
	}
}
