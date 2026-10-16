package thumb

import (
	"bytes"
	"encoding/binary"
	"io"
	"regexp"
	"strings"
	"time"
)

// 读拍摄时间（相册按拍摄时间排序用）。照片读 EXIF，视频读 ffmpeg 打出来的元数据；读不到返回零值，
// 由调用方改用上传时间。都是尽力而为：格式不对、数据坏了就当没有，不报错。

const maxMetaBytes = 1 << 20 // 照片的 EXIF 在文件开头这么多字节里找（JPEG 的 APP1 最多 64 KB）

// photoTaken 读照片的拍摄时间：JPEG、PNG 的 EXIF，HEIC / AVIF 里的 Exif 项。
// EXIF 里没写时区时按 loc 算（nil 时按 UTC）。
func photoTaken(r io.ReaderAt, size int64, typ string, loc *time.Location) (taken time.Time) {
	defer func() { // 文件是别人传上来的，解析时出了没想到的错也不能让服务崩溃
		if recover() != nil {
			taken = time.Time{}
		}
	}()
	var tiff []byte
	switch typ {
	case "image/jpeg":
		tiff = jpegExif(r, size)
	case "image/png":
		tiff = pngExif(r, size)
	case "image/heic", "image/heif", "image/avif":
		tiff = heifExif(r, size)
	}
	if tiff == nil {
		return time.Time{}
	}
	return exifTaken(tiff, loc)
}

func readAt(r io.ReaderAt, off, n, size int64) []byte {
	if off < 0 || n < 0 || off > size || n > size-off || n > maxMetaBytes {
		return nil
	}
	b := make([]byte, n)
	if _, err := r.ReadAt(b, off); err != nil && !(err == io.EOF && off+n == size) {
		return nil
	}
	return b
}

// jpegExif 找 APP1 里的 "Exif\0\0"，返回后面的 TIFF 数据。
func jpegExif(r io.ReaderAt, size int64) []byte {
	b := readAt(r, 0, min(size, maxMetaBytes), size)
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // 图像数据开始了
			return nil
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return nil
		}
		seg := b[i+4 : i+2+n]
		if marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return seg[6:]
		}
		i += 2 + n
	}
	return nil
}

// pngExif 找 eXIf 块（最多看前 1000 块，eXIf 一般在图像数据前面）。
func pngExif(r io.ReaderAt, size int64) []byte {
	for off, i := int64(8), 0; off+8 <= size && i < 1000; i++ {
		h := readAt(r, off, 8, size)
		if h == nil {
			return nil
		}
		n := int64(binary.BigEndian.Uint32(h))
		switch string(h[4:8]) {
		case "eXIf":
			return readAt(r, off+8, n, size)
		case "IEND":
			return nil
		}
		off += 12 + n
	}
	return nil
}

// box 是 ISOBMFF（HEIC、MP4）的一个盒子：类型和内容在文件里的位置。
type box struct {
	typ       string
	off, size int64 // 内容（不含盒子头）
}

// boxes 列出 [off, end) 里的盒子。
func boxes(r io.ReaderAt, off, end, size int64) []box {
	var out []box
	for off+8 <= end && len(out) < 1000 {
		h := readAt(r, off, 16, size)
		if h == nil {
			h = readAt(r, off, 8, size)
		}
		if h == nil {
			return out
		}
		n, hdr := int64(binary.BigEndian.Uint32(h)), int64(8)
		switch {
		case n == 1 && len(h) == 16:
			n, hdr = int64(binary.BigEndian.Uint64(h[8:])), 16
		case n == 0:
			n = end - off
		}
		if n < hdr || off+n > end {
			return out
		}
		out = append(out, box{typ: string(h[4:8]), off: off + hdr, size: n - hdr})
		off += n
	}
	return out
}

func findBox(bs []box, typ string) (box, bool) {
	for _, b := range bs {
		if b.typ == typ {
			return b, true
		}
	}
	return box{}, false
}

