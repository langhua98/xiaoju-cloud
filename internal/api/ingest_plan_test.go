package api

import (
	"testing"
	"time"
)

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

func TestSplitLines(t *testing.T) {
	for in, want := range map[string][]string{
		"":              nil,
		"a":             {"a"},
		"a\tb\nc ":      {"a\tb", "c "},  // 别的空白原样保留
		"a\r\nb\r\nc":   {"a", "b", "c"}, // \r\n 分隔
		"video\r\nb\nc": {"video\r", "b", "c"},
	} {
		got := splitLines(in)
		if len(got) != len(want) {
			t.Errorf("%q: %q, want %q", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: %q, want %q", in, got, want)
			}
		}
	}
}

func TestPhotoDays(t *testing.T) {
	day := func(y, m, d int) int64 { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Unix() / 86400 }
	for text, want := range map[string][]int64{
		"2026年10月18日 下午3:04":       {day(2026, 10, 18)},
		"2026年10月18日 15:04":        {day(2026, 10, 18)},
		"2026/10/18 15:04":         {day(2026, 10, 18)},
		"2026-10-18T15:04:05Z":     {day(2026, 10, 18)},
		"2026. 10. 18. 오후 3:04":    {day(2026, 10, 18)},
		"Oct 18, 2026 at 3:04 PM":  {day(2026, 10, 18)},
		"October 18th 2026":        {day(2026, 10, 18)},
		"18 Oct 2026 at 15:04":     {day(2026, 10, 18)},
		"18.10.2026, 15:04":        {day(2026, 10, 18)},
		"10/18/2026, 3:04\u202fPM": {day(2026, 10, 18)},
		"10/11/2026":               {day(2026, 10, 11), day(2026, 11, 10)}, // 分不清月、日
		"5/5/2026":                 {day(2026, 5, 5)},
		"2026年2月30日":               nil,
		"10/18/26":                 nil, // 两位数的年
		"18 oct. 2026":             {day(2026, 10, 18)},
		"18 octobre 2026":          {day(2026, 10, 18)},
		"18 de octubre de 2026":    nil,
		"":                         nil,
	} {
		got := photoDays(text)
		if len(got) != len(want) {
			t.Errorf("%q: %v, want %v", text, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: %v, want %v", text, got, want)
			}
		}
	}
}
