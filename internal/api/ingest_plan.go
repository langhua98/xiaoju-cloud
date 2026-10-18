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
	"unicode"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

// POST /api/ingest/plan：快捷指令开始时把文件夹里所有文件的名字（不带扩展名）和大小各一行一次发来，
// 服务端回答哪些还没传过（第几个，从 1 开始）。手机只处理要传的文件，跳过上千个传过的只要几秒；
// 以前在手机上一个个比对，快捷指令每个文件要执行三十多个动作，跳过一个就要 3–5 秒。
//
//	请求体 JSON：{"album": "hidden", "names": "IMG_0001\nIMG_0002", "sizes": "2.1 MB\n30.4 MB"}
//	回答：{"todo": "2\n5", "count": 2, "uploaded": 120}
//
// 名字去掉自动加的序号、扩展名后不分大小写比较；大小是快捷指令「文件大小」变成的文字（“30.4 MB”），
// 按显示的精度比较（同名的实况照片和视频靠大小区分）。看不懂的大小只比名字，日志里记个数。
// 和 exists 一样只透露“这个名字、差不多这个大小的文件在不在”，不能列出相册里有什么。
func (s *Server) ingestPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Album string `json:"album"`
		Names string `json:"names"`
		Sizes string `json:"sizes"`
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
	names, sizes := splitLines(req.Names), splitLines(req.Sizes)
	if len(names) != len(sizes) {
		writeError(w, http.StatusBadRequest, "names and sizes must have the same number of lines")
		return
	}

	// stored：比较用的名字 → 大小；loose：再去掉空白、控制字符和“_”的名字 → 大小（兜底，见 looseKey）
	stored, loose := map[string][]int64{}, map[string][]int64{}
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
			stored[k] = append(stored[k], f.Size)
			loose[looseKey(k)] = append(loose[looseKey(k)], f.Size)
		}
	}

	var todo, firstTodo []string // firstTodo：前几个要传的文件名原样（带引号）记进日志，对不上时看得出差在哪
	uploaded, unparsed := 0, 0
	for i, name := range names {
		match := false
		key := planKey(name, false)
		if candidates := stored[key]; len(candidates) > 0 {
			ok, parsed := sizeMatchesAny(sizes[i], candidates)
			if !parsed {
				unparsed++
				ok = true // 大小看不懂：只比名字
			}
			match = ok
		}
		if !match {
			// 兜底：名字里有制表符这类看不见的字符时，上传时被换成了“_”，这里两边各去掉再比（大小必须对得上）
			if candidates := loose[looseKey(key)]; len(candidates) > 0 {
				if ok, parsed := sizeMatchesAny(sizes[i], candidates); ok && parsed {
					match = true
					s.log.Info("ingest plan: matched after ignoring blanks and underscores", "phone_name", strconv.Quote(name))
				}
			}
		}
		if match {
			uploaded++
		} else {
			todo = append(todo, strconv.Itoa(i+1))
			if len(firstTodo) < 3 {
				firstTodo = append(firstTodo, strconv.Quote(name)+" "+strings.TrimSpace(sizes[i]))
			}
		}
	}
	s.log.Info("ingest plan", "folder", folder, "files", len(names), "uploaded", uploaded, "todo", len(todo),
		"unparsed_sizes", unparsed, "first_todo", strings.Join(firstTodo, ", "), "client", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"todo": strings.Join(todo, "\n"), "count": len(todo), "uploaded": uploaded})
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
