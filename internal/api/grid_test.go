package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：令牌 → 刷脸 → 格子口令 三步都要过；恢复码每次登录只能代替一步；关闭要当场验证。
func TestGridLoginFlow(t *testing.T) {
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
	reset := func() {
		store.DeleteAllPasskeys(context.Background())
		store.DeleteGridRule(context.Background())
	}
	reset()
	t.Cleanup(reset)
	userID, _ := store.EnsureUser(ctx, "it-grid")
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	shanghai, _ := time.LoadLocation("Asia/Shanghai")

	newServer := func() (*Server, *httptest.Server) {
		pk, err := auth.NewPasskeys(ctx, store, userID, auth.PasskeyConfig{RPID: "localhost", Origins: []string{"http://localhost:8090"}})
		if err != nil {
			t.Fatal(err)
		}
		grid, err := auth.NewGrid(ctx, store, master, shanghai)
		if err != nil {
			t.Fatal(err)
		}
		srv := New(svc, Options{UserID: userID, Token: "tok", Log: log, Passkeys: pk, Grid: grid})
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
	rule := auth.GridRule{Mul: 1, Add: 11, MondayBonus: 5}
	answer := func(ch map[string]any) int {
		var cells [auth.GridCells]int
		for i, v := range ch["grid"].([]any) {
			cells[i] = int(v.(float64))
		}
		return rule.Answer(cells, int(ch["weekday"].(float64)))
	}
	tokenH := []string{"X-Xiaoju-Token", "tok"}
	files := "/api/folders/root/children"

	// 1. 什么都没开启：仅凭令牌拿到会话，开启格子口令（练习题答错不开启）
	srv, ts := newServer()
	sess, _ := srv.sessions.Issue()
	sessH := append([]string{"X-Xiaoju-Session", sess}, tokenH...)
	if code, out := call(ts, "GET", "/api/auth/grid", "", sessH...); code != 200 || out["enabled"] != false || out["timezone"] != "Asia/Shanghai" {
		t.Fatalf("status before setup: %d %v", code, out)
	}
	_, ch := call(ts, "POST", "/api/auth/grid/setup/begin", "", sessH...)
	ruleJSON, _ := json.Marshal(rule)
	setup := func(id string, ans int) (int, map[string]any) {
		return call(ts, "POST", "/api/auth/grid/setup/finish", fmt.Sprintf(`{"id":%q,"rule":%s,"answer":%d}`, id, ruleJSON, ans), sessH...)
	}
	if code, out := setup(ch["id"].(string), answer(ch)+1); code != 403 || out["code"] != "wrong_answer" {
		t.Fatalf("setup with wrong practice answer: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/setup/begin", "", sessH...)
	code, out := setup(ch["id"].(string), answer(ch))
	if code != 200 || len(out["recovery_codes"].([]any)) != 10 {
		t.Fatalf("setup: %d %v", code, out)
	}
	recovery := out["recovery_codes"].([]any)
	if code, _ := call(ts, "POST", "/api/auth/grid/setup/begin", "", sessH...); code != 409 {
		t.Fatalf("setup while enabled: %d", code)
	}

	// 2. 只开了格子口令：仅凭令牌不能访问文件；答对才发会话
	srv, ts = newServer() // 重启后从数据库读出规则
	if code, out := call(ts, "GET", files, "", tokenH...); code != 401 || out["code"] != "second_factor_required" {
		t.Fatalf("token-only access with grid enabled: %d %v", code, out)
	}
	code, out = call(ts, "POST", "/api/auth/session", "", tokenH...)
	if code != 200 || out["session"] != nil || fmt.Sprint(out["steps"]) != "[grid]" {
		t.Fatalf("session with grid enabled: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "{}", tokenH...)
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)+1), tokenH...); code != 403 || out["code"] != "wrong_answer" {
		t.Fatalf("wrong answer: %d %v", code, out)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)), tokenH...); code != 400 || out["code"] != "challenge_expired" {
		t.Fatalf("retry on the same grid: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "{}", tokenH...)
	code, out = call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)), tokenH...)
	if code != 200 || out["session"] == nil {
		t.Fatalf("correct answer: %d %v", code, out)
	}
	if code, _ := call(ts, "GET", files, "", "X-Xiaoju-Session", out["session"].(string)); code != 200 {
		t.Fatalf("session after grid: %d", code)
	}
	if code, _ := call(ts, "POST", "/api/auth/grid/challenge", "{}"); code != 401 {
		t.Fatalf("challenge without token: %d", code)
	}

	// 3. 再注册刷脸设备：两步都要过，格子口令必须在刷脸之后
	if err := store.AddPasskey(ctx, []byte{9, 9, 9}, "测试手机", json.RawMessage(`{"id":"CQkJ","publicKey":"","flags":{},"authenticator":{},"attestation":{}}`)); err != nil {
		t.Fatal(err)
	}
	srv, ts = newServer()
	if _, out := call(ts, "POST", "/api/auth/session", "", tokenH...); fmt.Sprint(out["steps"]) != "[passkey grid]" || out["session"] != nil {
		t.Fatalf("steps with both enabled: %v", out)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/challenge", "{}", tokenH...); code != 401 || out["code"] != "passkey_required" {
		t.Fatalf("grid before face: %d %v", code, out)
	}
	if code, _ := call(ts, "POST", "/api/auth/grid/challenge", `{"ticket":"t1.9999999999.p.bogus"}`, tokenH...); code != 401 {
		t.Fatalf("forged ticket: %d", code)
	}
	if code, _ := call(ts, "GET", files, "", "X-Xiaoju-Session", srv.sessions.IssueTicket(false)); code != 401 {
		t.Fatalf("ticket used as a session: %d", code)
	}
	ticket := srv.sessions.IssueTicket(false) // 相当于刷脸通过
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", fmt.Sprintf(`{"ticket":%q}`, ticket), tokenH...)
	code, out = call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"ticket":%q,"id":%q,"answer":%d}`, ticket, ch["id"], answer(ch)), tokenH...)
	if code != 200 || out["session"] == nil {
		t.Fatalf("face + grid: %d %v", code, out)
	}
	// 恢复码代替格子口令
	code, out = call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"ticket":%q,"recovery":%q}`, ticket, recovery[0]), tokenH...)
	if code != 200 || out["session"] == nil {
		t.Fatalf("recovery code instead of grid: %d %v", code, out)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"ticket":%q,"recovery":%q}`, ticket, recovery[0]), tokenH...); code != 403 || out["code"] != "recovery_invalid" {
		t.Fatalf("recovery code reused: %d %v", code, out)
	}
	// 恢复码代替刷脸：之后不能再用恢复码代替格子口令
	code, out = call(ts, "POST", "/api/auth/recovery/passkey", fmt.Sprintf(`{"code":%q}`, recovery[1]), tokenH...)
	if code != 200 || out["next"] != "grid" {
		t.Fatalf("recovery code instead of face: %d %v", code, out)
	}
	rticket := out["ticket"].(string)
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"ticket":%q,"recovery":%q}`, rticket, recovery[2]), tokenH...); code != 400 || out["code"] != "recovery_used" {
		t.Fatalf("two recovery codes in one login: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", fmt.Sprintf(`{"ticket":%q}`, rticket), tokenH...)
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"ticket":%q,"id":%q,"answer":%d}`, rticket, ch["id"], answer(ch)), tokenH...); code != 200 || out["session"] == nil {
		t.Fatalf("recovery for face + grid answer: %d %v", code, out)
	}

	// 4. 关闭：要会话 + 当场刷脸 + 答对题
	sess, _ = srv.sessions.Issue()
	sessH = append([]string{"X-Xiaoju-Session", sess}, tokenH...)
	if code, out := call(ts, "GET", "/api/auth/grid", "", sessH...); code != 200 || out["enabled"] != true || out["recovery_left"] != float64(8) {
		t.Fatalf("status: %d %v", code, out)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/grid", "{}", sessH...); code != 401 {
		t.Fatalf("disable without fresh face: %d", code)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/grid", fmt.Sprintf(`{"ticket":%q}`, ticket), sessH...); code != 400 {
		t.Fatalf("disable without answer: %d", code)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/grid", "{}", tokenH...); code != 401 {
		t.Fatalf("disable without session: %d", code)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", fmt.Sprintf(`{"ticket":%q}`, ticket), tokenH...)
	if code, _ := call(ts, "DELETE", "/api/auth/grid", fmt.Sprintf(`{"ticket":%q,"id":%q,"answer":%d}`, ticket, ch["id"], answer(ch)), sessH...); code != 204 {
		t.Fatalf("disable: %d", code)
	}
	if _, out := call(ts, "POST", "/api/auth/session", "", tokenH...); fmt.Sprint(out["steps"]) != "[passkey]" {
		t.Fatalf("steps after disabling grid: %v", out)
	}
	// 只剩刷脸时，刷脸后直接发会话（afterPasskey），恢复码已全部作废
	if code, _ := call(ts, "POST", "/api/auth/recovery/passkey", fmt.Sprintf(`{"code":%q}`, recovery[3]), tokenH...); code != 400 {
		t.Fatalf("recovery after disabling grid: %d", code)
	}
}
