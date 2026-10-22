package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/media"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 上传链接（给 AI agent 用）：网页上生成一个链接 https://<服务端>/u/<token>，拿到链接的人（或 AI agent）
// 不用登录就能往一个文件夹里传文件：浏览器打开是上传页面，curl 直接传。只能传，不能列目录、下载、删除；
// 可以设有效期，随时撤销。随机串 144 位，猜不到；不需要认证的接口都按 IP 限速。
//
//	POST   /api/drops {"name": "备注", "folder_id": 文件夹（不填是根目录的“AI 上传”）, "expires_days": 7（0 不过期）} → 201 链接
//	GET    /api/drops → {"items": [链接…]}
//	DELETE /api/drops/{id}
//
// 不需要登录：
//
//	GET  /u/{token}            浏览器（Accept: text/html）是上传页面；Accept: application/json 或 ?format=json 是 JSON 说明；
//	                           其他（curl）是纯文字说明
//	PUT  /u/{token}/{文件名}    请求体是文件原始内容（curl -T 文件 https://…/u/<token>/）
//	POST /u/{token}            multipart/form-data（可以一次多个文件），或请求体是文件原始内容、名字放在 ?name= / X-File-Name
//	     → 201 {"files": [{"name","size","mime","sha256"}]}
//	     链接没有 / 撤销了 404 link_missing，过期 410 link_expired
//
// 同名的文件自动改名“名字 (2)”，不会覆盖。

// DropFolder 是上传链接默认放文件的文件夹（根目录下）。
const DropFolder = "AI 上传"

const (
	maxDropExpiryDays = 3650
	maxDropParts      = 1000 // 一个 multipart 请求最多几个文件
)

type dropView struct {
	*meta.DropLink
	Expired bool   `json:"expired"`
	Trashed bool   `json:"trashed,omitempty"`
	Path    string `json:"path"` // 链接地址（相对服务端）：/u/<token>
}

func viewDrop(d *meta.DropLink) dropView {
	return dropView{DropLink: d, Expired: d.Expired(time.Now()), Trashed: d.Folder.TrashedAt != nil, Path: "/u/" + d.Token}
}

