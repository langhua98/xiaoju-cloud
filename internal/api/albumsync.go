package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
	"github.com/langhua98/xiaoju-cloud/internal/service"
)

// 快捷指令“清点 → 续传 → 报数”同步。照片名称和创建日期放在请求体表单里传（不拼进网址，
// 避免快捷指令把隐形字符带进网址），由服务端整理成“拍摄时间_照片名”作为去重 key：
//
//	GET  /api/album/start?total=N   开始一轮，记下总数并清零计数
//	POST /api/album/check           表单 name、date → "exists" / "missing"
//	POST /api/album/upload          multipart 表单 name、date、file → "uploaded" / "exists"
//	GET  /api/album/finish          返回本轮汇总（纯文本，快捷指令直接显示）
//
// 已经传过的照片在 check 时就被跳过，中途中断后重新运行会从没传的那张接着传。

// albumRun 是当前这一轮同步的计数（单用户，放内存即可；服务重启后清零）。
type albumRun struct {
	mu                               sync.Mutex
	total, uploaded, existed, failed int
	started                          time.Time
}

func (a *albumRun) add(field *int) {
	a.mu.Lock()
	*field++
	a.mu.Unlock()
}

func (s *Server) albumStart(w http.ResponseWriter, r *http.Request) {
	total, _ := strconv.Atoi(nonDigit.ReplaceAllString(albumQuery(r).Get("total"), ""))
	a := &s.albumRun
	a.mu.Lock()
	a.total, a.uploaded, a.existed, a.failed, a.started = total, 0, 0, 0, time.Now()
	a.mu.Unlock()
	s.log.Info("album sync start", "total", total)
	writeText(w, http.StatusOK, fmt.Sprintf("开始同步：共 %d 张", total))
}

func (s *Server) albumFinish(w http.ResponseWriter, r *http.Request) {
	a := &s.albumRun
	a.mu.Lock()
	total, up, ex, fail, started := a.total, a.uploaded, a.existed, a.failed, a.started
	a.mu.Unlock()

	var b strings.Builder
	if started.IsZero() {
		b.WriteString("相册同步")
	} else {
		fmt.Fprintf(&b, "相册同步结束（用时 %s）", time.Since(started).Round(time.Second))
	}
	if total > 0 {
		fmt.Fprintf(&b, "\n共 %d 张", total)
	}
	fmt.Fprintf(&b, "\n✅ 本次新上传 %d 张\n☑️ 之前已备份 %d 张", up, ex)
	if fail > 0 {
		fmt.Fprintf(&b, "\n❌ 上传失败 %d 张（再运行一次会自动重试）", fail)
	}
	pending := total - up - ex - fail
	if pending > 0 {
		fmt.Fprintf(&b, "\n⏸ 还没处理 %d 张（再运行一次会接着传）", pending)
	}
	if total > 0 && fail == 0 && pending <= 0 {
		b.WriteString("\n🎉 全部备份完成")
	}
	s.log.Info("album sync finish", "total", total, "uploaded", up, "existed", ex, "failed", fail)
	writeText(w, http.StatusOK, b.String())
}

// albumCheckAny 处理带表单的 check：key 由 name、date 生成；没有表单时退回网址里的 key。
func (s *Server) albumCheckAny(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isForm(r) {
		if err := parseForm(r); err != nil {
			writeError(w, http.StatusBadRequest, "invalid form")
			return
		}
		f := normalizedForm(r)
		key := formKey(f["name"], f["date"], "")
		if validName(key) != nil {
			s.log.Warn("album check: invalid form", "fields", fmt.Sprint(r.PostForm))
			writeError(w, http.StatusBadRequest, "invalid name/date")
			return
		}
		exists, err := s.albumHas(r.Context(), key)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.log.Info("album check", "key", key, "exists", exists)
		if exists {
			s.albumRun.add(&s.albumRun.existed)
			writeText(w, http.StatusOK, "exists")
		} else {
			writeText(w, http.StatusOK, "missing")
		}
		return
	}
	s.albumCheck(w, r)
}

// albumUploadAny：multipart 表单走新流程，其余（请求体为原始文件、?key=）走原来的 albumUpload。
func (s *Server) albumUploadAny(w http.ResponseWriter, r *http.Request) {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "multipart/form-data" {
		s.albumUpload(w, r)
		return
	}
	status, msg, err := s.albumUploadForm(r)
	switch {
	case err != nil:
		s.albumRun.add(&s.albumRun.failed)
		s.log.Error("album upload failed", "err", err)
		writeText(w, status, "failed: "+msg)
	case msg == "exists":
		s.albumRun.add(&s.albumRun.existed)
		writeText(w, http.StatusOK, msg)
	default:
		s.albumRun.add(&s.albumRun.uploaded)
		writeText(w, http.StatusCreated, msg)
	}
}

