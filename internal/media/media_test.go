package media

import "testing"

func TestSniff(t *testing.T) {
	for _, c := range []struct{ head, ext string }{
		{"\xFF\xD8\xFF\xE1....", ".jpg"},
		{"\x89PNG\r\n\x1a\n....", ".png"},
		{"GIF89a......", ".gif"},
		{"\x00\x00\x00\x18ftypheic\x00\x00", ".heic"},
		{"\x00\x00\x00\x1cftypavif\x00\x00", ".avif"},
		{"\x00\x00\x00\x14ftypqt  \x00\x00", ".mov"},
		{"\x00\x00\x00\x20ftypisom\x00\x00", ".mp4"},
		{"RIFF\x00\x00\x00\x00WEBPVP8 ", ".webp"},
		{"\x1a\x45\xdf\xa3\x9f\x42\x86\x81", ".webm"},
		{"hello world", ""},
		{"", ""},
	} {
		if ext, _ := Sniff([]byte(c.head)); ext != c.ext {
			t.Errorf("%q: got %q, want %q", c.head, ext, c.ext)
		}
	}
}
