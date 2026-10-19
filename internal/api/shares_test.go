package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：分享链接。不用登录能看分享的文件夹和子文件夹、下载里面的文件；出不了分享的范围；密码、过期、撤销；
// 进了回收站的看不了；密码试太多次被限速。
func TestShares(t *testing.T) {
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
	store := meta.NewStore(pool)
	userID, _ := store.EnsureUser(ctx, "it-share-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	call := func(auth bool, method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if auth {
			req.Header.Set("X-Xiaoju-Token", "tok")
		}
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
	mkdir := func(name, parent string) string {
		t.Helper()
		p := any(nil)
		if parent != "" {
			p = parent
		}
		b, _ := json.Marshal(map[string]any{"name": name, "parent_id": p})
		_, out := call(true, "POST", "/api/folders", string(b))
		return out["id"].(string)
	}
	upload := func(name, parent, body string) string {
		t.Helper()
		_, out := call(true, "POST", "/api/files?name="+name+"&parent_id="+parent, body)
		return out["id"].(string)
	}
	share := func(id string, days int, password string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"file_id": id, "expires_days": days, "password": password})
		code, out := call(true, "POST", "/api/shares", string(b))
		if code != 201 {
			t.Fatalf("share: %d %v", code, out)
		}
		return out
	}
	names := func(out map[string]any) string {
		var n []string
		for _, it := range out["items"].([]any) {
			n = append(n, it.(map[string]any)["name"].(string))
		}
		sort.Strings(n)
		return strings.Join(n, ",")
	}

	trip := mkdir("旅行", "")
	day1 := mkdir("第一天", trip)
	other := mkdir("别的", "")
	upload("a.txt", trip, "hello a")
	upload("b.txt", day1, "hello b")
	secret := upload("s.txt", other, "secret")

	sh := share(trip, 7, "")
	token := sh["token"].(string)
	if len(token) < 20 || sh["expires_at"] == nil || sh["has_password"] != false {
		t.Fatalf("share: %v", sh)
	}
	// 不用登录：看分享的文件夹、进子文件夹，文件带下载地址
	code, out := call(false, "GET", "/api/s/"+token, "")
	if code != 200 || names(out) != "a.txt,第一天" || out["share"].(map[string]any)["name"] != "旅行" {
		t.Fatalf("open share: %d %v", code, out)
	}
	var fileURL string
	for _, it := range out["items"].([]any) {
		if m := it.(map[string]any); m["name"] == "a.txt" {
			fileURL = m["url"].(string)
		}
	}
	resp, err := http.Get(ts.URL + fileURL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello a" {
		t.Fatalf("download from share: %q", b)
	}
	code, out = call(false, "GET", "/api/s/"+token+"?folder="+day1, "")
	if code != 200 || names(out) != "b.txt" || len(out["crumbs"].([]any)) != 2 {
		t.Fatalf("subfolder: %d %v", code, out)
	}
	// 出不了分享的范围
	if code, _ := call(false, "GET", "/api/s/"+token+"?folder="+other, ""); code != 404 {
		t.Fatalf("outside folder: %d", code)
	}
	if code, _ := call(false, "GET", "/api/s/nope", ""); code != 404 {
		t.Fatalf("unknown token: %d", code)
	}
	// 单个文件的分享
	fsh := share(secret, 0, "")
	if code, out := call(false, "GET", "/api/s/"+fsh["token"].(string), ""); code != 200 || names(out) != "s.txt" || fsh["expires_at"] != nil {
		t.Fatalf("file share: %d %v", code, out)
	}

	// 密码
	psh := share(trip, 1, "芝麻开门")
	ptoken := psh["token"].(string)
	if code, out := call(false, "GET", "/api/s/"+ptoken, ""); code != 401 || out["code"] != "share_password" {
		t.Fatalf("needs password: %d %v", code, out)
	}
	if code, _ := call(false, "POST", "/api/s/"+ptoken+"/unlock", `{"password":"wrong"}`); code != 403 {
		t.Fatalf("wrong password: %d", code)
	}
	code, out = call(false, "POST", "/api/s/"+ptoken+"/unlock", `{"password":"芝麻开门"}`)
	if code != 200 || out["key"] == nil {
		t.Fatalf("unlock: %d %v", code, out)
	}
	key := out["key"].(string)
	if code, _ := call(false, "GET", "/api/s/"+ptoken, "", "X-Share-Key", key); code != 200 {
		t.Fatalf("with key: %d", code)
	}
	if code, _ := call(false, "GET", "/api/s/"+token, "", "X-Share-Key", key+"0"); code != 200 {
		t.Fatalf("a share without password ignores the key: %d", code)
	}
	if code, _ := call(false, "GET", "/api/s/"+ptoken, "", "X-Share-Key", "1."+strings.Repeat("0", 64)); code != 401 {
		t.Fatalf("forged key: %d", code)
	}

	// 过期
	pool.Exec(ctx, `UPDATE shares SET expires_at = now() - interval '1 minute' WHERE token = $1`, ptoken)
	if code, out := call(false, "GET", "/api/s/"+ptoken, "", "X-Share-Key", key); code != 410 || out["code"] != "share_expired" {
		t.Fatalf("expired: %d %v", code, out)
	}

	// 列出、撤销
	code, out = call(true, "GET", "/api/shares", "")
	if code != 200 || len(out["items"].([]any)) != 3 {
		t.Fatalf("list shares: %d %v", code, out)
	}
	if code, _ := call(true, "DELETE", "/api/shares/"+fsh["id"].(string), ""); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := call(false, "GET", "/api/s/"+fsh["token"].(string), ""); code != 404 {
		t.Fatalf("revoked: %d", code)
	}
	// 分享的文件夹进了回收站：看不了
	call(true, "DELETE", "/api/files/"+trip, "")
	if code, _ := call(false, "GET", "/api/s/"+token, ""); code != 404 {
		t.Fatalf("trashed: %d", code)
	}
	// 管理接口要登录
	if code, _ := call(false, "GET", "/api/shares", ""); code != 401 {
		t.Fatalf("list without auth: %d", code)
	}

	// 密码试太多次：限速
	limited := false
	for range 12 {
		if code, _ := call(false, "POST", "/api/s/"+ptoken+"/unlock", `{"password":"x"}`); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("password attempts must be rate limited")
	}
}