// albumUploadForm 读取 multipart 表单：文件先落到临时文件（字段顺序不确定，要等 name、date
// 都读到才能确定文件名），去重后再加密上传。
func (s *Server) albumUploadForm(r *http.Request) (status int, msg string, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return http.StatusBadRequest, "invalid form", err
	}
	var name, date, filename, partType string
	var tmp *os.File
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return http.StatusBadRequest, "invalid form", err
		}
		field := fieldName(p.FormName())
		switch {
		case p.FileName() != "" || field == "file":
			if tmp != nil {
				p.Close()
				continue // 只取第一个文件
			}
			filename, partType = p.FileName(), p.Header.Get("Content-Type")
			if tmp, err = os.CreateTemp("", "xiaoju-album-*"); err != nil {
				return http.StatusInternalServerError, "temp file", err
			}
			if _, err := io.Copy(tmp, p); err != nil {
				return http.StatusBadRequest, "read file", err
			}
		case field == "name":
			name = readField(p)
		case field == "date":
			date = readField(p)
		}
		p.Close()
	}
	if tmp == nil {
		return http.StatusBadRequest, "missing file", errors.New("no file part")
	}
	key := formKey(name, date, filename)
	if validName(key) != nil {
		return http.StatusBadRequest, "invalid name/date", fmt.Errorf("invalid key from name=%q date=%q file=%q", name, date, filename)
	}

	ctx := r.Context()
	album, err := s.albumDir(ctx)
	if err != nil {
		return http.StatusInternalServerError, "album folder", err
	}
	if exists, err := s.svc.Store().StemExists(ctx, s.userID, album.ID, key); err != nil {
		return http.StatusInternalServerError, "check", err
	} else if exists {
		return http.StatusOK, "exists", nil
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return http.StatusInternalServerError, "temp file", err
	}
	body := bufio.NewReaderSize(tmp, 512)
	head, _ := body.Peek(512)
	ctype, ext := mediaType(partType, head)
	if e := strings.ToLower(path.Ext(filename)); e != "" && len(e) <= 6 {
		ext = e // 保留原文件的扩展名（如 .heic、.mov）
	}
	f, err := s.svc.Upload(ctx, service.UploadRequest{
		UserID: s.userID, ParentID: &album.ID, Name: key + ext, Mime: ctype, Body: body,
	})
	if errors.Is(err, meta.ErrConflict) {
		return http.StatusOK, "exists", nil
	}
	if err != nil {
		return http.StatusInternalServerError, "upload", err
	}
	s.log.Info("album upload", "name", f.Name, "mime", f.Mime, "size", f.Size)
	return http.StatusCreated, "uploaded", nil
}

// fieldName 规范化表单字段名：手机上手打的字段名可能带空格、大写或全角字符。
func fieldName(s string) string {
	return strings.ToLower(nonLetter.ReplaceAllString(norm.NFKC.String(s), ""))
}

// normalizedForm 返回按 fieldName 规范化字段名后的表单值。
func normalizedForm(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			out[fieldName(k)] = v[0]
		}
	}
	return out
}

func readField(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 1024))
	return string(b)
}

func isForm(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "multipart/form-data" || ct == "application/x-www-form-urlencoded"
}

func parseForm(r *http.Request) error {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "multipart/form-data" {
		return r.ParseMultipartForm(1 << 20)
	}
	return r.ParseForm()
}

var (
	digitRun  = regexp.MustCompile(`\d+`)
	nonDigit  = regexp.MustCompile(`\D`)
	nonLetter = regexp.MustCompile(`[^A-Za-z]`)
	pmMarker  = regexp.MustCompile(`(?i)下午|晚上|傍晚|\bpm\b|p\.m\.`)
	amMarker  = regexp.MustCompile(`(?i)上午|凌晨|早上|\bam\b|a\.m\.`)
)

// formKey 由照片名称和创建日期生成去重 key（“拍摄时间_照片名”）。名称为空时用上传文件名。
func formKey(name, date, filename string) string {
	if strings.TrimSpace(name) == "" {
		name = filename
	}
	name = strings.TrimSpace(name)
	if ext := path.Ext(name); ext != "" && len(ext) <= 6 {
		name = strings.TrimSuffix(name, ext)
	}
	name = strings.Trim(nonKeyRun.ReplaceAllString(name, "_"), "_")
	stamp := dateStamp(date)
	switch {
	case stamp == "":
		return name
	case name == "":
		return stamp
	default:
		return stamp + "_" + name
	}
}

// dateStamp 把快捷指令里日期变量转成的文字（随系统语言而变，如“2026年10月10日 下午7:55”
// “2026/10/10 19:55:12”）整理成 yyyyMMdd_HHmm[ss]。认不出年月日顺序时退回成所有数字连在一起，
// 只要同一台手机的设置不变，结果就稳定，足够用来去重。
func dateStamp(s string) string {
	g := digitRun.FindAllString(s, -1)
	if len(g) == 0 {
		return ""
	}
	if len(g) >= 5 && len(g[0]) == 4 {
		n := make([]int, len(g))
		for i, v := range g {
			n[i], _ = strconv.Atoi(v)
		}
		h := n[3]
		if pmMarker.MatchString(s) && h < 12 {
			h += 12
		} else if amMarker.MatchString(s) && h == 12 {
			h = 0
		}
		stamp := fmt.Sprintf("%04d%02d%02d_%02d%02d", n[0], n[1], n[2], h, n[4])
		if len(n) >= 6 && len(g[5]) == 2 {
			stamp += fmt.Sprintf("%02d", n[5])
		}
		return stamp
	}
	return strings.Join(g, "")
}
