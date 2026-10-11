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
	for in, want := range map[string]string{
		"2026年10月10日 下午7:55":         "20261010_195500",
		"2026/10/10 19:55:12":        "20261010_195512",
		"Oct 10, 2026 at 7:55:12 PM": "", // 认不出年月日顺序，不能用来推进进度
		"2026年13月40日 19:55":          "",
		"":                           "",
	} {
		if got, ok := cursorStamp(in); got != want || ok != (want != "") {
			t.Errorf("cursorStamp(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]string{"": "1971-01-01 00:00", "20261010_195512": "2026-10-10 19:54", "20261010_000000": "2026-10-09 23:59"} {
		if got := cursorDate(in); got != want {
			t.Errorf("cursorDate(%q) = %q, want %q", in, got, want)
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
	uploadNoFile := func(name, date string) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("name", name)
		mw.WriteField("date", date)
		mw.Close()
		return send("POST", "/api/album/upload?token=tok", mw.FormDataContentType(), &buf)
	}
	start := func(query, want string) {
		t.Helper()
		if code, body := send("GET", "/api/album/start?token=tok"+query, "", nil); code != 200 || body != want {
			t.Fatalf("start%s: %d %q, want %q", query, code, body, want)
		}
	}
	finish := func(wants ...string) {
		t.Helper()
		_, report := send("GET", "/api/album/finish?token=tok", "", nil)
		for _, want := range wants {
			if !strings.Contains(report, want) {
				t.Fatalf("report missing %q:\n%s", want, report)
			}
		}
	}
	expect := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s = %q, want %q", what, got, want)
		}
	}
	uploaded := func(name, date string, fill byte) {
		t.Helper()
		if code, body := upload(name, date, name+".HEIC", heic(fill)); code != http.StatusCreated || body != "uploaded" {
			t.Fatalf("upload %s: %d %q", name, code, body)
		}
	}
	date1, date2, date3 := "2026年10月10日 下午7:55", "2026年10月10日 下午8:01", "2026年10月11日 09:12"

	// 第一轮：还没有进度，从很早的日期开始；传完第 1 张、检查完第 2 张时被打断（没走到 finish）
	start("&limit=2", "1971-01-01 00:00")
	expect("check 1", check("IMG_0001", date1), "missing")
	uploaded("IMG_0001", date1, 1)
	expect("check 2", check("IMG_0002", date2), "missing")

	// 第二轮：从第 1 张前 1 分钟接着查，第 1 张跳过，第 2 张接着传；查到的张数等于上限，说明后面还有
	start("&limit=2", "2026-10-10 19:54")
	expect("re-check 1", check("IMG_0001", date1), "exists")
	expect("check 2", check("IMG_0002", date2), "missing")
	uploaded("IMG_0002", date2, 2)
	finish("本次检查 2 张", "新上传 1 张", "之前已备份 1 张", "已备份到 2026年10月10日 20:01", "共 2 个文件", "后面还有照片")

	// 第三轮：只剩第 3 张
	start("&limit=3", "2026-10-10 20:00")
	expect("re-check 2", check("IMG_0002", date2), "exists")
	expect("check 3", check("IMG_0003", date3), "missing")
	uploaded("IMG_0003", date3, 3)
	finish("本次检查 2 张", "新上传 1 张", "已备份到 2026年10月11日 09:12", "共 3 个文件", "全部备份完成")
	start("&limit=3", "2026-10-11 09:11")
	finish("没有新照片", "全部备份完成")

	// 排序选错（最新的排最前）：进度不会跳到最新那张，汇总里提醒
	start("&limit=3&reset=1", "1971-01-01 00:00")
	expect("newest first", check("IMG_0003", date3), "exists")
	expect("then older", check("IMG_0001", date1), "exists")
	finish("最旧的排最前")
	start("&limit=3", "1971-01-01 00:00")

	// 上传失败：进度停在失败那张的前一张，下次从它重试
	expect("check 1", check("IMG_0001", date1), "exists")
	expect("check 2", check("IMG_0002", date2), "exists")
	if code, body := uploadNoFile("IMG_0003", date3); code != http.StatusBadRequest || !strings.HasPrefix(body, "failed") {
		t.Fatalf("upload without file: %d %q", code, body)
	}
	finish("上传失败 1 张", "已备份到 2026年10月10日 20:01", "从没传好的那张接着传")
	start("&limit=3", "2026-10-10 20:00")
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