func (s *Server) createDrop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string     `json:"name"`
		FolderID    *uuid.UUID `json:"folder_id"`
		ExpiresDays int        `json:"expires_days"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.ExpiresDays < 0 || req.ExpiresDays > maxDropExpiryDays {
		writeError(w, http.StatusBadRequest, "expires_days out of range")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len([]rune(name)) > 100 {
		writeError(w, http.StatusBadRequest, "name too long")
		return
	}
	var folder uuid.UUID
	if req.FolderID != nil {
		folder = *req.FolderID
	} else {
		f, err := s.albumFolder(r, DropFolder)
		if err != nil {
			s.fail(w, err)
			return
		}
		folder = f.ID
	}
	raw := make([]byte, 18)
	rand.Read(raw)
	d := &meta.DropLink{UserID: s.userID, FolderID: folder, Name: name, Token: base64.RawURLEncoding.EncodeToString(raw)}
	if req.ExpiresDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		d.ExpiresAt = &t
	}
	out, err := s.svc.Store().CreateDropLink(r.Context(), d)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, viewDrop(out))
}

func (s *Server) listDrops(w http.ResponseWriter, r *http.Request) {
	drops, err := s.svc.Store().ListDropLinks(r.Context(), s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	items := make([]dropView, len(drops))
	for i, d := range drops {
		items[i] = viewDrop(d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) deleteDrop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store().DeleteDropLink(r.Context(), s.userID, id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dropFor 找到链接对应的上传链接；没有、过期、文件夹删了时直接回复错误并返回 nil。
func (s *Server) dropFor(w http.ResponseWriter, r *http.Request) *meta.DropLink {
	d, err := s.svc.Store().DropLinkByToken(r.Context(), r.PathValue("token"))
	switch {
	case errors.Is(err, meta.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this upload link does not exist or was revoked", "code": "link_missing"})
		return nil
	case err != nil:
		s.fail(w, err)
		return nil
	case d.Expired(time.Now()):
		writeJSON(w, http.StatusGone, map[string]string{"error": "this upload link has expired", "code": "link_expired"})
		return nil
	case d.Folder.TrashedAt != nil || d.Folder.Status != meta.StatusActive:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the target folder was deleted", "code": "link_missing"})
		return nil
	}
	return d
}

// dropHeaders：链接里带着凭证，不让浏览器把它放进 Referer、不让搜索引擎收录；任何来源都可以调用
// （不用 Cookie，凭证就在地址里，放开跨域没有额外风险，浏览器里跑的 agent 也能传）。
func dropHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
}

func (s *Server) dropPreflight(w http.ResponseWriter, r *http.Request) {
	dropHeaders(w)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT")
	h.Set("Access-Control-Allow-Headers", "Content-Type, X-File-Name")
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// publicBase 是服务端对外的地址（HF Space 在代理后面，看 X-Forwarded-Proto）。
func publicBase(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme, _, _ = strings.Cut(p, ",")
		scheme = strings.TrimSpace(scheme)
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// GET /u/{token}
func (s *Server) dropPage(w http.ResponseWriter, r *http.Request) {
	dropHeaders(w)
	d := s.dropFor(w, r)
	if d == nil {
		return
	}
	link := publicBase(r) + "/u/" + d.Token
	info := dropInfo{Link: link, Folder: d.Folder.Name, Label: d.Name, ExpiresAt: d.ExpiresAt}
	format := r.URL.Query().Get("format")
	accept := r.Header.Get("Accept")
	switch {
	case format == "json" || (format == "" && strings.Contains(accept, "application/json")):
		writeJSON(w, http.StatusOK, info.json())
	case format == "html" || (format == "" && strings.Contains(accept, "text/html")):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'self'")
		dropPageTmpl.Execute(w, info)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, info.text())
	}
}

type dropInfo struct {
	Link      string
	Folder    string
	Label     string
	ExpiresAt *time.Time
}

func (i dropInfo) Expires() string {
	if i.ExpiresAt == nil {
		return "never"
	}
	return i.ExpiresAt.UTC().Format(time.RFC3339)
}

func (i dropInfo) json() map[string]any {
	return map[string]any{
		"service":    "xiaoju-cloud upload link",
		"about":      "Anyone with this link can upload files into one folder of a private cloud drive. Upload only: listing, downloading and deleting are not possible. Files with an existing name are renamed \"name (2)\", never overwritten.",
		"folder":     i.Folder,
		"label":      i.Label,
		"expires_at": i.ExpiresAt,
		"upload": map[string]string{
			"put":            "PUT " + i.Link + "/{filename}  (request body = raw file bytes)",
			"post_multipart": "POST " + i.Link + "  (multipart/form-data, any field name, several files allowed)",
			"post_raw":       "POST " + i.Link + "?name={filename}  (request body = raw file bytes)",
		},
		"response": `201 {"files":[{"name","size","mime","sha256"}]}`,
		"examples": []string{
			"curl -T report.pdf " + i.Link + "/",
			"curl -F file=@a.png -F file=@b.csv " + i.Link,
		},
	}
}

func (i dropInfo) text() string {
	return fmt.Sprintf(`xiaoju-cloud upload link (小桔云盘上传链接)

Uploads go into the folder %q. Upload only: you cannot list, download or delete.
Expires: %s

Upload one file (request body = raw bytes; curl appends the file name when the URL ends with "/"):
  curl -T report.pdf %s/

Upload several files at once (multipart/form-data, any field name):
  curl -F file=@a.png -F file=@b.csv %s

Raw body with the name in the query:
  curl --data-binary @notes.md "%s?name=notes.md"

