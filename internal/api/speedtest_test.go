package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpeedTest(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(nil, Options{Token: "tok", Log: log}).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/speedtest/ping")
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ping: %v %v", err, resp)
	}

	resp, err = http.Get(ts.URL + "/api/speedtest/down?bytes=3000000")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != 3000000 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("down: got %d bytes, cache %q", n, resp.Header.Get("Cache-Control"))
	}

	resp, err = http.Post(ts.URL+"/api/speedtest/up", "application/octet-stream", bytes.NewReader(make([]byte, 123456)))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Bytes int64 }
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != 200 || got.Bytes != 123456 {
		t.Fatalf("up: status %d, bytes %d", resp.StatusCode, got.Bytes)
	}

	resp, _ = http.Post(ts.URL+"/api/speedtest/up", "application/octet-stream", bytes.NewReader(make([]byte, speedTestMax+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload: status %d", resp.StatusCode)
	}
}
