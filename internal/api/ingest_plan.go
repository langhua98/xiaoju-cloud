package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/langhua98/xiaoju-cloud/internal/media"
	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// POST /api/ingest/plan：快捷指令开始时把所有文件的名字（不带扩展名）和大小（或拍摄日期）各一行一次发来，
// 服务端回答哪些还没传过（第几个，从 1 开始）。手机只处理要传的文件，跳过上千个传过的只要几秒；
// 以前在手机上一个个比对，快捷指令每个文件要执行三十多个动作，跳过一个就要 3–5 秒。
//
//	从「文件」里的文件夹传：{"album": "hidden", "names": "IMG_0001\nIMG_0002", "sizes": "2.1 MB\n30.4 MB"}
//	从相册直接读：          {"album": "", "names": "IMG_0001\nIMG_0002", "dates": "2026年10月18日 15:04\n…"}
//	回答：{"todo": "2\n5", "count": 2, "uploaded": 120, "files": 2}
//
// 名字去掉自动加的序号、扩展名后不分大小写比较。同名的不一定是同一个（换过手机后编号从 IMG_0001 重新开始），
// 所以还要比：
//   - 大小：快捷指令「文件大小」变成的文字（“30.4 MB”），按显示的精度比（同名的实况照片和视频也靠大小区分）；
//   - 拍摄日期：相册里的照片取不到可靠的大小（上传时视频要重新导出，大小会变），改比「创建日期」是哪一天，
//     和网盘里那个文件的拍摄时间（生成缩略图时读出来的）前后差一天以内就算同一个（手机和网盘的时区可能不一样）。
//
// 看不懂的大小、日期，以及网盘里不知道拍摄时间的文件，只比名字；日志里记个数。
// files 是收到了几行名字：快捷指令拿它和照片张数比，对不上（有的照片没取到名字）就不能按第几个去传。
// 和 exists 一样只透露“这个名字、差不多这个大小（这一天拍）的文件在不在”，不能列出相册里有什么。
func (s *Server) ingestPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Album string `json:"album"`
		Names string `json:"names"`
		Sizes string `json:"sizes"`
		Dates string `json:"dates"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "plan request must be JSON with names and sizes")
		return
	}
	folder, ok := ingestAlbums[strings.ToLower(strings.TrimSpace(req.Album))]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown album")
		return
	}
	names, sizes, dates := splitLines(req.Names), splitLines(req.Sizes), splitLines(req.Dates)
	if sizes != nil && len(names) != len(sizes) {
		writeError(w, http.StatusBadRequest, "names and sizes must have the same number of lines")
		return
	}
	if dates != nil && len(names) != len(dates) {
		// 有的照片没有创建日期，列表里就少了一行，对不齐：不比日期，只比名字
		s.log.Warn("ingest plan: dates ignored, line count differs from names", "names", len(names), "dates", len(dates))
		dates = nil
	}

	// stored：比较用的名字 → 传过的文件；loose：再去掉空白、控制字符和“_”的名字 → 传过的文件（兜底，见 looseKey）
	stored, loose := map[string][]meta.NameSize{}, map[string][]meta.NameSize{}
	album, err := s.svc.Store().FindFolder(r.Context(), s.userID, nil, folder)
	switch {
	case errors.Is(err, meta.ErrNotFound): // 还没传过任何文件
	case err != nil:
		s.fail(w, err)
		return
	default:
		files, err := s.svc.Store().ListNameSizes(r.Context(), s.userID, album.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, f := range files {
			k := planKey(f.Name, true)
			stored[k] = append(stored[k], f)
			loose[looseKey(k)] = append(loose[looseKey(k)], f)
		}
	}

	matched, keys := make([]bool, len(names)), make([]string, len(names))
	var dateMismatch []int // 名字（和大小）对得上、拍摄日期差得远的：多半是同名的另一张照片
	nameMatched, dateAgreed, unparsedSizes, unparsedDates := 0, 0, 0, 0
	for i, name := range names {
		var c planCheck
		if sizes != nil {
			c.size = sizes[i]
			if _, parsed := sizeMatchesAny(c.size, nil); !parsed {
				c.size = "" // 大小看不懂：只比名字
				unparsedSizes++
			}
		}
		if dates != nil {
			if c.days = photoDays(dates[i]); c.days == nil {
				unparsedDates++
			}
		}
		key := planKey(name, isMediaExt(filepath.Ext(name))) // 名字万一带着照片、视频的扩展名（IMG_0001.HEIC），也去掉
		keys[i] = key
		byName, agreed := false, false
		for _, f := range stored[key] {
			if !c.sizeOK(f) {
				continue
			}
			byName = true
			if ok, sure := c.dateOK(f, s.loc); ok {
				matched[i], agreed = true, sure
				break
			}
		}
		if byName {
			nameMatched++
			if agreed {
				dateAgreed++
			}
			if !matched[i] {
				dateMismatch = append(dateMismatch, i)
			}
		}
		if !matched[i] {
			// 兜底：名字里有制表符这类看不见的字符时，上传时被换成了“_”，这里两边各去掉再比（大小或日期必须真的比过、对得上）
			for _, f := range loose[looseKey(key)] {
				dOK, dSure := c.dateOK(f, s.loc)
				if c.sizeOK(f) && dOK && (c.size != "" || dSure) {
					matched[i] = true
					s.log.Info("ingest plan: matched after ignoring blanks and underscores", "phone_name", strconv.Quote(name))
					break
				}
			}
		}
	}
	// 名字对得上的不少，拍摄日期却一个都对不上：多半是日期没看懂对（手机的日期格式、时区），而不是这么多同名的照片。
	// 这时不比日期，免得每次都把传过的全部重传一遍
	if len(dateMismatch) >= 5 && dateAgreed == 0 {
		i := dateMismatch[0]
		s.log.Warn("ingest plan: dates ignored, none of them agreed", "mismatches", len(dateMismatch),
			"sample_name", strconv.Quote(names[i]), "sample_date", strconv.Quote(dates[i]), "stored", takenDays(stored[keys[i]], s.loc))
		for _, i := range dateMismatch {
			matched[i] = true
		}
		dateMismatch = nil
	}
	for n, i := range dateMismatch {
		if n == 3 {
			break
		}
		s.log.Info("ingest plan: same name, taken on another day", "phone_name", strconv.Quote(names[i]),
			"phone_date", strconv.Quote(dates[i]), "stored", takenDays(stored[keys[i]], s.loc))
	}

	var todo, firstTodo []string // firstTodo：前几个要传的文件名原样（带引号）记进日志，对不上时看得出差在哪
	for i, name := range names {
		if matched[i] {
			continue
		}
		todo = append(todo, strconv.Itoa(i+1))
		if len(firstTodo) < 3 {
			detail := strconv.Quote(name)
			if sizes != nil {
				detail += " " + strings.TrimSpace(sizes[i])
			}
			if dates != nil {
				detail += " " + strings.TrimSpace(dates[i])
			}
			firstTodo = append(firstTodo, detail)
		}
	}
	uploaded := len(names) - len(todo)
	s.log.Info("ingest plan", "folder", folder, "files", len(names), "uploaded", uploaded, "todo", len(todo),
		"by_date", dates != nil, "date_agreed", dateAgreed, "date_mismatch", len(dateMismatch),
		"unparsed_sizes", unparsedSizes, "unparsed_dates", unparsedDates, "first_todo", strings.Join(firstTodo, ", "), "client", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"todo": strings.Join(todo, "\n"), "count": len(todo), "uploaded": uploaded, "files": len(names)})
}

// planCheck 是手机上一个文件除了名字还能比的：显示的大小、拍摄日期可能是哪几天。空的不比。
type planCheck struct {
	size string
	days []int64
}

func (c planCheck) sizeOK(f meta.NameSize) bool {
	if c.size == "" {
		return true
	}
	ok, _ := sizeMatchesAny(c.size, []int64{f.Size})
	return ok
}

// dateOK：拍摄日期前后差一天以内（或者不比日期、网盘里不知道拍摄时间）。sure 表示真的比过了。
func (c planCheck) dateOK(f meta.NameSize, loc *time.Location) (ok, sure bool) {
	if len(c.days) == 0 || f.TakenAt == nil {
		return true, false
	}
	d := dayNumber(f.TakenAt.In(loc))
	for _, pd := range c.days {
		if d-pd <= 1 && pd-d <= 1 {
			return true, true
		}
	}
	return false, true
}

// takenDays 列出几个文件的拍摄日期（记日志用）。
func takenDays(files []meta.NameSize, loc *time.Location) string {
	var out []string
	for _, f := range files {
		if f.TakenAt == nil {
			out = append(out, "?")
		} else {
			out = append(out, f.TakenAt.In(loc).Format("2006-01-02"))
		}
	}
	return strings.Join(out, ",")
}

func isMediaExt(ext string) bool {
	t := media.ByExt(ext)
	return strings.HasPrefix(t, "image/") || strings.HasPrefix(t, "video/")
}

func dayNumber(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
}

var (
	// 年在前：2026年10月18日、2026/10/18、2026-10-18、2026. 10. 18.、2026년 10월 18일
	ymdRe = regexp.MustCompile(`(\d{4})\s*[年년/.\-]\s*(\d{1,2})\s*[月월/.\-]\s*(\d{1,2})`)
	// 年在后：10/18/2026（美国，月在前）、18/10/2026、18.10.2026、18-10-2026（日在前）。分不清时两种都算
	xxyRe = regexp.MustCompile(`(\d{1,2})\s*[/.\-]\s*(\d{1,2})\s*[/.\-]\s*(\d{4})`)
	// 英文月份：Oct 18, 2026、October 18th 2026、18 Oct 2026
	mdyRe = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})`)
	dmyRe = regexp.MustCompile(`(?i)\b(\d{1,2})\.?\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?,?\s+(\d{4})`)
)

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