// heifExif：meta 盒子里，iinf 找出类型为 Exif 的项，iloc 找出它在文件里的位置。
func heifExif(r io.ReaderAt, size int64) []byte {
	meta, ok := findBox(boxes(r, 0, min(size, 64<<20), size), "meta")
	if !ok || meta.size < 4 {
		return nil
	}
	inner := boxes(r, meta.off+4, meta.off+meta.size, size) // meta 是 full box：先跳过版本和标志
	iinf, ok1 := findBox(inner, "iinf")
	iloc, ok2 := findBox(inner, "iloc")
	if !ok1 || !ok2 {
		return nil
	}
	id, ok := exifItemID(r, iinf, size)
	if !ok {
		return nil
	}
	off, n, ok := itemLocation(readAt(r, iloc.off, iloc.size, size), id)
	if !ok {
		return nil
	}
	// Exif 项开头 4 个字节是到 TIFF 头的偏移（后面通常是 "Exif\0\0"）
	b := readAt(r, off, min(n, maxMetaBytes), size)
	if len(b) < 4 {
		return nil
	}
	skip := int(binary.BigEndian.Uint32(b))
	if skip > len(b)-4 {
		return nil
	}
	return b[4+skip:]
}

func exifItemID(r io.ReaderAt, iinf box, size int64) (uint32, bool) {
	b := readAt(r, iinf.off, min(iinf.size, 6), size)
	if len(b) < 6 {
		return 0, false
	}
	start := iinf.off + 6 // 版本 0：项数是 2 个字节
	if b[0] != 0 {
		start = iinf.off + 8
	}
	for _, e := range boxes(r, start, iinf.off+iinf.size, size) {
		if e.typ != "infe" {
			continue
		}
		d := readAt(r, e.off, min(e.size, 32), size)
		if len(d) < 12 || d[0] < 2 { // 版本 2、3 才有项类型
			continue
		}
		id, typ := uint32(binary.BigEndian.Uint16(d[4:])), d[8:12]
		if d[0] >= 3 {
			if len(d) < 14 {
				continue
			}
			id, typ = binary.BigEndian.Uint32(d[4:]), d[10:14]
		}
		if string(typ) == "Exif" {
			return id, true
		}
	}
	return 0, false
}

// itemLocation 从 iloc 的内容里找出项 id 的第一段数据（只认直接放在文件里的，iPhone 都是这样）。
func itemLocation(b []byte, id uint32) (off, n int64, ok bool) {
	if len(b) < 8 {
		return 0, 0, false
	}
	version := b[0]
	offSize, lenSize, baseSize := int(b[4]>>4), int(b[4]&15), int(b[5]>>4)
	idxSize := 0
	if version == 1 || version == 2 {
		idxSize = int(b[5] & 15)
	}
	p := 6
	num := func(size int) (uint64, bool) {
		if size != 0 && size != 4 && size != 8 && size != 2 || p+size > len(b) {
			return 0, false
		}
		var v uint64
		for _, c := range b[p : p+size] {
			v = v<<8 | uint64(c)
		}
		p += size
		return v, true
	}
	idSize := 2
	if version == 2 {
		idSize = 4
	}
	count, ok := num(idSize)
	for i := uint64(0); ok && i < count; i++ {
		var item, method, base, extents uint64
		if item, ok = num(idSize); !ok {
			break
		}
		if version == 1 || version == 2 {
			if method, ok = num(2); !ok {
				break
			}
			method &= 15
		}
		_, ok = num(2) // data_reference_index
		if base, ok = num(baseSize); !ok {
			break
		}
		if extents, ok = num(2); !ok {
			break
		}
		for j := uint64(0); ok && j < extents; j++ {
			var eoff, elen uint64
			if _, ok = num(idxSize); !ok {
				break
			}
			if eoff, ok = num(offSize); !ok {
				break
			}
			if elen, ok = num(lenSize); !ok {
				break
			}
			if j == 0 && uint32(item) == id {
				return int64(base + eoff), int64(elen), method == 0 && elen > 0
			}
		}
	}
	return 0, 0, false
}

