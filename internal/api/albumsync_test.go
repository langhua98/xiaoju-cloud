package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
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

func TestDateStampAndFormKey(t *testing.T) {
	for in, want := range map[string]string{
		"2026年10月10日 19:55":          "20261010_1955",
		"2026年10月10日 下午7:55":         "20261010_1955",
		"2026年10月10日 上午12:05":        "20261010_0005",
		"2026/10/10 19:55:12":        "20261010_195512",
		"2026-10-10T07:55:12-04:00":  "20261010_075512",
		"Oct 10, 2026 at 7:55:12 PM": "10202675512", // 认不出顺序：数字连起来，仍然稳定
		"":                           "",
	} {
		if got := dateStamp(in); got != want {
			t.Errorf("dateStamp(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"name": "name", " Name\n": "name", "ｎａｍｅ": "name", "DATE ": "date", "file": "file"} {
		if got := fieldName(in); got != want {
			t.Errorf("fieldName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, tc := range []struct{ name, date, file, want string }{
		{"IMG_1234", "2026年10月10日 19:55", "", "20261010_1955_IMG_1234"},
		{" IMG_1234.HEIC\n", "2026年10月10日 19:55", "", "20261010_1955_IMG_1234"},
		{"", "2026年10月10日 19:55", "IMG_1234.HEIC", "20261010_1955_IMG_1234"},
		{"IMG_1234", "", "", "IMG_1234"},
	} {
		if got := formKey(tc.name, tc.date, tc.file); got != tc.want {
			t.Errorf("formKey(%q, %q, %q) = %q, want %q", tc.name, tc.date, tc.file, got, tc.want)
		}
	}
}

// 集成测试：需要一个 PostgreSQL 数据库，设置 XIAOJU_TEST_DATABASE_URL 后运行。
func TestAlbumSyncRunIntegration(t *testing.T) {
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
	driver, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := meta.NewStore(pool)
	userID, err := store.EnsureUser(ctx, "it-"+t.Name()+"-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1 << 20, TempDir: t.TempDir(), Log: log})
	ts := httptest.NewServer(New(svc, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts.Close()

	send := func(method, path, ctype string, body io.Reader) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, body)
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
	// 快捷指令把令牌放在网址里，名称和日期放在表单里
	check := func(name, date string) string {
		t.Helper()
		form := url.Values{" Name ": {name}, "date\n": {date}} // 字段名带空格、大写也能识别
		code, body := send("POST", "/api/album/check?token=tok", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if code != 200 {
			t.Fatalf("check %s: %d %s", name, code, body)
		}
		return body
	}
	upload := func(name, date, filename string, content []byte) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", filename) // 故意把文件放在最前面
		fw.Write(content)
		mw.WriteField("name", name)
		mw.WriteField("date", date)
		mw.Close()
		return send("POST", "/api/album/upload?token=tok", mw.FormDataContentType(), &buf)
	}
	heic := func(fill byte) []byte {
		return append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), bytes.Repeat([]byte{fill}, 3000)...)
	}
	date1, date2, date3 := "2026年10月10日 下午7:55", "2026年10月10日 下午8:01", "2026年10月11日 09:12"

	// 第一轮：3 张里只传了 1 张就“中断”了
	if code, body := send("GET", "/api/album/start?token=tok&total=3", "", nil); code != 200 || !strings.Contains(body, "共 3 张") {
		t.Fatalf("start: %d %q", code, body)
	}
	if got := check("IMG_0001", date1); got != "missing" {
		t.Fatalf("first check = %q", got)
	}
	if code, body := upload("IMG_0001", date1, "IMG_0001.HEIC", heic(1)); code != http.StatusCreated || body != "uploaded" {
		t.Fatalf("upload: %d %q", code, body)
	}
	_, report := send("GET", "/api/album/finish?token=tok", "", nil)
	for _, want := range []string{"共 3 张", "本次新上传 1 张", "之前已备份 0 张", "还没处理 2 张"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report 1 missing %q:\n%s", want, report)
		}
	}

	// 第二轮：从头再跑，已传的跳过，剩下的接着传
	send("GET", "/api/album/start?token=tok&total=3", "", nil)
	if got := check("IMG_0001", date1); got != "exists" {
		t.Fatalf("re-check = %q", got)
	}
	for _, p := range []struct{ name, date string }{{"IMG_0002", date2}, {"IMG_0003", date3}} {
		if got := check(p.name, p.date); got != "missing" {
			t.Fatalf("check %s = %q", p.name, got)
		}
		if code, body := upload(p.name, p.date, p.name+".HEIC", heic(2)); code != http.StatusCreated {
			t.Fatalf("upload %s: %d %q", p.name, code, body)
		}
	}
	_, report = send("GET", "/api/album/finish?token=tok", "", nil)
	for _, want := range []string{"共 3 张", "本次新上传 2 张", "之前已备份 1 张", "全部备份完成"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report 2 missing %q:\n%s", want, report)
		}
	}

	album, err := store.FindFolder(ctx, userID, nil, albumFolder)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := store.ListChildren(ctx, userID, &album.ID)
	var names []string
	for _, f := range items {
		names = append(names, f.Name+"|"+f.Mime)
	}
	want := "20261010_1955_IMG_0001.heic|image/heic,20261010_2001_IMG_0002.heic|image/heic,20261011_0912_IMG_0003.heic|image/heic"
	if strings.Join(names, ",") != want {
		t.Fatalf("album files = %v", names)
	}
	// 重复上传同一张直接返回 exists
	if code, body := upload("IMG_0001", date1, "IMG_0001.HEIC", heic(1)); code != 200 || body != "exists" {
		t.Fatalf("duplicate upload: %d %q", code, body)
	}
}
