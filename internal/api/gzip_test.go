package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSONGzip(t *testing.T) {
	big := map[string]any{"items": strings.Repeat("abcdef0123456789", 4000)}
	small := map[string]any{"items": "x"}
	cases := []struct {
		name   string
		accept string
		v      any
		gz     bool
	}{
		{"big+gzip", "gzip, deflate, br", big, true},
		{"big no accept", "", big, false},
		{"big q=0", "gzip;q=0", big, false},
		{"small+gzip", "gzip", small, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/folders/root/children", nil)
		if c.accept != "" {
			r.Header.Set("Accept-Encoding", c.accept)
		}
		w := httptest.NewRecorder()
		writeJSONGzip(w, r, http.StatusOK, c.v)
		res := w.Result()
		if got := res.Header.Get("Content-Encoding") == "gzip"; got != c.gz {
			t.Fatalf("%s: gzip=%v want %v", c.name, got, c.gz)
		}
		var body io.Reader = res.Body
		if c.gz {
			zr, err := gzip.NewReader(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = zr
		}
		var out map[string]any
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if out["items"] != c.v.(map[string]any)["items"] {
			t.Fatalf("%s: body mismatch", c.name)
		}
	}
}