// exifTaken 从 TIFF 格式的 EXIF 里读拍摄时间：优先 DateTimeOriginal，其次 DateTimeDigitized，最后 IFD0 的 DateTime，
// 各自配对应的时区（iPhone 都写了）。
func exifTaken(t []byte, loc *time.Location) time.Time {
	if len(t) < 8 {
		return time.Time{}
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return time.Time{}
	}
	ifd0 := tiffIFD(t, bo, bo.Uint32(t[4:]))
	var exif map[uint16][]byte
	if p := ifd0[0x8769]; len(p) == 4 {
		exif = tiffIFD(t, bo, bo.Uint32(p))
	}
	for _, pair := range [][2]uint16{{0x9003, 0x9011}, {0x9004, 0x9012}} {
		if tm := exifTime(exif[pair[0]], exif[pair[1]], loc); !tm.IsZero() {
			return tm
		}
	}
	return exifTime(ifd0[0x0132], exif[0x9010], loc)
}

// tiffIFD 读一个 IFD，返回各标签的原始值（不超过 4 字节的直接放在条目里，长的按偏移取）。
func tiffIFD(t []byte, bo binary.ByteOrder, off uint32) map[uint16][]byte {
	out := make(map[uint16][]byte)
	if int64(off)+2 > int64(len(t)) {
		return out
	}
	count := int(bo.Uint16(t[off:]))
	sizes := map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}
	for i := 0; i < count; i++ {
		e := int(off) + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		tag, typ, n := bo.Uint16(t[e:]), bo.Uint16(t[e+2:]), int64(bo.Uint32(t[e+4:]))
		size := int64(sizes[typ]) * n
		if size == 0 || size > int64(len(t)) {
			continue
		}
		v := t[e+8 : e+12]
		if size > 4 {
			p := int64(bo.Uint32(t[e+8:]))
			if p+size > int64(len(t)) {
				continue
			}
			v = t[p : p+size]
		} else {
			v = v[:size]
		}
		out[tag] = v
	}
	return out
}

func exifTime(value, offset []byte, loc *time.Location) time.Time {
	s := strings.TrimRight(string(value), "\x00 ")
	if s == "" {
		return time.Time{}
	}
	if loc == nil {
		loc = time.UTC
	}
	tm, err := time.ParseInLocation("2006:01:02 15:04:05", s, loc)
	if err != nil {
		return time.Time{}
	}
	if o := strings.TrimRight(string(offset), "\x00 "); o != "" {
		if z, err := time.Parse("2006:01:02 15:04:05-07:00", s+o); err == nil {
			tm = z
		}
	}
	return plausible(tm)
}

// plausible 去掉明显不对的时间（相机没设时间时的 1970 年、未来的时间）。
func plausible(t time.Time) time.Time {
	if t.Year() < 1980 || t.After(time.Now().Add(48*time.Hour)) {
		return time.Time{}
	}
	return t
}

var (
	appleCreationRe = regexp.MustCompile(`(?m)^\s*com\.apple\.quicktime\.creationdate\s*:\s*(\S+)`)
	creationTimeRe  = regexp.MustCompile(`(?m)^\s*creation_time\s*:\s*(\S+(?: \d\d:\d\d:\d\d)?)`)
)

// videoTaken 从 ffmpeg 的输出里读视频的拍摄时间：iPhone 写的 com.apple.quicktime.creationdate（带拍摄地的时区），
// 没有时用 creation_time（UTC）。
func videoTaken(stderr string) time.Time {
	for _, re := range []*regexp.Regexp{appleCreationRe, creationTimeRe} {
		m := re.FindStringSubmatch(stderr)
		if m == nil {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05"} {
			if tm, err := time.Parse(layout, m[1]); err == nil {
				if tm = plausible(tm); !tm.IsZero() {
					return tm
				}
				break
			}
		}
	}
	return time.Time{}
}
