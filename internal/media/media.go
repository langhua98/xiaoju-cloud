// Package media 按文件开头的字节认出常见的照片、视频格式（批量上传补扩展名、生成缩略图前确认格式都要用）。
package media

import (
	"bytes"
	"mime"
	"strings"
)

// Sniff 按文件开头的字节（至少 12 字节）认出常见的照片、视频格式，返回扩展名和 MIME 类型；认不出返回空串。
func Sniff(b []byte) (ext, mimeType string) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return ".jpg", "image/jpeg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png", "image/png"
	case bytes.HasPrefix(b, []byte("GIF8")):
		return ".gif", "image/gif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp", "image/webp"
	case bytes.HasPrefix(b, []byte("\x1a\x45\xdf\xa3")): // Matroska / WebM
		return ".webm", "video/webm"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		switch string(b[8:12]) {
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
			return ".heic", "image/heic"
		case "avif", "avis":
			return ".avif", "image/avif"
		case "qt  ":
			return ".mov", "video/quicktime"
		default: // isom、mp41、mp42、M4V、3gp4 …
			return ".mp4", "video/mp4"
		}
	}
	return "", ""
}

// ByExt 按扩展名猜 MIME 类型（补上标准库不认识的 HEIC、MOV 等）。
func ByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".heic", ".heif":
		return "image/heic"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	}
	return mime.TypeByExtension(ext)
}
