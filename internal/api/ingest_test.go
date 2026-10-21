package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/crypto"
	"github.com/langhua98/xiaoju-cloud/internal/db"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
	"github.com/langhua98/xiaoju-cloud/internal/storage"
	"github.com/langhua98/xiaoju-cloud/internal/storage/local"
	"github.com/langhua98/xiaoju-cloud/migrations"
)

func TestIngestName(t *testing.T) {
	req := func(q, header string) *http.Request {
		r := httptest.NewRequest("POST", "/api/ingest"+q, nil)
		if header != "" {
			r.Header.Set("X-File-Name", header)
		}
		return r
	}
	for _, c := range []struct {
		r    *http.Request
		ext  string
		want string
	}{
		{req("", "IMG_0001"), ".heic", "IMG_0001.heic"},      // 快捷指令给的名字没有扩展名
		{req("", "IMG_0001.HEIC"), ".heic", "IMG_0001.HEIC"}, // 有扩展名就不动
		{req("?name=a%2Fb.jpg", ""), ".jpg", "a_b.jpg"},      // 不能带路径
		{req("", "%E5%9B%BE.png"), ".png", "图.png"},          // URL 编码的中文名
	} {
		if got, ok := ingestName(c.r, c.ext); !ok || got != c.want {
			t.Errorf("got %q %v, want %q", got, ok, c.want)
		}
	}
	// 没给名字（快捷指令取不到文件名时会给“.”）：不再自动起名
	for _, h := range []string{"", ".", "..", " . "} {
		if got, ok := ingestName(req("", h), ".jpg"); ok {
			t.Errorf("name %q must be rejected, got %q", h, got)
		}
	}
	if got, _ := ingestName(req("", strings.Repeat("长", 120)+".jpg"), ""); len(got) > 200 || !strings.HasSuffix(got, ".jpg") {
		t.Errorf("long name not truncated safely: %d bytes", len(got))
	}
	if stripCopySuffix("IMG_1 (2).jpg") != "IMG_1.jpg" || withCopySuffix("IMG_1.jpg", 3) != "IMG_1 (3).jpg" {
		t.Error("copy suffix")
	}
}

