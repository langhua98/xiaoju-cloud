package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// 集成测试：按内容去重。网页先问哪些内容已经有了；上传时 dedupe=1 不再存一样的；快捷指令传来的改过名、
// 挪过文件夹的照片认得出来，之后的上传计划也当作传过；私密文件夹、回收站的规则。
func TestContentDedupe(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "it-dedupe-"+uuid.NewString()[:8])
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	call := func(method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Xiaoju-Token", "tok")
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
	mkdir := func(name string) string {
		t.Helper()
		code, out := call("POST", "/api/folders", `{"name":"`+name+`","parent_id":null}`)
		if code != 201 {
			t.Fatalf("mkdir %s: %d %v", name, code, out)
		}
		return out["id"].(string)
	}
	upload := func(name, parent, body string, dedupe bool) (int, map[string]any) {
		t.Helper()
		q := url.Values{"name": {name}, "parent_id": {parent}}
		if dedupe {
			q.Set("dedupe", "1")
		}
		return call("POST", "/api/files?"+q.Encode(), body)
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	known := func(parent string, contents ...string) map[string]any {
		t.Helper()
		sums := make([]string, len(contents))
		for i, c := range contents {
			sums[i] = sum(c)
		}
		p := any(nil)
		if parent != "" {
			p = parent
		}
		body, _ := json.Marshal(map[string]any{"sha256": sums, "parent_id": p})
		code, out := call("POST", "/api/files/known", string(body))
		if code != 200 {
			t.Fatalf("known: %d %v", code, out)
		}
		return out["known"].(map[string]any)
	}
	count := func(folder string) int {
		t.Helper()
		_, out := call("GET", "/api/folders/"+folder+"/children", "")
		return len(out["items"].([]any))
	}
	parts := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM file_parts p JOIN files f ON f.id = p.file_id WHERE f.user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	photos, other := mkdir("Photos"), mkdir("Other")
	photo := "\xFF\xD8\xFF\xE0" + strings.Repeat("p", 500) // 一个分块
	big := strings.Repeat("b", 2500)                       // 三个分块
	exact := strings.Repeat("e", 2000)                     // 正好两个分块
	for _, c := range []string{photo, big, exact} {
		if code, out := upload("orig-"+sum(c)[:6], photos, c, false); code != 201 {
			t.Fatalf("upload: %d %v", code, out)
		}
	}

	// 网页先问：只回答已经有的
	k := known(other, photo, big, "never uploaded")
	if len(k) != 2 || k[sum(photo)] == nil || k[sum(big)] == nil {
		t.Fatalf("known: %v", k)
	}
	if f := k[sum(photo)].(map[string]any); f["parent_id"] != photos || f["name"] != "orig-"+sum(photo)[:6] {
		t.Fatalf("known photo: %v", f)
	}
	if code, _ := call("POST", "/api/files/known", `{"sha256":["zz"]}`); code != 400 {
		t.Fatalf("bad sum: %d", code)
	}

	// 换个名字、传进别的文件夹：dedupe=1 时不再存，分块一个都不多
	before := parts()
	for _, c := range []string{photo, big, exact} {
		code, out := upload("renamed.jpg", other, c, true)
		if code != 200 || out["same_content"] != true || out["file"].(map[string]any)["parent_id"] != photos {
			t.Fatalf("dedupe upload: %d %v", code, out)
		}
	}
	if count(other) != 0 || parts() != before {
		t.Fatalf("duplicates were stored: other has %d, parts %d → %d", count(other), before, parts())
	}
	// 不带 dedupe 照常存一份（网页“上传文件”、curl）
	if code, _ := upload("copy.jpg", other, photo, false); code != 201 {
		t.Fatalf("plain upload must still store: %d", code)
	}

	// 私密文件夹：只认私密文件夹里的那份，否则照传
	secret := mkdir("Secret")
	if code, out := call("PATCH", "/api/files/"+secret, `{"private":true}`); code != 200 {
		t.Fatalf("private: %d %v", code, out)
	}
	if k := known(secret, photo); len(k) != 0 {
		t.Fatalf("a public copy must not count for a private folder: %v", k)
	}
	if code, out := upload("p.jpg", secret, photo, true); code != 201 {
		t.Fatalf("upload into private folder: %d %v", code, out)
	}
	if k := known(secret, photo); k[sum(photo)].(map[string]any)["parent_id"] != secret {
		t.Fatalf("private copy: %v", k)
	}
	// 同样内容有好几份时，优先回答要传进的那个文件夹里的
	if k := known(other, photo); k[sum(photo)].(map[string]any)["parent_id"] != other {
		t.Fatalf("prefer the target folder: %v", k)
	}

	// 回收站里的（包括所在文件夹进了回收站的）不算
	trashed := mkdir("Trashed")
	only := "only in the trashed folder"
	upload("t.bin", trashed, only, false)
	if len(known("", only)) != 1 {
		t.Fatal("before trashing")
	}
	if code, _ := call("DELETE", "/api/files/"+trashed, ""); code != 204 {
		t.Fatalf("trash folder: %d", code)
	}
	if k := known("", only); len(k) != 0 {
		t.Fatalf("content in a trashed folder must not count: %v", k)
	}

	// 快捷指令：传过的照片在网盘里改了名，再传一次按内容认出来；之后计划、exists 都当作传过
	shot := "\xFF\xD8\xFF\xE0" + strings.Repeat("s", 800)
	ingest := func(name, album, body string) (int, map[string]any) {
		t.Helper()
		return call("POST", "/api/ingest", body, "X-File-Name", name, "X-Xiaoju-Album", album)
	}
	code, out := ingest("IMG_1", "", shot)
	if code != 201 {
		t.Fatalf("ingest: %d %v", code, out)
	}
	_, root := call("GET", "/api/folders/root/children", "")
	var albumID, shotID string
	for _, it := range root["items"].([]any) {
		if m := it.(map[string]any); m["name"] == AlbumFolder {
			albumID = m["id"].(string)
		}
	}
	_, items := call("GET", "/api/folders/"+albumID+"/children", "")
	shotID = items["items"].([]any)[0].(map[string]any)["id"].(string)
	if code, out := call("PATCH", "/api/files/"+shotID, `{"name":"生日.jpg"}`); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	if code, out := ingest("IMG_1", "", shot); code != 200 || out["result"] != "skipped" || out["same_content"] != true || out["name"] != "IMG_1.jpg" {
		t.Fatalf("renamed photo must be recognized: %d %v", code, out)
	}
	if n := count(albumID); n != 1 {
		t.Fatalf("album has %d files, want 1", n)
	}
	if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_1", "X-File-Size", "804"); code != 200 || out["result"] != "exists" {
		t.Fatalf("exists via alias: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest/plan", `{"album":"","names":"IMG_1\nIMG_2"}`); code != 200 || out["todo"] != "2" {
		t.Fatalf("plan via alias: %d %v", code, out)
	}
	// 第三次不用再发内容：名字、大小就认得出来
	if code, out := ingest("IMG_1", "", shot); code != 200 || out["result"] != "skipped" {
		t.Fatalf("third time: %d %v", code, out)
	}

	// 那张照片永久删除后，别名跟着没了：再传就存
	call("DELETE", "/api/files/"+shotID, "")
	if code, _ := call("DELETE", "/api/files/"+shotID+"?permanent=1", ""); code != 204 {
		t.Fatalf("purge: %d", code)
	}
	if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_1", "X-File-Size", "804"); out["result"] != "missing" {
		t.Fatalf("alias must go with the file: %d %v", code, out)
	}
	if code, out := ingest("IMG_1", "", shot); code != 201 {
		t.Fatalf("after purge: %d %v", code, out)
	}

	// 隐藏相册是私密的：“相册”里有一份也照传；反过来，隐藏相册里有的不再传进“相册”（不会因此露出来）
	if code, out := ingest("IMG_1", "hidden", shot); code != 201 {
		t.Fatalf("hidden album must store its own copy: %d %v", code, out)
	}
	hiddenShot := "\xFF\xD8\xFF\xE0" + strings.Repeat("h", 300)
	if code, out := ingest("IMG_5", "hidden", hiddenShot); code != 201 {
		t.Fatalf("hidden: %d %v", code, out)
	}
	if code, out := ingest("IMG_5", "", hiddenShot); code != 200 || out["same_content"] != true {
		t.Fatalf("a private copy counts for the public album: %d %v", code, out)
	}
}
