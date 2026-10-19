package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

// 集成测试：需要一个空的 PostgreSQL 数据库，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestUploadDownloadIntegration(t *testing.T) {
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
	userID, err := store.EnsureUser(ctx, "it-"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", CORSOrigins: []string{"https://web.example"}, Log: log}).Handler())
	defer ts.Close()

	do := func(method, path string, body io.Reader, hdr ...string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, body)
		req.Header.Set("Authorization", "Bearer tok")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	plain := make([]byte, 3500)
	rand.Read(plain)
	resp := do("POST", "/api/files?name=a.bin", bytes.NewReader(plain))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status %d", resp.StatusCode)
	}
	var f meta.File
	json.NewDecoder(resp.Body).Decode(&f)
	resp.Body.Close()
	if f.Size != int64(len(plain)) {
		t.Fatalf("size = %d", f.Size)
	}
	if parts, _ := store.ListParts(ctx, f.ID); len(parts) != 4 {
		t.Fatalf("parts = %d, want 4", len(parts))
	}

	resp = do("GET", fmt.Sprintf("/api/files/%s/content", f.ID), nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, plain) {
		t.Fatal("download mismatch")
	}

	resp = do("GET", fmt.Sprintf("/api/files/%s/content", f.ID), nil, "Range", "bytes=995-2004")
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, plain[995:2005]) {
		t.Fatalf("range: status %d, match=%v", resp.StatusCode, bytes.Equal(got, plain[995:2005]))
	}

	if resp = do("POST", "/api/files?name=a.bin", bytes.NewReader(plain)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate upload status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/files/%s", ts.URL, f.ID), nil)
	req.Header.Set("X-Xiaoju-Token", "tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("X-Xiaoju-Token auth failed: %v %v", err, resp)
	}
	if resp, _ := http.Get(ts.URL + "/api/folders/root/children"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", resp.StatusCode)
	}
	if resp = do("GET", fmt.Sprintf("/api/files/%s", f.ID), nil, "Origin", "https://web.example"); resp.Header.Get("Access-Control-Allow-Origin") != "https://web.example" {
		t.Fatal("missing CORS header on allowed origin")
	}

	// 签名下载链接：无需认证头，支持 Range；篡改签名或过期时拒绝。
	resp = do("POST", fmt.Sprintf("/api/files/%s/link", f.ID), nil)
	var link struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&link)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || link.URL == "" {
		t.Fatalf("link status %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", ts.URL+link.URL, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, plain[10:20]) {
		t.Fatalf("signed link range: status %d", resp.StatusCode)
	}
	for _, bad := range []string{
		strings.Replace(link.URL, "sig=", "sig=0", 1), // 签名被篡改
		strings.Replace(link.URL, "exp=", "exp=9", 1), // 有效期被延长
	} {
		if resp, _ := http.Get(ts.URL + bad); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bad link %s status %d", bad, resp.StatusCode)
		}
	}

	// CORS：允许的来源预检直接通过（不需要认证），其他来源不返回允许头。
	for origin, want := range map[string]string{"https://web.example": "https://web.example", "https://evil.example": ""} {
		req, _ = http.NewRequest("OPTIONS", ts.URL+"/api/folders/root/children", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != want {
			t.Fatalf("preflight from %s: status %d, allow-origin %q", origin, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		// 网页上重命名、移动用 PATCH，断点续传用 PUT
		if m := resp.Header.Get("Access-Control-Allow-Methods"); want != "" && (!strings.Contains(m, "PATCH") || !strings.Contains(m, "PUT")) {
			t.Fatalf("preflight must allow PATCH and PUT: %q", m)
		}
	}

	if resp = do("DELETE", fmt.Sprintf("/api/files/%s?permanent=1", f.ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge status %d", resp.StatusCode)
	}
	if resp = do("GET", fmt.Sprintf("/api/files/%s", f.ID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after purge status %d", resp.StatusCode)
	}
}
