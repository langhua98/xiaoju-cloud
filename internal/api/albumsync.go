package api

import (
	"bufio"
	"context"
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

// 快捷指令“分批续传”相册同步。快捷指令一次查整个相册会超出内存限制，所以每次运行只处理一批：
//
//	GET  /api/album/start?limit=100   开始一轮，返回进度日期（纯文本，如 "2026-10-10 19:54"）；
//	                                  快捷指令查找创建日期在它之后的照片，从旧到新、限制 limit 张
//	POST /api/album/check             表单 name、date → "exists" / "missing"
//	POST /api/album/upload            multipart 表单 name、date、file → "uploaded" / "exists"
//	GET  /api/album/finish            返回本轮汇总（纯文本，快捷指令直接显示）
//
// 照片名称和创建日期放在请求体表单里传（不拼进网址，避免快捷指令把隐形字符带进网址），由服务端
// 整理成“拍摄时间_照片名”作为去重 key。照片按拍摄时间从旧到新处理，备份好的部分记为进度存进
// 数据库，下次运行从进度处接着传；某张上传失败时进度停在它前面，下次运行会重试。
// start 加 reset=1 清除进度，从头检查一遍（已传过的只检查不重传）。

// albumRun 是当前这一轮同步的状态（单用户，放内存即可；服务重启后清零，进度在数据库里）。
type albumRun struct {
	mu sync.Mutex
	albumStats
}

type albumStats struct {
	limit, seen, uploaded, existed, failed int
	started                                time.Time

	last     string // 本轮最近一张照片的拍摄时间（yyyyMMdd_HHmmss）
	lastDone bool   // 最近一张是否已经备份好
	waiting  string // check 返回 missing、正在等上传的那张的 key
	hold     bool   // 本轮进度不再前进（有照片失败、顺序不对等）
	warn     string // 进度不再前进的原因（失败时为空，汇总里另有失败张数）
}

// photo 记录轮到一张新照片（check，或没有先 check 的 upload）。照片应按拍摄时间从旧到新到来：
// 上一张已经备份好、且这一张不比它早，才把上一张的拍摄时间存为进度。进度慢一张保存，是为了在
// 排序选错（最新的排最前）时不会第一张就把进度跳到最新、漏掉更早的照片。
func (a *albumStats) photo(date string) (save string) {
	a.seen++
	if a.waiting != "" {
		a.stop("有照片检查后没上传，请检查“如果”那一步")
	}
	a.waiting = ""
	stamp, ok := cursorStamp(date)
	switch {
	case a.hold:
	case !ok:
		a.stop(fmt.Sprintf("认不出照片的创建日期 %q", date))
	case stamp < a.last:
		a.stop("“查找照片”的排序要选“最旧的排最前”")
	case a.lastDone:
		save = a.last
	}
	a.last, a.lastDone = stamp, false
	return save
}

func (a *albumStats) stop(warn string) {
	if !a.hold {
		a.hold, a.warn = true, warn
	}
}

// albumTrack 在锁内更新本轮状态，需要时把进度存进数据库。
func (s *Server) albumTrack(ctx context.Context, update func(a *albumStats) (save string)) {
	s.albumRun.mu.Lock()
	save := update(&s.albumRun.albumStats)
	s.albumRun.mu.Unlock()
	if save != "" {
		if err := s.svc.Store().SetAlbumCursor(ctx, s.userID, save); err != nil {
			s.log.Error("album cursor", "err", err)
		}
	}
}

func (s *Server) albumStart(w http.ResponseWriter, r *http.Request) {
	ctx, q, store := r.Context(), albumQuery(r), s.svc.Store()
	if q.Get("reset") == "1" {
		if err := store.SetAlbumCursor(ctx, s.userID, ""); err != nil {
			s.fail(w, err)
			return
		}
	}
	cursor, err := store.AlbumCursor(ctx, s.userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(nonDigit.ReplaceAllString(q.Get("limit"), ""))
	s.albumRun.mu.Lock()
	s.albumRun.albumStats = albumStats{limit: limit, started: time.Now()}
	s.albumRun.mu.Unlock()
	since := cursorDate(cursor)
	s.log.Info("album sync start", "limit", limit, "cursor", cursor, "since", since)
	writeText(w, http.StatusOK, since)
}

func (s *Server) albumFinish(w http.ResponseWriter, r *http.Request) {
	ctx, store := r.Context(), s.svc.Store()
	s.albumRun.mu.Lock()
	a := s.albumRun.albumStats
	s.albumRun.mu.Unlock()
	if !a.hold && a.lastDone {
		if err := store.SetAlbumCursor(ctx, s.userID, a.last); err != nil {
			s.log.Error("album cursor", "err", err)
		}
	}
	cursor, err := store.AlbumCursor(ctx, s.userID)
	if err != nil {
		s.log.Error("album cursor", "err", err)
	}
	files := -1
	if album, err := store.FindFolder(ctx, s.userID, nil, albumFolder); err == nil {
		files, _ = store.CountFiles(ctx, s.userID, album.ID)
	}
	s.log.Info("album sync finish", "seen", a.seen, "uploaded", a.uploaded, "existed", a.existed,
		"failed", a.failed, "warn", a.warn, "cursor", cursor)
	writeText(w, http.StatusOK, albumReport(a, cursor, files))
}

// albumReport 生成本轮汇总，files 为网盘“相册”里的文件数（未知时为 -1）。
func albumReport(a albumStats, cursor string, files int) string {
	var b strings.Builder
	b.WriteString("相册同步结束")
	if !a.started.IsZero() {
		fmt.Fprintf(&b, "（用时 %s）", durationText(time.Since(a.started)))
	}
	if a.seen == 0 {
		b.WriteString("\n没有新照片")
	} else {
		fmt.Fprintf(&b, "\n本次检查 %d 张\n✅ 新上传 %d 张\n☑️ 之前已备份 %d 张", a.seen, a.uploaded, a.existed)
	}
	if a.failed > 0 {
		fmt.Fprintf(&b, "\n❌ 上传失败 %d 张", a.failed)
	}
	if pending := a.seen - a.uploaded - a.existed - a.failed; pending > 0 {
		fmt.Fprintf(&b, "\n⏸ 检查了还没上传 %d 张", pending)
	}
	if t, err := time.Parse(stampLayout, cursor); err == nil {
		fmt.Fprintf(&b, "\n📍 已备份到 %s 拍的照片", t.Format("2006年1月2日 15:04"))
	}
	if files >= 0 {
		fmt.Fprintf(&b, "\n☁️ 网盘“相册”里共 %d 个文件", files)
	}
	switch {
	case a.warn != "":
		fmt.Fprintf(&b, "\n⚠️ 进度没有前进：%s", a.warn)
	case a.hold:
		b.WriteString("\n🔁 再运行一次，会从没传好的那张接着传")
	case a.limit > 0 && a.seen >= a.limit:
		b.WriteString("\n⏩ 后面还有照片，再运行一次接着传")
	case a.limit == 0 && a.seen > 0:
		b.WriteString("\n⏩ 再运行一次，直到显示“全部备份完成”")
	default:
		b.WriteString("\n🎉 全部备份完成")
	}
	return b.String()
}

func durationText(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
}

// albumCheckAny 处理带表单的 check：key 由 name、date 生成；没有表单时退回网址里的 key。
func (s *Server) albumCheckAny(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !isForm(r) {
		s.albumCheck(w, r)
		return
	}
	if err := parseForm(r); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}
	ctx := r.Context()
	f := normalizedForm(r)
	key := formKey(f["name"], f["date"], "")
	bad := validName(key) != nil
	var exists bool
	var err error
	if bad {
		s.log.Warn("album check: invalid form", "fields", fmt.Sprint(r.PostForm))
	} else {
		exists, err = s.albumHas(ctx, key)
	}
	s.albumTrack(ctx, func(a *albumStats) string {
		save := a.photo(f["date"])
		switch {
		case bad || err != nil:
			a.failed++
			a.stop("")
		case exists:
			a.existed++
			a.lastDone = true
		default:
			a.waiting = key
		}
		return save
	})
	switch {
	case bad:
		writeError(w, http.StatusBadRequest, "invalid name/date")
	case err != nil:
		s.fail(w, err)
	case exists:
		s.log.Info("album check", "key", key, "exists", true)
		writeText(w, http.StatusOK, "exists")
	default:
		s.log.Info("album check", "key", key, "exists", false)
		writeText(w, http.StatusOK, "missing")
	}
}

// albumUploadAny：multipart 表单走新流程，其余（请求体为原始文件、?key=）走原来的 albumUpload。
func (s *Server) albumUploadAny(w http.ResponseWriter, r *http.Request) {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "multipart/form-data" {
		s.albumUpload(w, r)
		return
	}
	ctx := r.Context()
	p, err := readAlbumPhoto(r)
	defer p.close()
	key := p.key()
	status, msg := http.StatusBadRequest, "invalid form"
	if err == nil {
		status, msg, err = s.albumSave(ctx, p, key)
	}
	s.albumTrack(ctx, func(a *albumStats) string {
		var save string
		if a.waiting != key {
			save = a.photo(p.date) // 没有先 check 的照片
		}
		a.waiting = ""
		switch {
		case err != nil:
			a.failed++
			a.stop("")
		case msg == "exists":
			a.existed++
			a.lastDone = true
		default:
			a.uploaded++
			a.lastDone = true
		}
		return save
	})
	if err != nil {
		s.log.Error("album upload failed", "key", key, "err", err)
		writeText(w, status, "failed: "+msg)
		return
	}
	writeText(w, status, msg)
}

// albumPhoto 是 upload 表单里的一张照片。文件先落到临时文件：字段顺序不确定，要等 name、date
// 都读到才能确定文件名。
type albumPhoto struct {
	name, date, filename, ctype string
	file                        *os.File
}

func (p *albumPhoto) key() string { return formKey(p.name, p.date, p.filename) }

func (p *albumPhoto) close() {
	if p.file != nil {
		p.file.Close()
		os.Remove(p.file.Name())
	}
}

func readAlbumPhoto(r *http.Request) (*albumPhoto, error) {
	p := &albumPhoto{}
	mr, err := r.MultipartReader()
	if err != nil {
		return p, err
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
		field := fieldName(part.FormName())
		switch {
		case part.FileName() != "" || field == "file":
			if p.file != nil {
				break // 只取第一个文件
			}
			p.filename, p.ctype = part.FileName(), part.Header.Get("Content-Type")
			if p.file, err = os.CreateTemp("", "xiaoju-album-*"); err != nil {
				return p, err
			}
			if _, err := io.Copy(p.file, part); err != nil {
				return p, err
			}
		case field == "name":
			p.name = readField(part)
		case field == "date":
			p.date = readField(part)
		}
		part.Close()
	}
	if p.file == nil {
		return p, errors.New("no file part")
	}
	return p, nil
}

// albumSave 去重后把照片加密上传到“相册”，文件名为 key + 原扩展名。
func (s *Server) albumSave(ctx context.Context, p *albumPhoto, key string) (status int, msg string, err error) {
	if validName(key) != nil {
		return http.StatusBadRequest, "invalid name/date", fmt.Errorf("invalid key from name=%q date=%q file=%q", p.name, p.date, p.filename)
	}
	album, err := s.albumDir(ctx)
	if err != nil {
		return http.StatusInternalServerError, "album folder", err
	}
	if exists, err := s.svc.Store().StemExists(ctx, s.userID, album.ID, key); err != nil {
		return http.StatusInternalServerError, "check", err
	} else if exists {
		return http.StatusOK, "exists", nil
	}

	if _, err := p.file.Seek(0, io.SeekStart); err != nil {
		return http.StatusInternalServerError, "temp file", err
	}
	body := bufio.NewReaderSize(p.file, 512)
	head, _ := body.Peek(512)
	ctype, ext := mediaType(p.ctype, head)
	if e := strings.ToLower(path.Ext(p.filename)); e != "" && len(e) <= 6 {
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

const stampLayout = "20060102_150405"

var stampMinute = regexp.MustCompile(`^\d{8}_\d{4}$`)

// cursorStamp 把照片创建日期的文字整理成可以按字符串比较先后的拍摄时间 yyyyMMdd_HHmmss。
func cursorStamp(date string) (string, bool) {
	st := dateStamp(date)
	if stampMinute.MatchString(st) {
		st += "00"
	}
	if _, err := time.Parse(stampLayout, st); err != nil {
		return "", false
	}
	return st, true
}

// cursorDate 是 start 返回给快捷指令的日期：在进度上往前留 1 分钟余量（创建日期的文字可能只精确
// 到分钟，同一分钟里的照片重新检查一遍，已传过的会跳过）。没有进度时从很早的日期开始。
func cursorDate(stamp string) string {
	t, err := time.Parse(stampLayout, stamp)
	if err != nil {
		return "1971-01-01 00:00"
	}
	return t.Add(-time.Minute).Format("2006-01-02 15:04")
}
