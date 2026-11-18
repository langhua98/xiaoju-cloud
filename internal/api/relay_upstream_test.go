package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/relay"
)

func TestStreamLink(t *testing.T) {
	video := &meta.File{ID: uuid.New(), Mime: "video/quicktime"}
	if New(nil, Options{Token: "t"}).streamLink(video, "x=1") != "" {
		t.Fatal("stream link without a relay configured")
	}
	s := New(nil, Options{Token: "t", RelayURL: "https://relay.example/"})
	if s.streamLink(video, "x=1") != "" {
		t.Fatal("stream link while the relay is offline")
	}
	s.videoRelay.ok.Store(true)
	if got, want := s.streamLink(video, "x=1"), "https://relay.example/rl/"+video.ID.String()+"?x=1"; got != want {
		t.Fatalf("stream link %q, want %q", got, want)
	}
	if s.streamLink(&meta.File{ID: uuid.New(), Mime: "image/jpeg"}, "x=1") != "" {
		t.Fatal("stream link for a photo")
	}
}

func TestRelayIndexNeedsKey(t *testing.T) {
	h := New(nil, Options{Token: "t", RelayURL: "https://relay.example"}).Handler()
	for _, key := range []string{"", relay.Key("other token")} {
		req := httptest.NewRequest("GET", "/relay/files/"+uuid.NewString()+"?exp=9999999999&sig=x", nil)
		if key != "" {
			req.Header.Set(relay.KeyHeader, key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("key %q: status %d", key, rec.Code)
		}
	}
	// 钥匙对、签名不对
	req := httptest.NewRequest("GET", "/relay/files/"+uuid.NewString()+"?exp=9999999999&sig=x", nil)
	req.Header.Set(relay.KeyHeader, relay.Key("t"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad sig: status %d", rec.Code)
	}
}

// agent 接口（小橘推送看录像）拿到的中转地址，签名要能通过家里电脑给中转的索引检查（relayIndex 用 linkSig 核对）。
func TestRelayStreamSignature(t *testing.T) {
	video := &meta.File{ID: uuid.New(), Mime: "video/quicktime"}
	s := New(nil, Options{Token: "t", RelayURL: "https://relay.example"})
	if s.relayStream(video) != "" {
		t.Fatal("relay stream while the relay is offline")
	}
	s.videoRelay.ok.Store(true)
	st := s.relayStream(video)
	u, err := url.Parse(st)
	if err != nil || u.Host != "relay.example" || u.Path != "/rl/"+video.ID.String() {
		t.Fatalf("relay stream %q", st)
	}
	exp, err := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if err != nil || exp < time.Now().Add(downloadLinkTTL-time.Minute).Unix() {
		t.Fatalf("relay stream expiry %q", st)
	}
	if u.Query().Get("sig") != s.linkSig(video.ID, exp) {
		t.Fatalf("relay stream signature %q", st)
	}
	if s.relayStream(&meta.File{ID: uuid.New(), Mime: "audio/wav"}) != "" {
		t.Fatal("relay stream for audio")
	}
}
