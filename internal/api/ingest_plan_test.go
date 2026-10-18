package api

import "testing"

func TestSizeMatchesAny(t *testing.T) {
	for _, c := range []struct {
		text          string
		size          int64
		match, parsed bool
	}{
		{"30.4 MB", 30400819, true, true},
		{"30.4 MB", 30400819, true, true},
		{"30,4 MB", 30400819, true, true},
		{"30.4 MB", 31900000, true, true}, // 1024 进制：30.4 MiB ≈ 31.9 MB
		{"30.4 MB", 29000000, false, true},
		{"254 KB", 254891, true, true},
		{"254 KB", 270000, false, true},
		{"1.17 GB", 1171012000, true, true},
		{"999 bytes", 999, true, true},
		{"999 bytes", 998, false, true},
		{"30,400,819", 30400819, true, true},
		{"30400819", 30400818, false, true},
		{"Zero KB", 0, true, true},
		{"很大", 1, false, false},
		{"", 1, false, false},
	} {
		match, parsed := sizeMatchesAny(c.text, []int64{c.size})
		if match != c.match || parsed != c.parsed {
			t.Errorf("%q vs %d: match=%v parsed=%v, want %v %v", c.text, c.size, match, parsed, c.match, c.parsed)
		}
	}
}

func TestPlanKey(t *testing.T) {
	for stored, phone := range map[string]string{
		"IMG_0001.JPG":          "IMG_0001",
		"IMG_0001 (2).jpg":      "img_0001",
		"video_.mov":            "video_",
		"Screen 2025.04.07.MP4": "Screen 2025.04.07",
	} {
		if planKey(stored, true) != planKey(phone, false) {
			t.Errorf("%q and %q should match: %q vs %q", stored, phone, planKey(stored, true), planKey(phone, false))
		}
	}
}
