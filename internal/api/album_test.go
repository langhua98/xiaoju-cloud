package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestMediaType(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	ftyp := func(brand string) []byte { return []byte("\x00\x00\x00\x18ftyp" + brand + "\x00\x00\x00\x00") }
	for _, tc := range []struct {
		ct       string
		head     []byte
		wantType string
		wantExt  string
	}{
		{"image/heic", nil, "image/heic", ".heic"},
		{"image/jpeg; charset=binary", nil, "image/jpeg", ".jpg"},
		{"video/quicktime", nil, "video/quicktime", ".mov"},
		{"", jpeg, "image/jpeg", ".jpg"},
		{"application/octet-stream", ftyp("heic"), "image/heic", ".heic"},
		{"application/octet-stream", ftyp("qt  "), "video/quicktime", ".mov"},
		{"", ftyp("isom"), "video/mp4", ".mp4"},
		{"", []byte("\x89PNG\r\n\x1a\n"), "image/png", ".png"},
	} {
		gotType, gotExt := mediaType(tc.ct, tc.head)
		if gotType != tc.wantType || gotExt != tc.wantExt {
			t.Errorf("mediaType(%q, %q) = %q, %q; want %q, %q", tc.ct, tc.head, gotType, gotExt, tc.wantType, tc.wantExt)
		}
	}
}

// 集成测试：需要一个空的 PostgreSQL 数据库，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestAlbumSyncIntegration(t *testing.T) {
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
	driver, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "it-"+t.Name()+"-"+uuid.NewString()) // 每次运行用新用户，测试可重复执行
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	do := func(method, path, ctype string, body []byte) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Xiaoju-Token", "tok")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	key := "20261010_153012_IMG_1234"
	if code, body := do("GET", "/api/album/check?key="+key, "", nil); code != 200 || body != "missing" {
		t.Fatalf("check before upload: %d %q", code, body)
	}
	photo := append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), bytes.Repeat([]byte{7}, 5000)...)
	code, body := do("POST", "/api/album/upload?key="+key, "", photo)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, body)
	}
	var f meta.File
	json.Unmarshal([]byte(body), &f)
	if f.Name != key+".heic" || f.Mime != "image/heic" || f.Size != int64(len(photo)) {
		t.Fatalf("uploaded file = %+v", f)
	}
	album, err := store.FindFolder(ctx, userID, nil, albumFolder)
	if err != nil || f.ParentID == nil || *f.ParentID != album.ID {
		t.Fatalf("file not in album folder: %v %+v", err, f)
	}
	if code, got := do("GET", "/api/files/"+f.ID.String()+"/content", "", nil); code != 200 || got != string(photo) {
		t.Fatalf("download: %d, match=%v", code, got == string(photo))
	}
	if code, body := do("GET", "/api/album/check?key="+key, "", nil); body != "exists" {
		t.Fatalf("check after upload: %d %q", code, body)
	}
	if code, body := do("POST", "/api/album/upload?key="+key, "image/heic", photo); code != 200 || body != "exists" {
		t.Fatalf("re-upload: %d %q", code, body)
	}
	// 前缀相同但不是同一主文件名的不算已同步
	if _, body := do("GET", "/api/album/check?key=20261010_153012_IMG_123", "", nil); body != "missing" {
		t.Fatalf("prefix key: %q", body)
	}
	// 删到回收站的照片不会被再次同步
	if code, _ := do("DELETE", "/api/files/"+f.ID.String(), "", nil); code != http.StatusNoContent {
		t.Fatalf("trash: %d", code)
	}
	if _, body := do("GET", "/api/album/check?key="+key, "", nil); body != "exists" {
		t.Fatalf("check after trash: %q", body)
	}
	if code, _ := do("GET", "/api/album/check?key="+strings.Repeat("x", 300), "", nil); code != http.StatusBadRequest {
		t.Fatalf("long key: %d", code)
	}

	// 相册接口也接受网址里的 ?token=（快捷指令用），其他接口不接受
	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/api/album/check?key=" + key + "&token=tok"); code != http.StatusOK {
		t.Fatalf("album check with ?token=: %d", code)
	}
	if code := get("/api/album/check?key=" + key + "&token=bad"); code != http.StatusUnauthorized {
		t.Fatalf("album check with bad ?token=: %d", code)
	}
	if code := get("/api/folders/root/children?token=tok"); code != http.StatusUnauthorized {
		t.Fatalf("?token= accepted outside album API: %d", code)
	}
}