// photoDays 看快捷指令「创建日期」变成的文字是哪一天（随手机的语言、地区：“2026年10月18日 下午3:04”
// “Oct 18, 2026 at 3:04 PM”“18.10.2026, 15:04”），只要年月日。月、日分不清先后（“10/11/2026”）时两种都返回。
// 看不懂（比如两位数的年、别的语言的月份名）返回 nil，这时只比名字。
func photoDays(text string) []int64 {
	text = strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(text)
	var days []int64
	add := func(y, m, d int) {
		if y < 1900 || y > 2200 {
			return
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		if t.Year() == y && int(t.Month()) == m && t.Day() == d { // 13 月、2 月 30 日这种不算
			days = append(days, t.Unix()/86400)
		}
	}
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case ymdRe.MatchString(text):
		m := ymdRe.FindStringSubmatch(text)
		add(num(m[1]), num(m[2]), num(m[3]))
	case mdyRe.MatchString(text):
		m := mdyRe.FindStringSubmatch(text)
		add(num(m[3]), monthNames[strings.ToLower(m[1])], num(m[2]))
	case dmyRe.MatchString(text):
		m := dmyRe.FindStringSubmatch(text)
		add(num(m[3]), monthNames[strings.ToLower(m[2])], num(m[1]))
	case xxyRe.MatchString(text):
		m := xxyRe.FindStringSubmatch(text)
		a, b, y := num(m[1]), num(m[2]), num(m[3])
		add(y, a, b)
		if a != b {
			add(y, b, a)
		}
	}
	return days
}

