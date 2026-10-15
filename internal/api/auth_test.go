package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：已注册设备后，仅凭令牌不能访问文件；运维接口仍可用；添加码一次有效。
func TestPasskeyAccessControl(t *testing.T) {
	url := os.Getenv("XIAOJU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XIAOJU_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.MigrateUp(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	store.DeleteAllPasskeys(ctx)
	store.DeleteGridRule(ctx)
	t.Cleanup(func() { store.DeleteAllPasskeys(context.Background()) })
	userID, _ := store.EnsureUser(ctx, "it-passkeys")
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	cfg := auth.PasskeyConfig{RPID: "localhost", Origins: []string{"http://localhost:8090"}}

	newServer := func() (*Server, *httptest.Server) {
		pk, err := auth.NewPasskeys(ctx, store, userID, cfg)
		if err != nil {
			t.Fatal(err)
		}
		srv := New(svc, Options{UserID: userID, Token: "tok", Log: log, Passkeys: pk})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return srv, ts
	}
	call := func(ts *httptest.Server, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	tokenH := []string{"X-Xiaoju-Token", "tok"}

	// 1. 还没注册设备：仅凭令牌可以访问，/api/auth/session 直接给会话，可以开始注册
	_, ts := newServer()
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", tokenH...); code != 200 {
		t.Fatalf("token access without passkeys: %d", code)
	}
	code, out := call(ts, "POST", "/api/auth/session", "", tokenH...)
	if code != 200 || out["passkey_required"] != false || out["session"] == nil || out["passkeys_available"] != true {
		t.Fatalf("session without passkeys: %d %v", code, out)
	}
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", "X-Xiaoju-Session", out["session"].(string)); code != 200 {
		t.Fatalf("session access: %d", code)
	}
	code, out = call(ts, "POST", "/api/auth/passkey/register/begin", `{"name":"iPhone"}`, tokenH...)
	if code != 200 || out["ceremony"] == nil || out["options"].(map[string]any)["publicKey"] == nil {
		t.Fatalf("register begin (first device): %d %v", code, out)
	}
	pub := out["options"].(map[string]any)["publicKey"].(map[string]any)
	sel := pub["authenticatorSelection"].(map[string]any)
	if sel["userVerification"] != "required" || sel["authenticatorAttachment"] != "platform" || pub["rp"].(map[string]any)["id"] != "localhost" {
		t.Fatalf("registration options: %v", pub)
	}

	// 2. 已注册一台设备（直接写库模拟）
	if err := store.AddPasskey(ctx, []byte{1, 2, 3}, "测试手机", json.RawMessage(`{"id":"AQID","publicKey":"","flags":{},"authenticator":{},"attestation":{}}`)); err != nil {
		t.Fatal(err)
	}
	srv, ts := newServer()
	code, out = call(ts, "GET", "/api/folders/root/children", "", tokenH...)
	if code != 401 || out["code"] != "second_factor_required" {
		t.Fatalf("token-only file access must require face: %d %v", code, out)
	}
	if code, _ := call(ts, "GET", "/api/folders/root/children", ""); code != 401 {
		t.Fatalf("no credentials: %d", code)
	}
	if code, _ := call(ts, "GET", "/api/stats", "", tokenH...); code != 200 {
		t.Fatalf("ops endpoint with token: %d", code)
	}
	code, out = call(ts, "POST", "/api/auth/session", "", tokenH...)
	if code != 200 || out["passkey_required"] != true || out["session"] != nil {
		t.Fatalf("session must not be issued without face: %d %v", code, out)
	}
	code, out = call(ts, "POST", "/api/auth/passkey/login/begin", "", tokenH...)
	if code != 200 || out["options"].(map[string]any)["publicKey"].(map[string]any)["userVerification"] != "required" {
		t.Fatalf("login begin: %d %v", code, out)
	}
	if code, out := call(ts, "POST", "/api/auth/passkey/login/finish?ceremony=bogus", "{}", tokenH...); code != 400 || out["code"] != "ceremony_expired" {
		t.Fatalf("bogus ceremony: %d %v", code, out)
	}
	if code, out := call(ts, "POST", "/api/auth/passkey/register/begin", `{}`, tokenH...); code != 403 || out["code"] != "add_code_required" {
		t.Fatalf("second device without code: %d %v", code, out)
	}
	if code, _ := call(ts, "POST", "/api/auth/passkey/login/begin", ""); code != 401 {
		t.Fatalf("login begin without token: %d", code)
	}

	// 3. 已登录（刷脸后）的会话：可以访问文件、生成添加码；添加码只能用一次
	sess, _ := srv.sessions.Issue()
	sessH := []string{"X-Xiaoju-Session", sess}
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", sessH...); code != 200 {
		t.Fatalf("session file access: %d", code)
	}
	code, out = call(ts, "POST", "/api/auth/passkeys/add-code", "", sessH...)
	if code != 200 || len(out["code"].(string)) != 8 {
		t.Fatalf("add code: %d %v", code, out)
	}
	body := `{"name":"iPad","code":"` + strings.ToLower(out["code"].(string)) + `"}`
	if code, _ := call(ts, "POST", "/api/auth/passkey/register/begin", body, tokenH...); code != 200 {
		t.Fatalf("register with add code: %d", code)
	}
	if code, _ := call(ts, "POST", "/api/auth/passkey/register/begin", body, tokenH...); code != 403 {
		t.Fatalf("add code reused: %d", code)
	}
	if code, _ := call(ts, "POST", "/api/auth/passkeys/add-code", "", tokenH...); code != 401 {
		t.Fatalf("add code needs a face-verified session: %d", code)
	}

	// 4. 设备列表和删除；删掉最后一台后恢复仅凭令牌访问
	code, out = call(ts, "GET", "/api/auth/passkeys", "", sessH...)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["name"] != "测试手机" {
		t.Fatalf("list passkeys: %d %v", code, out)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/passkeys/"+items[0].(map[string]any)["id"].(string), "", sessH...); code != 204 {
		t.Fatalf("delete passkey: %d", code)
	}
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", tokenH...); code != 200 {
		t.Fatalf("token access after removing all devices: %d", code)
	}
}