// 集成测试：不需要认证就能往“相册”里传文件；已传过的跳过；同名不同内容改名；但不能用来看文件。
func TestIngest(t *testing.T) {
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
	userID, _ := store.EnsureUser(ctx, "it-ingest-"+uuid.NewString()[:8]) // 每次用新用户，相册从空开始
	driver, _ := local.New(t.TempDir())
	master, _ := crypto.NewKey()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(store, driver, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	srv := New(svc, Options{UserID: userID, Token: "tok", Log: log})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	call := func(method, path, body string, hdr ...string) (int, map[string]any) {
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

	jpeg := "\xFF\xD8\xFF\xE0" + strings.Repeat("x", 2500) // 跨多个分块
	ingest := func(name, body string) (int, map[string]any) {
		return call("POST", "/api/ingest", body, "X-File-Name", name)
	}
	if code, out := ingest("IMG_0001", jpeg); code != 201 || out["name"] != "IMG_0001.jpg" || out["result"] != "uploaded" {
		t.Fatalf("ingest: %d %v", code, out)
	}
	if code, out := ingest("IMG_0001", jpeg); code != 200 || out["result"] != "skipped" {
		t.Fatalf("same photo again must be skipped: %d %v", code, out)
	}
	if code, out := ingest("img_0001.JPG", jpeg); code != 200 || out["result"] != "skipped" {
		t.Fatalf("case-insensitive duplicate: %d %v", code, out)
	}
	if code, out := ingest("IMG_0001", jpeg+"y"); code != 201 || out["name"] != "IMG_0001 (2).jpg" {
		t.Fatalf("same name, different content: %d %v", code, out)
	}
	// 只进不出：不带认证不能列目录、下载
	if code, _ := call("GET", "/api/folders/root/children", ""); code != 401 {
		t.Fatalf("anonymous must not list files: %d", code)
	}

	// 文件都在“相册”里，内容能正确解密
	_, root := call("GET", "/api/folders/root/children", "", tokenH...)
	var albumID string
	for _, it := range root["items"].([]any) {
		if m := it.(map[string]any); m["name"] == AlbumFolder {
			albumID = m["id"].(string)
		}
	}
	_, album := call("GET", "/api/folders/"+albumID+"/children", "", tokenH...)
	if n := len(album["items"].([]any)); n != 2 {
		t.Fatalf("album has %d files, want 2", n)
	}
	first := album["items"].([]any)[1].(map[string]any)
	if first["mime"] != "image/jpeg" {
		t.Fatalf("mime from content: %v", first)
	}
	if code, _ := call("GET", "/api/files/"+first["id"].(string)+"/content", ""); code != 401 {
		t.Fatalf("anonymous must not download: %d", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/files/"+first["id"].(string)+"/content", nil)
	req.Header.Set("X-Xiaoju-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != jpeg {
		t.Fatalf("content mismatch: %d bytes", len(got))
	}

	// 大文件写存储端很慢：先回 200，期间发空格保持连接，最后才是 JSON
	ingestHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { ingestHeartbeat = 10 * time.Second })
	slow := service.New(store, slowDriver{driver, 120 * time.Millisecond}, service.Options{MasterKey: master, PartSize: 1000, TempDir: t.TempDir(), Log: log})
	ts2 := httptest.NewServer(New(slow, Options{UserID: userID, Token: "tok", Log: log}).Handler())
	defer ts2.Close()
	video := "\x00\x00\x00\x14ftypqt  " + strings.Repeat("v", 2990) // 3 个分块，写入约 360ms
	req, _ = http.NewRequest("POST", ts2.URL+"/api/ingest", strings.NewReader(video))
	req.Header.Set("X-File-Name", "VID_0001")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response with heartbeats must still be valid JSON: %v %q", err, raw)
	}
	spaces := len(raw) - len(strings.TrimLeft(string(raw), " "))
	if resp.StatusCode != 200 || spaces < 3 || out["result"] != "uploaded" || out["name"] != "VID_0001.mov" {
		t.Fatalf("slow ingest: %d, %d heartbeat spaces, %v", resp.StatusCode, spaces, out)
	}
	if code, out := ingest("VID_0001", video); code != 200 || out["result"] != "skipped" {
		t.Fatalf("video re-run must be skipped: %d %v", code, out)
	}
	ingestHeartbeat = 10 * time.Second // 后面的上传要按正常情况回 201

	// 隐藏相册：X-Xiaoju-Album: hidden 放进根目录的“隐藏相册”，和“相册”分开查重；不认识的值拒绝（不能随意建文件夹）
	if code, out := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0001", "X-Xiaoju-Album", "hidden"); code != 201 || out["name"] != "IMG_0001.jpg" {
		t.Fatalf("hidden album ingest: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0001", "X-Xiaoju-Album", "Hidden"); code != 200 || out["result"] != "skipped" {
		t.Fatalf("hidden album re-run must be skipped: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0002", "X-Xiaoju-Album", "../etc"); code != 400 {
		t.Fatalf("unknown album must be rejected: %d", code)
	}
	_, root = call("GET", "/api/folders/root/children", "", tokenH...)
	var hiddenID string
	for _, it := range root["items"].([]any) {
		if m := it.(map[string]any); m["name"] == HiddenAlbumFolder {
			hiddenID = m["id"].(string)
		}
	}
	_, hidden := call("GET", "/api/folders/"+hiddenID+"/children", "", tokenH...)
	if hiddenID == "" || len(hidden["items"].([]any)) != 1 {
		t.Fatalf("hidden album folder: %q %v", hiddenID, hidden)
	}

	// 先问再传：同名同大小才算传过；名字没有扩展名时只比前面的部分；大小看不懂就当没传过
	size := strconv.Itoa(len(jpeg))
	for _, c := range []struct {
		name, size, album, want string
	}{
		{"IMG_0001.JPG", size, "hidden", "exists"},
		{"IMG_0001.jpg", size, "", "exists"},
		{"IMG_0001", size, "", "exists"},
		{"IMG_0001 (3).jpg", size, "", "exists"},
		{"IMG_0001.jpg", "1," + size[1:], "", "missing"}, // 大小不一样
		{"IMG_0001.jpg", "2.5 KB", "", "missing"},
		{"IMG_0001.jpg", "", "", "missing"},
		{"IMG_0009.jpg", size, "", "missing"},
	} {
		code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", c.name, "X-File-Size", c.size, "X-Xiaoju-Album", c.album)
		if code != 200 || out["result"] != c.want {
			t.Errorf("exists %q size %q album %q: %d %v, want %s", c.name, c.size, c.album, code, out, c.want)
		}
	}
	if len(jpeg) >= 1000 {
		if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_0001.jpg", "X-File-Size", size[:1]+","+size[1:]); code != 200 || out["result"] != "exists" {
			t.Errorf("size with a thousands separator: %d %v", code, out)
		}
	}
	if code, _ := call("GET", "/api/ingest/exists", "", "X-File-Name", "a.jpg", "X-File-Size", "1", "X-Xiaoju-Album", "x"); code != 400 {
		t.Errorf("unknown album must be rejected: %d", code)
	}

	// 没带文件名：拒绝（以前会自动起名“照片 日期…”）
	if code, out := call("POST", "/api/ingest", jpeg+"zz", "X-File-Name", ".", "X-Xiaoju-Album", "hidden"); code != 400 || out["code"] != "missing_name" {
		t.Fatalf("upload without a name must be rejected: %d %v", code, out)
	}
	// 以前没带文件名传上来、被自动起名的：同一个文件夹里大小完全一样的，先问时改回真名、当作传过
	video2 := "\x00\x00\x00\x14ftypqt  " + strings.Repeat("w", 1234)
	if code, out := call("POST", "/api/ingest", video2, "X-File-Name", "照片 2026-10-16 035540-982f", "X-Xiaoju-Album", "hidden"); code != 201 || out["name"] != "照片 2026-10-16 035540-982f.mov" {
		t.Fatalf("placeholder-named upload: %d %v", code, out)
	}
	code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_0500.MOV", "X-File-Size", strconv.Itoa(len(video2)), "X-Xiaoju-Album", "hidden")
	if code != 200 || out["result"] != "exists" || out["name"] != "IMG_0500.MOV" {
		t.Fatalf("placeholder upload must be renamed and reported as existing: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest", video2, "X-File-Name", "IMG_0500.MOV", "X-Xiaoju-Album", "hidden"); code != 200 || out["result"] != "skipped" {
		t.Fatalf("renamed file must be skipped: %d %v", code, out)
	}
	_, hidden = call("GET", "/api/folders/"+hiddenID+"/children", "", tokenH...)
	names := []string{}
	for _, it := range hidden["items"].([]any) {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	sort.Strings(names) // 列目录按拍摄 / 上传时间排，这里只看名字
	if strings.Join(names, ",") != "IMG_0001.jpg,IMG_0500.MOV" {
		t.Fatalf("hidden album after rename: %v", names)
	}

	// 已有文件“名字|大小”的摘要：快捷指令一次取回，在手机上比对（只给 SHA-256，不给名字）
	getNames := func(album string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/api/ingest/names", nil)
		req.Header.Set("X-Xiaoju-Album", album)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// 换过手机后编号重复的另一张照片：自动改名为“ (2)”；摘要里也有去掉序号的那一行，再运行时在手机上就能认出来
	other := "\xFF\xD8\xFF\xE0" + strings.Repeat("y", 777)
	if code, out := call("POST", "/api/ingest", other, "X-File-Name", "IMG_0001.jpg", "X-Xiaoju-Album", "hidden"); code != 201 || out["name"] != "IMG_0001 (2).jpg" {
		t.Fatalf("different photo with the same name: %d %v", code, out)
	}
	code, body := getNames("hidden")
	hashes := strings.Fields(body)
	sort.Strings(hashes)
	var want []string
	for _, k := range []string{"IMG_0001.jpg|" + size, "IMG_0500.MOV|" + strconv.Itoa(len(video2)), "IMG_0001 (2).jpg|" + strconv.Itoa(len(other)), "IMG_0001.jpg|" + strconv.Itoa(len(other))} {
		sum := sha256.Sum256([]byte(k))
		want = append(want, hex.EncodeToString(sum[:]))
	}
	sort.Strings(want)
	if code != 200 || strings.Join(hashes, ",") != strings.Join(want, ",") || strings.Contains(body, "IMG_") {
		t.Fatalf("names of the hidden album: %d %q", code, body)
	}
	if code, _ := getNames("x"); code != 400 {
		t.Errorf("unknown album must be rejected: %d", code)
	}

	// 整个文件夹的清单一次问：名字不带扩展名，大小是“2.5 KB”这样的文字；回答还没传过的是第几个
	plan := func(body string) (int, map[string]any) {
		t.Helper()
		return call("POST", "/api/ingest/plan", body, "Content-Type", "application/json")
	}
	kb := func(n int) string { return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + " KB" }
	planNames := "IMG_0001\nIMG_0500\nIMG_0002\nimg_0001\nIMG_0500"
	sizesText := kb(len(jpeg)) + "\n" + kb(len(video2)) + "\n5 KB\n" + strconv.Itoa(len(other)) + " bytes\n9.9 MB"
	code, out = plan(fmt.Sprintf(`{"album":"hidden","names":%q,"sizes":%q}`, planNames, sizesText))
	// 第 3 个没传过；第 5 个同名但大小不一样（换过手机后编号重复的另一个视频）
	if code != 200 || out["todo"] != "3\n5" || out["count"] != 2.0 || out["uploaded"] != 3.0 {
		t.Fatalf("plan: %d %v", code, out)
	}
	if code, out := plan(`{"album":"","names":"IMG_0001","sizes":"5 KB"}`); code != 200 || out["todo"] != "1" {
		t.Fatalf("plan for the other album: %d %v", code, out)
	}
	// 名字里有制表符：上传时存成“clip_.mov”；清单里原样带着制表符，或者制表符丢了，都要认出是传过的
	clip := "\x00\x00\x00\x14ftypqt  " + strings.Repeat("c", 3000)
	if code, out := call("POST", "/api/ingest", clip, "X-File-Name", "clip\t.mov", "X-Xiaoju-Album", "hidden"); code != 201 || out["name"] != "clip_.mov" {
		t.Fatalf("upload with a tab in the name: %d %v", code, out)
	}
	for _, n := range []string{"clip\t", "clip", "clip_"} {
		body, _ := json.Marshal(map[string]string{"album": "hidden", "names": n + "\nnew one", "sizes": kb(len(clip)) + "\n3 KB"})
		if code, out := plan(string(body)); code != 200 || out["todo"] != "2" {
			t.Errorf("plan for %q: %d %v", n, code, out)
		}
	}
	if code, _ := plan(`{"album":"hidden","names":"a\nb","sizes":"1 KB"}`); code != 400 {
		t.Errorf("names and sizes of different lengths must be rejected: %d", code)
	}

	// 从相册直接读：没有大小，名字一样还要拍摄日期前后差一天以内（网盘里的拍摄时间是生成缩略图时读出来的）
	// “相册”里有两个 IMG_0001：IMG_0001.jpg 和换过手机后编号重复的 IMG_0001 (2).jpg
	for name, taken := range map[string]string{"IMG_0001.jpg": "2026-10-18 07:04:00+00", "IMG_0001 (2).jpg": "2024-05-01 12:00:00+00"} {
		if _, err := pool.Exec(ctx, `UPDATE files SET taken_at = $3 WHERE user_id = $1 AND name = $2
			AND parent_id = (SELECT id FROM files WHERE user_id = $1 AND name = $4 AND parent_id IS NULL)`,
			userID, name, taken, AlbumFolder); err != nil {
			t.Fatal(err)
		}
	}
	datePlan := func(album string, names, dates []string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"album": album, "names": strings.Join(names, "\n"), "dates": strings.Join(dates, "\n")})
		return plan(string(body))
	}
	rep := func(s string, n int) []string { return strings.Split(strings.Repeat(s+"\n", n-1)+s, "\n") }
	for _, c := range []struct {
		why          string
		album        string
		names, dates []string
		todo         string
	}{
		{"same day, a day apart, the other IMG_0001, another year, a new name", "",
			[]string{"IMG_0001", "IMG_0001", "IMG_0001", "IMG_0001", "IMG_7777", "IMG_0001.HEIC"},
			[]string{"2026年10月18日 15:04", "Oct 19, 2026 at 1:00 AM", "2024年5月2日 08:00", "2019年3月1日 10:00", "2026年10月18日 15:05", "2026/10/18 15:04"}, "4\n5"},
		{"no dates: name only", "", []string{"IMG_0001", "IMG_7777"}, nil, "2"},
		{"dates do not line up: name only", "", []string{"IMG_0001", "IMG_7777"}, []string{"2019年3月1日"}, "2"},
		{"unreadable date: name only", "", []string{"IMG_0001"}, []string{"昨天"}, ""},
		{"stored file without a capture time: name only", "hidden", []string{"IMG_0500"}, []string{"2019年3月1日"}, ""},
		{"no date ever agrees: the dates must be misread", "", rep("IMG_0001", 5), rep("2019年3月1日", 5), ""},
		{"some dates agree: the others really are other photos", "",
			rep("IMG_0001", 6), append(rep("2019年3月1日", 5), "2026年10月18日"), "1\n2\n3\n4\n5"},
	} {
		code, out := datePlan(c.album, c.names, c.dates)
		if code != 200 || out["todo"] != c.todo || out["files"] != float64(len(c.names)) {
			t.Errorf("%s: %d %v, want todo %q", c.why, code, out, c.todo)
		}
	}

	// 列目录时文件夹带上里面有几张照片、几个视频（连同子文件夹）
	if code, out := call("POST", "/api/folders", `{"name":"子文件夹","parent_id":"`+albumID+`"}`, tokenH...); code != 201 {
		t.Fatalf("create subfolder: %d %v", code, out)
	} else if code, _ := call("POST", "/api/files?name=a.txt&parent_id="+out["id"].(string), "hello", tokenH...); code != 201 {
		t.Fatalf("upload into subfolder: %d", code)
	}
	_, root = call("GET", "/api/folders/root/children", "", tokenH...)
	counts := map[string]string{}
	for _, it := range root["items"].([]any) {
		m := it.(map[string]any)
		c := m["counts"].(map[string]any)
		counts[m["name"].(string)] = fmt.Sprintf("%v/%v/%v", c["photos"], c["videos"], c["others"])
	}
	// 相册：IMG_0001.jpg、IMG_0001 (2).jpg，VID_0001.mov，子文件夹里的 a.txt；隐藏相册：两张照片，IMG_0500.MOV、clip_.mov
	if counts[AlbumFolder] != "2/1/1" || counts[HiddenAlbumFolder] != "2/2/0" {
		t.Fatalf("folder counts: %v", counts)
	}
	// 进了文件夹，统计栏只算这个文件夹里的（连同子文件夹）
	if code, st := call("GET", "/api/stats?folder="+albumID, "", tokenH...); code != 200 ||
		st["files"] != 4.0 || st["photos"] != 2.0 || st["videos"] != 1.0 || st["others"] != 1.0 || st["folders"] != 1.0 {
		t.Fatalf("album stats: %d %v", code, st)
	}
	if code, st := call("GET", "/api/stats", "", tokenH...); code != 200 || st["files"] != 8.0 || st["folders"] != 3.0 {
		t.Fatalf("drive stats: %d %v", code, st)
	}
	if code, _ := call("GET", "/api/stats?folder="+uuid.NewString(), "", tokenH...); code != 404 {
		t.Errorf("stats of an unknown folder: %d", code)
	}
	if code, _ := call("GET", "/api/stats?folder=x", "", tokenH...); code != 400 {
		t.Errorf("stats with a bad folder id: %d", code)
	}

	// App 按手机相册上传：放进“相册”下面同名的子文件夹，exists 也按子文件夹查
	trip := "%E6%97%85%E8%A1%8C" // 旅行
	if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_0900.jpg", "X-File-Size", "2504", "X-Xiaoju-Album-Name", trip); code != 200 || out["result"] != "missing" {
		t.Fatalf("exists before the sub-album is created: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest", jpeg, "X-File-Name", "IMG_0900.jpg", "X-Xiaoju-Album-Name", trip); code != 201 || out["result"] != "uploaded" {
		t.Fatalf("ingest into a sub-album: %d %v", code, out)
	}
	if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "IMG_0900.jpg", "X-File-Size", "2504", "X-Xiaoju-Album-Name", trip); code != 200 || out["result"] != "exists" {
		t.Fatalf("exists in the sub-album: %d %v", code, out)
	}
	_, kids := call("GET", "/api/folders/"+albumID+"/children", "", tokenH...)
	found := false
	for _, it := range kids["items"].([]any) {
		if m := it.(map[string]any); m["name"] == "旅行" && m["type"] == "folder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sub-album folder missing: %v", kids)
	}
	if code, _ := call("POST", "/api/ingest", jpeg, "X-File-Name", "a.jpg", "X-Xiaoju-Album", "hidden", "X-Xiaoju-Album-Name", trip); code != 400 {
		t.Errorf("sub-album under the hidden album: %d", code)
	}
	if code, _ := call("POST", "/api/ingest", jpeg, "X-File-Name", "a.jpg", "X-Xiaoju-Album-Name", "."); code != 400 {
		t.Errorf("bad sub-album name: %d", code)
	}
	// App 的断点续传：分两段传，中间断开后问进度接着传；再开始一次同一个文件就是“传过了”
	big := "\xFF\xD8\xFF\xE0" + strings.Repeat("r", 2996)
	upHdr := []string{"X-File-Name", "IMG_0950.JPG", "X-File-Size", "3000", "X-Xiaoju-Album-Name", trip}
	code, up := call("POST", "/api/ingest/uploads", "", upHdr...)
	id, _ := up["id"].(string)
	if code != 200 || up["result"] != "started" || id == "" || up["offset"] != 0.0 {
		t.Fatalf("start ingest upload: %d %v", code, up)
	}
	if code, out := call("PUT", "/api/ingest/uploads/"+id+"?offset=0", big[:1200]); code != 200 || out["offset"] != 1200.0 {
		t.Fatalf("first piece: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest/uploads", "", upHdr...); code != 200 || out["id"] != id || out["offset"] != 1200.0 {
		t.Fatalf("restart must resume: %d %v", code, out)
	}
	if code, out := call("GET", "/api/ingest/uploads/"+id, ""); code != 200 || out["offset"] != 1200.0 {
		t.Fatalf("upload status: %d %v", code, out)
	}
	if code, out := call("PUT", "/api/ingest/uploads/"+id+"?offset=1200", big[1200:]); code != 201 || out["file"] == nil {
		t.Fatalf("last piece: %d %v", code, out)
	}
	if code, out := call("POST", "/api/ingest/uploads", "", upHdr...); code != 200 || out["result"] != "skipped" {
		t.Fatalf("finished upload must be skipped: %d %v", code, out)
	}
	// 通讯录、日历这类放进“手机数据”下面的子文件夹
	if code, out := call("POST", "/api/ingest", "BEGIN:VCARD\nEND:VCARD\n", "X-File-Name", "contacts.vcf", "X-Xiaoju-Album", "data", "X-Xiaoju-Album-Name", "%E9%80%9A%E8%AE%AF%E5%BD%95"); code != 201 || out["result"] != "uploaded" {
		t.Fatalf("ingest phone data: %d %v", code, out)
	}
	// 「文件」App 的文件夹同步：“手机数据”下面可以多层（保留目录结构）；“相册”下面只有一层
	nested := url.PathEscape("文件/下载/报告")
	if code, out := call("POST", "/api/ingest", "hello", "X-File-Name", "a.txt", "X-Xiaoju-Album", "data", "X-Xiaoju-Album-Name", nested); code != 201 || out["result"] != "uploaded" {
		t.Fatalf("ingest nested phone data: %d %v", code, out)
	}
	if code, out := call("GET", "/api/ingest/exists", "", "X-File-Name", "a.txt", "X-File-Size", "5", "X-Xiaoju-Album", "data", "X-Xiaoju-Album-Name", nested); code != 200 || out["result"] != "exists" {
		t.Fatalf("exists in nested folder: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/ingest", "x", "X-File-Name", "b.txt", "X-Xiaoju-Album", "data", "X-Xiaoju-Album-Name", url.PathEscape("a/../b")); code != 400 {
		t.Fatalf("'..' segment must be rejected: %d", code)
	}

	// 恢复到手机：网页（已登录）换恢复凭证，App 拿它列出、下载
	if code, _ := call("GET", "/api/restore/list?folder="+url.QueryEscape(PhoneDataFolder), "", "X-Xiaoju-Restore", "r1.9999999999.bad"); code != 401 {
		t.Fatalf("forged restore token must be rejected: %d", code)
	}
	code, grant := call("POST", "/api/restore/grant", "{}", tokenH...)
	if code != 200 || grant["token"] == nil {
		t.Fatalf("restore grant: %d %v", code, grant)
	}
	restoreH := []string{"X-Xiaoju-Restore", grant["token"].(string)}
	if code, _ := call("GET", "/api/restore/list?folder=other", "", restoreH...); code != 400 {
		t.Fatalf("only the album and phone data folders can be restored: %d", code)
	}
	code, list := call("GET", "/api/restore/list?folder="+url.QueryEscape(PhoneDataFolder), "", restoreH...)
	var fileID string
	for _, it := range list["items"].([]any) {
		if m := it.(map[string]any); m["path"] == PhoneDataFolder+"/文件/下载/报告/a.txt" {
			fileID = m["id"].(string)
		}
	}
	if code != 200 || fileID == "" {
		t.Fatalf("restore list: %d %v", code, list)
	}
	dl, _ := http.NewRequest("GET", ts.URL+"/api/restore/files/"+fileID, nil)
	dl.Header.Set(restoreH[0], restoreH[1])
	dlResp, err := http.DefaultClient.Do(dl)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(dlResp.Body)
	dlResp.Body.Close()
	if dlResp.StatusCode != 200 || string(got) != "hello" {
		t.Fatalf("restore download: %d %q", dlResp.StatusCode, got)
	}
}

// slowDriver 模拟写 Telegram 很慢（大视频）
type slowDriver struct {
	storage.Driver
	delay time.Duration
}

func (d slowDriver) Put(ctx context.Context, req storage.PutRequest) (storage.Location, error) {
	time.Sleep(d.delay)
	return d.Driver.Put(ctx, req)
}

// iPhone 后台上传开始前的 OPTIONS：还不支持断点续传，回 501。
func TestIngestPreflight(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	s.ingestPreflight(w, httptest.NewRequest(http.MethodOptions, "/api/ingest", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", w.Code)
	}
}

// 收大文件时按字节数记进度；读请求体出错时记下错误。
func TestIngestProgress(t *testing.T) {
	var buf strings.Builder
	old := ingestProgressBytes
	ingestProgressBytes = 1000
	defer func() { ingestProgressBytes = old }()
	p := &ingestProgress{r: iotest.OneByteReader(strings.NewReader(strings.Repeat("x", 2500))), total: 2500, start: time.Now(),
		next: ingestProgressBytes, last: time.Now(), log: slog.New(slog.NewTextHandler(&buf, nil))}
	if _, err := io.Copy(io.Discard, io.LimitReader(p, 2500)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "ingest progress"); n < 2 {
		t.Errorf("progress lines = %d, want at least 2:\n%s", n, buf.String())
	}
	if p.n != 2500 || p.err != nil {
		t.Errorf("n=%d err=%v", p.n, p.err)
	}

	broken := &ingestProgress{r: io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(io.ErrUnexpectedEOF))}
	io.Copy(io.Discard, broken)
	if broken.n != 3 || !errors.Is(broken.err, io.ErrUnexpectedEOF) {
		t.Errorf("n=%d err=%v", broken.n, broken.err)
	}
}