// splitLines 按行拆开（快捷指令的「合并文本」用换行连接）；空文本是零行。每一行都以 \r 结尾时（用的是 \r\n）
// 去掉 \r。别的空白不去掉：文件名里可能就有制表符这类字符（上传时被换成了“_”，比较时 planKey 也会换），
// 先去掉就对不上了——video_.mov 每次都被当成没传过，就是这样。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	crlf := len(lines) > 1
	for _, l := range lines[:len(lines)-1] {
		if !strings.HasSuffix(l, "\r") {
			crlf = false
			break
		}
	}
	if crlf {
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
	}
	return lines
}

// looseKey 去掉比较用的名字里所有空白、控制字符、零宽字符和“_”（兜底比较用，大小也必须对得上）。
func looseKey(key string) string {
	return strings.Map(func(c rune) rune {
		if c == '_' || unicode.IsSpace(c) || unicode.IsControl(c) || unicode.Is(unicode.Cf, c) { // Cf：零宽字符等
			return -1
		}
		return c
	}, key)
}

// planKey 是比较用的名字：去掉自动加的序号、扩展名（存着的名字带扩展名，快捷指令给的不带），不分大小写。
func planKey(name string, hasExt bool) string {
	name = stripCopySuffix(cleanName(name))
	if hasExt {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// sizeTextRe 是快捷指令把「文件大小」变成文字的样子：“30.4 MB”“254 KB”“999 bytes”（小数点也可能是逗号）。
var sizeTextRe = regexp.MustCompile(`^([0-9]+(?:[.,][0-9]+)?)\s*([A-Za-z]+|字节)$`)

// sizeMatchesAny：显示成 text 的大小和 sizes 里的某一个对得上吗。parsed 为 false 表示看不懂 text。
// 按显示的精度比（“30.4 MB”是 30.3–30.5 MB，末位差 1 也算，不管四舍五入还是截断）；1000 和 1024 进制都试。
func sizeMatchesAny(text string, sizes []int64) (match, parsed bool) {
	text = strings.TrimSpace(strings.NewReplacer("\u00a0", " ", "\u202f", " ").Replace(text)) // 数字和单位之间可能是不换行空格
	if n := parseByteCount(text); n > 0 {                                                     // 纯数字：精确的字节数
		for _, s := range sizes {
			if s == n {
				return true, true
			}
		}
		return false, true
	}
	if strings.HasPrefix(strings.ToLower(text), "zero ") {
		for _, s := range sizes {
			if s == 0 {
				return true, true
			}
		}
		return false, true
	}
	m := sizeTextRe.FindStringSubmatch(text)
	if m == nil {
		return false, false
	}
	num := strings.Replace(m[1], ",", ".", 1)
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return false, false
	}
	decimals := 0
	if i := strings.IndexByte(num, '.'); i >= 0 {
		decimals = len(num) - i - 1
	}
	exp := map[string]int{"b": 0, "byte": 0, "bytes": 0, "字节": 0, "k": 1, "kb": 1, "m": 2, "mb": 2, "g": 3, "gb": 3, "t": 4, "tb": 4}
	e, ok := exp[strings.ToLower(m[2])]
	if !ok {
		return false, false
	}
	tol := math.Pow(10, -float64(decimals))
	if e == 0 {
		tol = 0
	}
	for _, base := range []float64{1000, 1024} {
		unit := math.Pow(base, float64(e))
		lo, hi := (v-tol)*unit, (v+tol)*unit
		for _, s := range sizes {
			if f := float64(s); f >= lo-0.5 && f <= hi+0.5 {
				return true, true
			}
		}
	}
	return false, true
}
