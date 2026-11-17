package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
