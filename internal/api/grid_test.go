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

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/auth"
	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：登录只要令牌 + 刷脸；看照片、视频每随机 3–7 张抽查一次格子口令，两次机会都没答对就退出登录。
func TestGridViewChecks(t *testing.T) {
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
		sessions, err := auth.NewSessionsWithStore(ctx, "tok", store)
		if err != nil {
			t.Fatal(err)
		}
		srv := New(svc, Options{UserID: userID, Token: "tok", Log: log, Passkeys: pk, Grid: grid, Sessions: sessions})
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
	sessH := func(sess string) []string { return append([]string{"X-Xiaoju-Session", sess}, tokenH...) }
	login := func(ts *httptest.Server) []string {
		t.Helper()
		code, out := call(ts, "POST", "/api/auth/session", "", tokenH...)
		if code != 200 || out["session"] == nil {
			t.Fatalf("login: %d %v", code, out)
		}
		return sessH(out["session"].(string))
	}

	// 1. 什么都没开启：上传几张照片和一个文本文件，开启格子口令（练习题答错不开启）
	_, ts := newServer()
	h := login(ts)
	var photos []string
	for i := range 9 {
		code, out := call(ts, "POST", fmt.Sprintf("/api/files?name=it-grid-%d-%d.jpg", time.Now().UnixNano(), i), "jpeg", append(h, "Content-Type", "image/jpeg")...)
		if code != 201 {
			t.Fatalf("upload photo: %d %v", code, out)
		}
		photos = append(photos, out["id"].(string))
	}
	_, doc := call(ts, "POST", fmt.Sprintf("/api/files?name=it-grid-%d.txt", time.Now().UnixNano()), "hello", append(h, "Content-Type", "text/plain")...)
	t.Cleanup(func() {
		for _, id := range append(photos, doc["id"].(string)) {
			svc.Purge(context.Background(), userID, uuid.MustParse(id))
		}
	})

	_, ch := call(ts, "POST", "/api/auth/grid/setup/begin", "", h...)
	ruleJSON, _ := json.Marshal(rule)
	setup := func(id string, ans int) (int, map[string]any) {
		return call(ts, "POST", "/api/auth/grid/setup/finish", fmt.Sprintf(`{"id":%q,"rule":%s,"answer":%d}`, id, ruleJSON, ans), h...)
	}
	if code, out := setup(ch["id"].(string), answer(ch)+1); code != 403 || out["code"] != "wrong_answer" {
		t.Fatalf("setup with wrong practice answer: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/setup/begin", "", h...)
	code, out := setup(ch["id"].(string), answer(ch))
	if code != 200 || len(out["recovery_codes"].([]any)) != 10 {
		t.Fatalf("setup: %d %v", code, out)
	}
	recovery := out["recovery_codes"].([]any)

	// 2. 开启后：登录不用答题，但不能再仅凭令牌访问文件（要靠会话计数）
	_, ts = newServer() // 重启后从数据库读出规则
	if code, out := call(ts, "GET", "/api/folders/root/children", "", tokenH...); code != 401 || out["code"] != "second_factor_required" {
		t.Fatalf("token-only access with grid enabled: %d %v", code, out)
	}
	h = login(ts)
	link := func(h []string, id string) (int, map[string]any) {
		return call(ts, "POST", "/api/files/"+id+"/link", "", h...)
	}
	// 用完额度：允许的张数在 3–7 之间
	exhaust := func(h []string) int {
		t.Helper()
		n := 0
		for i := 0; ; i++ {
			code, out := link(h, photos[i%len(photos)])
			if code == 403 {
				if out["code"] != "grid_required" {
					t.Fatalf("blocked view: %v", out)
				}
				return n
			}
			if code != 200 {
				t.Fatalf("link: %d %v", code, out)
			}
			n++
		}
	}
	if n := exhaust(h); n < auth.ViewsMin || n > auth.ViewsMax {
		t.Fatalf("views before a check: %d", n)
	}
	// 非照片视频不受影响；直接下载照片也算；视频拖动（不从头开始的 Range）不重复计数
	if code, _ := link(h, doc["id"].(string)); code != 200 {
		t.Fatalf("text file must not be gated: %d", code)
	}
	if code, out := call(ts, "GET", "/api/files/"+photos[0]+"/content", "", h...); code != 403 || out["code"] != "grid_required" {
		t.Fatalf("direct download must be gated: %d %v", code, out)
	}
	if code, _ := call(ts, "GET", "/api/files/"+photos[0]+"/content", "", append(h, "Range", "bytes=1-")...); code != 206 {
		t.Fatalf("mid-file range request: %d", code)
	}

	// 第一次答错 → 换新格子、20 秒 → 答对后又能看
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h...)
	if ch["attempt"] != float64(1) || ch["seconds"] != float64(15) {
		t.Fatalf("first chance: %v", ch)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)+1), h...); code != 403 || out["code"] != "wrong_answer" {
		t.Fatalf("first wrong answer: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h...)
	if ch["attempt"] != float64(2) || ch["seconds"] != float64(20) {
		t.Fatalf("second chance: %v", ch)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)), h...); code != 200 || out["ok"] != true {
		t.Fatalf("second chance correct: %d %v", code, out)
	}
	if code, _ := link(h, photos[0]); code != 200 {
		t.Fatalf("view after a correct answer: %d", code)
	}

	// 页面倒计时到了算超时；两次都没答对 → 退出登录，重启后依然无效
	exhaust(h)
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h...)
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"timeout":true}`, ch["id"]), h...); code != 403 || out["code"] != "timeout" {
		t.Fatalf("timeout: %d %v", code, out)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h...)
	if code, out := call(ts, "POST", "/api/auth/grid/verify", fmt.Sprintf(`{"id":%q,"answer":%d}`, ch["id"], answer(ch)+1), h...); code != 401 || out["code"] != "grid_logout" {
		t.Fatalf("second wrong answer must log out: %d %v", code, out)
	}
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", h...); code != 401 {
		t.Fatalf("logged-out session still works: %d", code)
	}
	_, ts = newServer()
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", h...); code != 401 {
		t.Fatalf("logged-out session revived by a restart: %d", code)
	}
	h2 := login(ts) // 重新登录就能继续，别的会话不受影响
	if code, _ := link(h2, photos[0]); code != 200 {
		t.Fatalf("new session: %d", code)
	}
	// 出了题不答、直接再要一道：算用掉一次机会
	exhaust(h2)
	call(ts, "POST", "/api/auth/grid/challenge", "", h2...)
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h2...)
	if ch["attempt"] != float64(2) {
		t.Fatalf("abandoned grid must use up a chance: %v", ch)
	}
	if code, out := call(ts, "POST", "/api/auth/grid/challenge", "", h2...); code != 401 || out["code"] != "grid_logout" {
		t.Fatalf("abandoning both chances must log out: %d %v", code, out)
	}

	// 3. 再注册刷脸设备：登录只要刷脸，不答题；设备丢了可以用恢复码代替刷脸
	if err := store.AddPasskey(ctx, []byte{9, 9, 9}, "测试手机", json.RawMessage(`{"id":"CQkJ","publicKey":"","flags":{},"authenticator":{},"attestation":{}}`)); err != nil {
		t.Fatal(err)
	}
	srv, ts := newServer()
	if _, out := call(ts, "POST", "/api/auth/session", "", tokenH...); fmt.Sprint(out["steps"]) != "[passkey]" || out["session"] != nil || out["recovery_available"] != true {
		t.Fatalf("steps with both enabled: %v", out)
	}
	if code, out := call(ts, "POST", "/api/auth/recovery/passkey", `{"code":"zzzz-zzzz"}`, tokenH...); code != 403 || out["code"] != "recovery_invalid" {
		t.Fatalf("wrong recovery code: %d %v", code, out)
	}
	code, out = call(ts, "POST", "/api/auth/recovery/passkey", fmt.Sprintf(`{"code":%q}`, recovery[0]), tokenH...)
	if code != 200 || out["session"] == nil {
		t.Fatalf("recovery code instead of face: %d %v", code, out)
	}
	h = sessH(out["session"].(string))
	if code, _ := call(ts, "GET", "/api/folders/root/children", "", h...); code != 200 {
		t.Fatalf("session from recovery code: %d", code)
	}

	// 4. 关闭：要会话 + 当场刷脸 + 答对题（或恢复码）
	ticket := srv.sessions.IssueTicket() // 相当于刚刚刷过脸
	if code, out := call(ts, "DELETE", "/api/auth/grid", "{}", h...); code != 401 || out["code"] != "passkey_required" {
		t.Fatalf("disable without fresh face: %d %v", code, out)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/grid", fmt.Sprintf(`{"ticket":%q}`, ticket), h...); code != 400 {
		t.Fatalf("disable without answer: %d", code)
	}
	if code, _ := call(ts, "DELETE", "/api/auth/grid", "{}", tokenH...); code != 401 {
		t.Fatalf("disable without session: %d", code)
	}
	_, ch = call(ts, "POST", "/api/auth/grid/challenge", "", h...)
	if code, _ := call(ts, "DELETE", "/api/auth/grid", fmt.Sprintf(`{"ticket":%q,"id":%q,"answer":%d}`, ticket, ch["id"], answer(ch)), h...); code != 204 {
		t.Fatalf("disable: %d", code)
	}
	if code, out := call(ts, "GET", "/api/auth/grid", "", h...); code != 200 || out["enabled"] != false {
		t.Fatalf("status after disable: %d %v", code, out)
	}
	for range 10 { // 关闭后不再抽查
		if code, _ := link(h, photos[0]); code != 200 {
			t.Fatalf("view after disabling: %d", code)
		}
	}
}
