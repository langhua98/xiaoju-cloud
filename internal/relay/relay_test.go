package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
)

// 两个加密分块存进本地驱动（代替 Telegram），假的家里电脑给索引；经中转拿回来的应该是原文，Range 也对。
func TestRelay(t *testing.T) {
	master := bytes.Repeat([]byte{1}, 32)
	drv, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 3<<20)
	rand.Read(plain)
	dek, _ := crypto.NewKey()
	wrapped, _ := crypto.WrapKey(master, dek)
	id := uuid.New()
	var parts []meta.Part
	for i, chunk := range [][]byte{plain[:2<<20], plain[2<<20:]} {
		iv, _ := crypto.NewIV()
		st, _ := crypto.NewCTRAt(dek, iv, 0)
		enc := make([]byte, len(chunk))
		st.XORKeyStream(enc, chunk)
		loc, err := drv.Put(context.Background(), storage.PutRequest{Name: "p", Body: bytes.NewReader(enc), Size: int64(len(enc))})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, meta.Part{FileID: id, PartNo: i, ChannelID: loc.ChannelID, MessageID: loc.MessageID, Size: int64(len(chunk)), IV: iv})
	}

	const token = "admin"
	asked := 0
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get(KeyHeader) != Key(token) || r.URL.Query().Get("sig") != "good" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(Meta{Name: "a.mov", Mime: "video/quicktime", Size: int64(len(plain)),
			UpdatedAt: time.Now(), DEKWrapped: wrapped, Parts: parts})
	}))
	defer home.Close()
	rs := httptest.NewServer(New(Options{Upstream: home.URL, AdminToken: token, MasterKey: master, Driver: drv}).Handler())
	defer rs.Close()
	exp := time.Now().Add(time.Hour).Unix()
	u := rs.URL + "/rl/" + id.String() + "?exp=" + itoa(exp) + "&sig=good&inline=1"

	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, plain) || resp.Header.Get("Content-Type") != "video/quicktime" {
		t.Fatalf("full: status %d, %d bytes, type %q", resp.StatusCode, len(got), resp.Header.Get("Content-Type"))
	}
	// 跨两个分块的 Range（拖动进度条）
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Range", "bytes=2097000-2098000")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(got, plain[2097000:2098001]) {
		t.Fatalf("range: status %d, %d bytes", resp.StatusCode, len(got))
	}
	if asked != 1 {
		t.Fatalf("asked home %d times, want 1 (index cached)", asked)
	}
	// 签名不对：电脑拒绝，中转也拒绝
	resp, _ = http.Get(rs.URL + "/rl/" + id.String() + "?exp=" + itoa(exp) + "&sig=bad")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad sig: status %d", resp.StatusCode)
	}
	// 过期的链接不用问电脑
	resp, _ = http.Get(rs.URL + "/rl/" + id.String() + "?exp=1&sig=good")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || asked != 2 {
		t.Fatalf("expired: status %d, asked %d", resp.StatusCode, asked)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