Response: 201 {"files":[{"name","size","mime","sha256"}]}
A file whose name is already taken is renamed "name (2)", never overwritten.
JSON description: %s?format=json
`, i.Folder, i.Expires(), i.Link, i.Link, i.Link, i.Link)
}

// PUT /u/{token}/{name...}
func (s *Server) dropPut(w http.ResponseWriter, r *http.Request) {
	dropHeaders(w)
	d := s.dropFor(w, r)
	if d == nil {
		return
	}
	f, err := s.dropStore(r.Context(), d, r.PathValue("name"), r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		s.dropFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"files": []dropResult{f}})
}

// POST /u/{token}：multipart（一个或多个文件），或请求体就是文件、名字在 ?name= / X-File-Name。
func (s *Server) dropPost(w http.ResponseWriter, r *http.Request) {
	dropHeaders(w)
	d := s.dropFor(w, r)
	if d == nil {
		return
	}
	ctx := r.Context()
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = r.Header.Get("X-File-Name")
			if u, err := url.PathUnescape(name); err == nil {
				name = u
			}
		}
		f, err := s.dropStore(ctx, d, name, r.Header.Get("Content-Type"), r.Body)
		if err != nil {
			s.dropFail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"files": []dropResult{f}})
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart body")
		return
	}
	var out []dropResult
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		if part.FileName() == "" { // 普通表单字段，不是文件
			part.Close()
			continue
		}
		if len(out) >= maxDropParts {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files per request", maxDropParts))
			return
		}
		f, err := s.dropStore(ctx, d, part.FileName(), part.Header.Get("Content-Type"), part)
		part.Close()
		if err != nil {
			s.dropFail(w, err)
			return
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no file in the request", "code": "no_file"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"files": out})
}

type dropResult struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Mime   string `json:"mime"`
	SHA256 string `json:"sha256"`
}

// dropStore 把一个文件存进链接的文件夹：名字整理一下（没有就按时间起一个），同名的改名“名字 (2)”。
func (s *Server) dropStore(ctx context.Context, d *meta.DropLink, name, contentType string, body io.Reader) (dropResult, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	head, _ := br.Peek(64)
	cr := &countingReader{r: br}
	ext, sniffed := media.Sniff(head)
	name = cleanName(name)
	if name == "" {
		name = "upload " + time.Now().In(s.loc).Format("2006-01-02 150405") + "-" + randHex(2)
	}
	if filepath.Ext(name) == "" && ext != "" {
		name += ext
	}
	if len(name) > 200 { // 留出改名“ (2)”的余地
		e := filepath.Ext(name)
		stem := []rune(strings.TrimSuffix(name, e))
		for len(string(stem))+len(e) > 200 && len(stem) > 0 {
			stem = stem[:len(stem)-1]
		}
		name = string(stem) + e
	}
	mimeType, _, _ := mime.ParseMediaType(contentType)
	switch mimeType {
	case "", "application/octet-stream", "application/x-www-form-urlencoded": // curl 的默认值，不可信
		mimeType = sniffed
		if mimeType == "" {
			mimeType = media.ByExt(filepath.Ext(name))
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n = withCopySuffix(name, i)
		}
		f, err := s.svc.Upload(ctx, service.UploadRequest{UserID: s.userID, ParentID: &d.FolderID, Name: n, Mime: mimeType, Body: cr})
		// 重名在读请求体之前就会发现，可以换个名字重试
		if errors.Is(err, meta.ErrConflict) && cr.n == 0 && i < 100 {
			continue
		}
		if err != nil {
			return dropResult{}, err
		}
		if err := s.svc.Store().CountDropUpload(context.WithoutCancel(ctx), d.ID, f.Size); err != nil {
			s.log.Warn("drop: count upload", "err", err)
		}
		s.log.Info("drop link upload", "link", d.ID, "folder", d.Folder.Name, "name", f.Name, "size", f.Size)
		return dropResult{Name: f.Name, Size: f.Size, Mime: f.Mime, SHA256: hex.EncodeToString(f.SHA256)}, nil
	}
}

func (s *Server) dropFail(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrEmptyUpload) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty file", "code": "empty_file"})
		return
	}
	s.fail(w, err)
}

// countingReader 数读了多少字节（重名重试前确认请求体还没被读过）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var dropPageTmpl = template.Must(template.New("drop").Parse(dropPageHTML))

// 上传页面：选文件或拖进来，一个一个 PUT 到 /u/<token>/<名字>，显示进度。下面写着给 AI agent 的用法。
const dropPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="no-referrer">
<title>上传到小桔云盘</title>
<style>
:root { color-scheme: light dark; --bg:#0f1012; --card:#1a1b1f; --fg:#f2f2f3; --muted:#9a9ba1; --line:#2a2b31; --accent:#ff8a1f; --ok:#3ccf7a; --err:#ff5d5d; }
@media (prefers-color-scheme: light) { :root { --bg:#f6f6f7; --card:#fff; --fg:#17181a; --muted:#6b6c72; --line:#e3e3e7; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.55 -apple-system, BlinkMacSystemFont, "PingFang SC", "Segoe UI", sans-serif; }
main { max-width:640px; margin:0 auto; padding:32px 16px 48px; }
h1 { font-size:22px; margin:0 0 4px; }
.sub { color:var(--muted); margin:0 0 20px; }
#drop { display:block; border:2px dashed var(--line); border-radius:16px; padding:36px 16px; text-align:center; background:var(--card); cursor:pointer; transition:border-color .15s; }
#drop.over { border-color:var(--accent); }
#drop b { display:block; font-size:17px; margin-bottom:4px; }
#drop span { color:var(--muted); font-size:13px; }
input[type=file] { display:none; }
ul { list-style:none; padding:0; margin:16px 0 0; }
li { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:10px 12px; margin-bottom:8px; }
li .row { display:flex; justify-content:space-between; gap:12px; }
li .name { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
li .st { color:var(--muted); font-size:13px; white-space:nowrap; }
li.ok .st { color:var(--ok); } li.err .st { color:var(--err); }
li.ok .bar i { width:100% !important; background:var(--ok); }
.bar { height:3px; background:var(--line); border-radius:2px; margin-top:8px; overflow:hidden; }
.bar i { display:block; height:100%; width:0; background:var(--accent); transition:width .2s; }
details { margin-top:28px; background:var(--card); border:1px solid var(--line); border-radius:12px; padding:12px 14px; }
summary { cursor:pointer; font-weight:600; }
code { word-break:break-all; }
pre { overflow-x:auto; background:var(--bg); border:1px solid var(--line); border-radius:8px; padding:10px; font-size:12.5px; white-space:pre-wrap; word-break:break-all; }
</style>
</head>
<body>
<main>
<h1>上传到小桔云盘</h1>
<p class="sub">文件会放进“{{.Folder}}”{{if .Label}} · {{.Label}}{{end}}。这个链接只能上传，看不到网盘里的其他东西。{{if .ExpiresAt}}有效期到 {{.Expires}}。{{end}}</p>
<label id="drop"><input id="pick" type="file" multiple><b>点这里选文件，或拖进来</b><span>可以一次选多个；同名的会自动改名，不会覆盖</span></label>
<ul id="list"></ul>
<details open>
<summary>For AI agents / 给 AI agent</summary>
<p>This is an upload-only link to a private cloud drive. Send files with plain HTTP, no login needed:</p>
<pre>
# one file (curl appends the file name to a URL ending in "/")
curl -T report.pdf {{.Link}}/

# several files (multipart/form-data, any field name)
curl -F file=@a.png -F file=@b.csv {{.Link}}

# raw body, name in the query
curl --data-binary @notes.md "{{.Link}}?name=notes.md"
</pre>
<p>Response: <code>201 {"files":[{"name","size","mime","sha256"}]}</code>. A taken name becomes "name (2)". Machine-readable description: <code>{{.Link}}?format=json</code></p>
</details>
</main>
<script>
(function () {
  var base = location.pathname.replace(/\/+$/, "");
  var list = document.getElementById("list"), drop = document.getElementById("drop"), pick = document.getElementById("pick");
  var queue = [], busy = false;
  function size(n) { var u = ["B","KB","MB","GB"], i = 0; while (n >= 1024 && i < 3) { n /= 1024; i++; } return (i ? n.toFixed(1) : n) + " " + u[i]; }
  function add(files) {
    for (var i = 0; i < files.length; i++) {
      var f = files[i], li = document.createElement("li");
      li.innerHTML = '<div class="row"><span class="name"></span><span class="st">等待中</span></div><div class="bar"><i></i></div>';
      li.querySelector(".name").textContent = f.name + " · " + size(f.size);
      list.appendChild(li);
      queue.push({ file: f, li: li });
    }
    next();
  }
  function next() {
    if (busy || !queue.length) return;
    busy = true;
    var job = queue.shift(), st = job.li.querySelector(".st"), bar = job.li.querySelector(".bar i");
    var xhr = new XMLHttpRequest();
    xhr.open("PUT", base + "/" + encodeURIComponent(job.file.name));
    if (job.file.type) xhr.setRequestHeader("Content-Type", job.file.type);
    xhr.upload.onprogress = function (e) {
      if (!e.lengthComputable) return;
      var p = Math.round(e.loaded / e.total * 100);
      bar.style.width = p + "%";
      st.textContent = p < 100 ? p + "%" : "正在保存…";
    };
    xhr.onload = function () {
      var res = {}; try { res = JSON.parse(xhr.responseText); } catch (e) {}
      if (xhr.status === 201) {
        job.li.className = "ok"; bar.style.width = "100%";
        var name = res.files && res.files[0] && res.files[0].name;
        st.textContent = name && name !== job.file.name ? "已上传（存为 " + name + "）" : "已上传";
      } else {
        job.li.className = "err"; st.textContent = "失败：" + (res.error || xhr.status);
      }
      busy = false; next();
    };
    xhr.onerror = function () { job.li.className = "err"; st.textContent = "网络出错"; busy = false; next(); };
    xhr.send(job.file);
  }
  pick.addEventListener("change", function () { add(pick.files); pick.value = ""; });
  ["dragenter","dragover"].forEach(function (t) { drop.addEventListener(t, function (e) { e.preventDefault(); drop.classList.add("over"); }); });
  ["dragleave","drop"].forEach(function (t) { drop.addEventListener(t, function (e) { e.preventDefault(); drop.classList.remove("over"); }); });
  drop.addEventListener("drop", function (e) { if (e.dataTransfer && e.dataTransfer.files.length) add(e.dataTransfer.files); });
  window.addEventListener("beforeunload", function (e) { if (busy || queue.length) { e.preventDefault(); e.returnValue = ""; } });
})();
</script>
</body>
</html>
`
