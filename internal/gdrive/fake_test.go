package gdrive

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeDrive 是测试用的假谷歌云盘：只实现同步用到的接口。
type fakeDrive struct {
	mu      sync.Mutex
	files   map[string]*fakeFile
	next    int
	changes int // 每次改动加一，changes 接口用
	uploads map[string]func(body []byte) *fakeFile
	srv     *httptest.Server
	noSHA   bool // 模拟谷歌没给 sha256Checksum
}

type fakeFile struct {
	File
	data []byte
}

func newFakeDrive() *fakeDrive {
	d := &fakeDrive{files: map[string]*fakeFile{}, uploads: map[string]func([]byte) *fakeFile{}}
	d.files["root"] = &fakeFile{File: File{ID: "root", Name: "My Drive", MimeType: FolderMime}}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	return d
}

func (d *fakeDrive) endpoints() Endpoints {
	return Endpoints{Auth: d.srv.URL + "/auth", Token: d.srv.URL + "/token", API: d.srv.URL + "/drive/v3", Upload: d.srv.URL + "/upload/drive/v3"}
}

func (d *fakeDrive) newID() string {
	d.next++
	return fmt.Sprintf("g%d", d.next)
}

func (d *fakeDrive) setData(f *fakeFile, data []byte) {
	f.data = data
	m := md5.Sum(data)
	s := sha256.Sum256(data)
	f.MD5 = hex.EncodeToString(m[:])
	f.SHA256 = hex.EncodeToString(s[:])
	if d.noSHA {
		f.SHA256 = ""
	}
	f.Size = strconv.Itoa(len(data))
	f.Modified = time.Now()
}

// 测试里直接改假云盘（模拟用户在谷歌云盘里操作）
func (d *fakeDrive) mkdir(parent, name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.newID()
	d.files[id] = &fakeFile{File: File{ID: id, Name: name, MimeType: FolderMime, Parents: []string{parent}}}
	d.changes++
	return id
}

func (d *fakeDrive) put(parent, name, data string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.newID()
	f := &fakeFile{File: File{ID: id, Name: name, MimeType: "text/plain", Parents: []string{parent}}}
	d.setData(f, []byte(data))
	d.files[id] = f
	d.changes++
	return id
}

func (d *fakeDrive) edit(id string, fn func(f *fakeFile)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.files[id])
	d.files[id].Modified = time.Now()
	d.changes++
}

func (d *fakeDrive) write(id, data string) {
	d.edit(id, func(f *fakeFile) { d.setData(f, []byte(data)) })
}

// find 按路径找（"a/b.txt"，从 root 起），只看不在回收站里的
func (d *fakeDrive) find(root, path string) *fakeFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := root
	var f *fakeFile
	for _, seg := range strings.Split(path, "/") {
		f = nil
		for _, c := range d.files {
			if len(c.Parents) > 0 && c.Parents[0] == cur && c.Name == seg && !c.Trashed && !d.inTrash(c) {
				f = c
				break
			}
		}
		if f == nil {
			return nil
		}
		cur = f.ID
	}
	return f
}

func (d *fakeDrive) inTrash(f *fakeFile) bool {
	for f != nil {
		if f.Trashed {
			return true
		}
		if len(f.Parents) == 0 {
			return false
		}
		f = d.files[f.Parents[0]]
	}
	return false
}

// names 列出文件夹里（不在回收站的）名字
func (d *fakeDrive) names(parent string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.files {
		if len(c.Parents) > 0 && c.Parents[0] == parent && !c.Trashed {
			out = append(out, c.Name)
		}
	}
	return out
}

var qParent = regexp.MustCompile(`^'([^']+)' in parents and trashed = false( and mimeType = '([^']+)')?$`)

func (d *fakeDrive) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	p := r.URL.Path
	if p == "/token" {
		r.ParseForm()
		if r.Form.Get("refresh_token") == "revoked" {
			w.WriteHeader(400)
			writeJSON(map[string]string{"error": "invalid_grant"})
			return
		}
		writeJSON(map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		w.WriteHeader(401)
		return
	}
	notFound := func() {
		w.WriteHeader(404)
		writeJSON(map[string]any{"error": map[string]string{"message": "File not found"}})
	}
	switch {
	case p == "/drive/v3/about":
		writeJSON(map[string]any{"user": map[string]string{"emailAddress": "me@example.com"}})
	case p == "/drive/v3/changes/startPageToken":
		writeJSON(map[string]string{"startPageToken": strconv.Itoa(d.changes)})
	case p == "/drive/v3/changes":
		from, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		var ch []map[string]string
		for i := from; i < d.changes; i++ {
			ch = append(ch, map[string]string{"fileId": "x"})
		}
		writeJSON(map[string]any{"newStartPageToken": strconv.Itoa(d.changes), "changes": ch})
	case p == "/drive/v3/files" && r.Method == http.MethodGet:
		m := qParent.FindStringSubmatch(r.URL.Query().Get("q"))
		if m == nil {
			w.WriteHeader(400)
			return
		}
		out := []File{}
		for _, f := range d.files {
			if len(f.Parents) > 0 && f.Parents[0] == m[1] && !f.Trashed && (m[3] == "" || f.MimeType == m[3]) {
				out = append(out, f.File)
			}
		}
		writeJSON(map[string]any{"files": out})
	case p == "/drive/v3/files" && r.Method == http.MethodPost:
		var body struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := d.newID()
		f := &fakeFile{File: File{ID: id, Name: body.Name, MimeType: body.MimeType, Parents: body.Parents}}
		d.files[id] = f
		d.changes++
		writeJSON(f.File)
	case strings.HasPrefix(p, "/drive/v3/files/"):
		id := strings.TrimPrefix(p, "/drive/v3/files/")
		f := d.files[id]
		if f == nil {
			notFound()
			return
		}
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("alt") == "media" {
				w.Write(f.data)
				return
			}
			writeJSON(f.File)
		case http.MethodPatch:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if n, ok := body["name"].(string); ok {
				f.Name = n
			}
			if t, ok := body["trashed"].(bool); ok {
				f.Trashed = t
			}
			if add := r.URL.Query().Get("addParents"); add != "" {
				f.Parents = []string{add}
			}
			d.changes++
			writeJSON(f.File)
		}
	case strings.HasPrefix(p, "/upload/drive/v3/files"):
		var body struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mime := r.Header.Get("X-Upload-Content-Type")
		id := strings.TrimPrefix(strings.TrimPrefix(p, "/upload/drive/v3/files"), "/")
		key := d.newID()
		d.uploads[key] = func(data []byte) *fakeFile {
			f := d.files[id]
			if f == nil {
				f = &fakeFile{File: File{ID: d.newID(), MimeType: mime, Parents: body.Parents}}
				d.files[f.ID] = f
			}
			if body.Name != "" {
				f.Name = body.Name
			}
			d.setData(f, data)
			d.changes++
			return f
		}
		w.Header().Set("Location", d.srv.URL+"/session/"+key)
	case strings.HasPrefix(p, "/session/"):
		fn := d.uploads[strings.TrimPrefix(p, "/session/")]
		if fn == nil {
			notFound()
			return
		}
		data, _ := io.ReadAll(r.Body)
		if int64(len(data)) != r.ContentLength {
			w.WriteHeader(400)
			return
		}
		writeJSON(fn(data).File)
	default:
		notFound()
	}
}
