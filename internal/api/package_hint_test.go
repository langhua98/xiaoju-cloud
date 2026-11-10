package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

func TestRelayHubHintOnlyToSubscribedBoxes(t *testing.T) {
	h := newRelayHub()
	now := time.Now()
	h.wantUpdates("遥控/A/收件", "android", now)
	h.take("遥控/iPhone/收件", now) // 收信但没要提醒
	h.wantUpdates("遥控/旧/收件", "android", now.Add(-relayIdleExpire-time.Minute))
	if n := h.hint("android", json.RawMessage(`{"do":"update_hint"}`), now); n != 1 {
		t.Fatalf("hinted %d boxes, want 1", n)
	}
	if msgs, _ := h.take("遥控/A/收件", now); len(msgs) != 1 {
		t.Fatalf("subscribed box: %v", msgs)
	}
	if msgs, _ := h.take("遥控/iPhone/收件", now); msgs != nil {
		t.Fatalf("unsubscribed box got %v", msgs)
	}
	if h.hint("ios", json.RawMessage(`{}`), now) != 0 {
		t.Fatal("other kinds must not be hinted")
	}
}

// 集成测试：往「软件包」传 .apk，带 &updates=android 等着的信箱马上收到提醒；传到别处、不是 apk 的不提醒。
func TestPackageUploadHintsDevices(t *testing.T) {
	dsn := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	driver, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "it-"+t.Name()+"-"+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	srv := New(svc, Options{UserID: userID, Token: "tok", Log: log})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	do := func(method, path string, body io.Reader) []byte {
		req, _ := http.NewRequest(method, ts.URL+path, body)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return b
	}
	var pkg meta.File
	json.Unmarshal(do("POST", "/api/folders", strings.NewReader(`{"name":"软件包"}`)), &pkg)
	var other meta.File
	json.Unmarshal(do("POST", "/api/folders", strings.NewReader(`{"name":"别的"}`)), &other)

	box := "遥控/A/收件"
	// 被控端：收信时说要安卓安装包提醒（直接用 hub，省掉 agent 令牌）
	srv.relay.wantUpdates(box, "android", time.Now())
	upload := func(parent *meta.File, name string) {
		do("POST", "/api/files?name="+url.QueryEscape(name)+"&parent_id="+parent.ID.String(), bytes.NewReader([]byte("PK fake apk")))
	}
	waitMsgs := func(d time.Duration) []relayMsg {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if msgs, _ := srv.relay.take(box, time.Now()); msgs != nil {
				return msgs
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}

	upload(&other, "a.apk")
	upload(&pkg, "readme.txt")
	if msgs := waitMsgs(300 * time.Millisecond); msgs != nil {
		t.Fatalf("unexpected hint: %s", msgs[0].Body)
	}
	upload(&pkg, "app-release.apk")
	msgs := waitMsgs(3 * time.Second)
	if len(msgs) != 1 {
		t.Fatalf("hints: %v", msgs)
	}
	var hint map[string]any
	json.Unmarshal(msgs[0].Body, &hint)
	if hint["do"] != "update_hint" || hint["name"] != "app-release.apk" {
		t.Fatalf("hint body: %s", msgs[0].Body)
	}
}
