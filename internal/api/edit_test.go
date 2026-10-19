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

// 集成测试：重命名、移动、设私密、回收站（列出、恢复、永久删除文件夹）、手机备份记录。
func TestEditAndTrash(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "it-edit-"+uuid.NewString()[:8])
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
	mkdir := func(name, parent string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": name, "parent_id": map[bool]any{true: nil, false: parent}[parent == ""]})
		code, out := call("POST", "/api/folders", string(body))
		if code != 201 {
			t.Fatalf("mkdir %s: %d %v", name, code, out)
		}
		return out["id"].(string)
	}
	upload := func(name, parent, body string) string {
		t.Helper()
		code, out := call("POST", "/api/files?name="+name+"&parent_id="+parent, body)
		if code != 201 {
			t.Fatalf("upload %s: %d %v", name, code, out)
		}
		return out["id"].(string)
	}
	names := func(folder string) string {
		t.Helper()
		_, out := call("GET", "/api/folders/"+folder+"/children", "")
		var n []string
		for _, it := range out["items"].([]any) {
			n = append(n, it.(map[string]any)["name"].(string))
		}
		sort.Strings(n) // 列目录按拍摄 / 上传时间排，这里只看有哪些
		return strings.Join(n, ",")
	}

	a, b := mkdir("A", ""), mkdir("B", "")
	sub := mkdir("sub", a)
	x := upload("x.txt", a, "xxx")
	upload("y.txt", b, "yyy")

	// 重命名、移动；重名 409；文件夹不能移进自己里面；文件不能设私密
	if code, out := call("PATCH", "/api/files/"+x, `{"name":"y.txt"}`); code != 200 || out["name"] != "y.txt" {
		t.Fatalf("rename: %d %v", code, out)
	}
	if code, _ := call("PATCH", "/api/files/"+x, `{"parent_id":"`+b+`"}`); code != 409 {
		t.Fatalf("move onto a name that exists: %d", code)
	}
	if code, out := call("PATCH", "/api/files/"+x, `{"name":"x.txt","parent_id":"`+b+`"}`); code != 200 || out["parent_id"] != b {
		t.Fatalf("move: %d %v", code, out)
	}
	if got := names(b); got != "x.txt,y.txt" {
		t.Fatalf("B after move: %s", got)
	}
	if code, _ := call("PATCH", "/api/files/"+a, `{"parent_id":"`+sub+`"}`); code != 400 {
		t.Fatalf("folder into its own subfolder: %d", code)
	}
	if code, _ := call("PATCH", "/api/files/"+a, `{"parent_id":"`+a+`"}`); code != 400 {
		t.Fatalf("folder into itself: %d", code)
	}
	if code, out := call("PATCH", "/api/files/"+sub, `{"parent_id":null}`); code != 200 || out["parent_id"] != nil {
		t.Fatalf("move to the root: %d %v", code, out)
	}
	if code, _ := call("PATCH", "/api/files/"+x, `{"private":true}`); code != 400 {
		t.Fatalf("private file: %d", code)
	}
	if code, out := call("PATCH", "/api/files/"+a, `{"private":true}`); code != 200 || out["private"] != true {
		t.Fatalf("private folder: %d %v", code, out)
	}
	if code, out := call("GET", "/api/files/"+a, ""); code != 200 || out["private"] != true {
		t.Fatalf("private flag not kept: %d %v", code, out)
	}
	if code, _ := call("PATCH", "/api/files/"+x, `{"name":"a/b"}`); code != 400 {
		t.Fatalf("bad name: %d", code)
	}

	// 回收站：删掉的在列表里，恢复后回原处；原来的文件夹也删了时恢复到根目录
	if code, _ := call("DELETE", "/api/files/"+x, ""); code != 204 {
		t.Fatalf("trash: %d", code)
	}
	if _, out := call("GET", "/api/trash", ""); len(out["items"].([]any)) != 1 {
		t.Fatalf("trash list: %v", out)
	}
	if code, out := call("POST", "/api/files/"+x+"/restore", ""); code != 200 || out["parent_id"] != b {
		t.Fatalf("restore: %d %v", code, out)
	}
	call("DELETE", "/api/files/"+x, "")
	call("DELETE", "/api/files/"+b, "")
	if _, out := call("GET", "/api/trash", ""); len(out["items"].([]any)) != 2 {
		t.Fatalf("x and B both listed (x's folder is in the trash, but x was deleted on its own first): %v", out)
	}
	if code, out := call("POST", "/api/files/"+x+"/restore", ""); code != 200 || out["parent_id"] != nil {
		t.Fatalf("restore with the folder gone: %d %v", code, out)
	}

	// 永久删除文件夹：要先移入回收站，里面的文件一起删掉
	if code, _ := call("DELETE", "/api/files/"+a+"?permanent=1", ""); code != 400 {
		t.Fatalf("purge a folder outside the trash: %d", code)
	}
	if code, _ := call("DELETE", "/api/files/"+b+"?permanent=1", ""); code != 204 {
		t.Fatalf("purge a trashed folder: %d", code)
	}
	if code, _ := call("GET", "/api/files/"+b, ""); code != 404 {
		t.Fatalf("purged folder still there: %d", code)
	}
	if _, out := call("GET", "/api/trash", ""); len(out["items"].([]any)) != 0 {
		t.Fatalf("trash after purge: %v", out)
	}

	// 隐藏相册第一次上传时建好，默认私密；快捷指令的清单记下来，备份状态里看得到之后又传了几个
	if code, out := call("POST", "/api/ingest/plan", `{"album":"hidden","names":"IMG_1\nIMG_2","sizes":"1 KB\n2 KB"}`); code != 200 || out["count"] != 2.0 {
		t.Fatalf("plan: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/ingest", "\xFF\xD8\xFF\xE0"+strings.Repeat("z", 1000), "X-File-Name", "IMG_1", "X-Xiaoju-Album", "hidden"); code != 201 {
		t.Fatalf("ingest: %d", code)
	}
	_, root := call("GET", "/api/folders/root/children", "")
	private := map[string]bool{}
	for _, it := range root["items"].([]any) {
		m := it.(map[string]any)
		private[m["name"].(string)] = m["private"] == true
	}
	if !private[HiddenAlbumFolder] || !private["A"] || private["sub"] {
		t.Fatalf("private folders at the root: %v", private)
	}
	code, out := call("GET", "/api/backup/phone", "")
	albums, _ := out["albums"].([]any)
	if code != 200 || len(albums) != 2 {
		t.Fatalf("phone backup: %d %v", code, out)
	}
	hidden := albums[1].(map[string]any)
	checks := hidden["checks"].([]any)
	if hidden["folder"] != HiddenAlbumFolder || hidden["files"] != 1.0 || len(checks) != 1 {
		t.Fatalf("hidden album backup: %v", hidden)
	}
	c := checks[0].(map[string]any)
	if c["files"] != 2.0 || c["todo"] != 2.0 || c["uploaded_since"] != 1.0 || c["by_date"] != false {
		t.Fatalf("hidden album check: %v", c)
	}
	if album := albums[0].(map[string]any); album["folder"] != AlbumFolder || album["files"] != 0.0 || album["id"] != nil {
		t.Fatalf("album without a folder yet: %v", album)
	}
}
