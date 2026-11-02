package api

import (
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

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/gdrive"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 连接谷歌云盘：要授权地址 → 谷歌跳回 /gdrive/callback → 状态里显示已连接；state 只能用一次。
func TestGDriveConnect(t *testing.T) {
	dbURL := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	driver, _ := local.New(t.TempDir())
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "it-"+t.Name())
	_ = store.DeleteGDriveAccount(ctx, userID)
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})

	var gotRedirect string
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
				return
			}
			gotRedirect = r.Form.Get("redirect_uri")
			if r.Form.Get("code") != "good" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/api/about":
			w.Write([]byte(`{"user":{"emailAddress":"me@example.com"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer google.Close()
	oauth := &gdrive.OAuth{ClientID: "cid", ClientSecret: "sec",
		Endpoints: gdrive.Endpoints{Auth: google.URL + "/auth", Token: google.URL + "/token", API: google.URL + "/api", Upload: google.URL + "/up"}}
	sync := gdrive.New(oauth, svc, userID, master, log)
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log, GDrive: sync, PublicURL: "https://xj.example/"}).Handler())
	defer ts.Close()

	call := func(method, path string) map[string]any {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	st := call("GET", "/api/gdrive")
	if st["configured"] != true || st["connected"] != false || st["redirect_uri"] != "https://xj.example/gdrive/callback" {
		t.Fatalf("status %v", st)
	}
	u, _ := url.Parse(call("POST", "/api/gdrive/connect")["url"].(string))
	state := u.Query().Get("state")
	if u.Query().Get("redirect_uri") != "https://xj.example/gdrive/callback" || state == "" || u.Query().Get("access_type") != "offline" {
		t.Fatalf("auth url %v", u)
	}
	resp, _ := http.Get(ts.URL + "/gdrive/callback?code=good&state=" + state)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "已连接") || gotRedirect != "https://xj.example/gdrive/callback" {
		t.Fatalf("callback %d %s", resp.StatusCode, body)
	}
	if resp, _ := http.Get(ts.URL + "/gdrive/callback?code=good&state=" + state); resp.StatusCode != 400 {
		t.Fatalf("reused state: %d", resp.StatusCode)
	}
	st = call("GET", "/api/gdrive")
	if st["connected"] != true || st["email"] != "me@example.com" {
		t.Fatalf("status after connect %v", st)
	}
	if c, err := sync.Client(ctx); err != nil || c == nil {
		t.Fatalf("client: %v", err)
	}
	call("DELETE", "/api/gdrive")
	if st = call("GET", "/api/gdrive"); st["connected"] != false {
		t.Fatalf("status after disconnect %v", st)
	}
}
