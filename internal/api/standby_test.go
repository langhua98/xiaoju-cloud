package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStandbyGuard(t *testing.T) {
	s := New(nil, Options{Token: "t", Standby: true, CORSOrigins: []string{"https://web.example"}})
	// 放行的请求交给一个什么都不做的处理器（真的处理器要数据库）；预检请求走真的 cors
	h := s.standbyGuard(s.cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	cases := []struct {
		method, path string
		locked       bool
	}{
		{"GET", "/healthz", false},
		{"POST", "/api/folders", true},
		{"POST", "/api/files", true},
		{"PUT", "/api/uploads/abc", true},
		{"DELETE", "/api/files/abc", true},
		{"PATCH", "/api/files/abc", true},
		{"POST", "/api/ingest", true},
		{"POST", "/api/maintenance/reconcile", true},
		{"POST", "/api/backup/db", true},
		{"POST", "/u/tok", true},
		{"POST", "/mcp", true},
		{"GET", "/mcp", true},
		{"POST", "/api/auth/session", false},
		{"POST", "/api/auth/passkey/login/begin", false},
		{"POST", "/api/auth/passkey/register/begin", true},
		{"POST", "/api/auth/grid/verify", false},
		{"POST", "/api/files/abc/link", false},
		{"POST", "/api/s/tok/unlock", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Origin", "https://web.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Code == http.StatusLocked; got != c.locked {
			t.Errorf("%s %s: status %d, locked=%v want %v", c.method, c.path, rec.Code, got, c.locked)
		}
		if rec.Header().Get(standbyHeader) != "1" {
			t.Errorf("%s %s: missing %s header", c.method, c.path, standbyHeader)
		}
		if c.locked && rec.Header().Get("Access-Control-Allow-Origin") != "https://web.example" {
			t.Errorf("%s %s: locked response lacks CORS header", c.method, c.path)
		}
	}
	// 跨域预检照常放行
	req := httptest.NewRequest("OPTIONS", "/api/folders", nil)
	req.Header.Set("Origin", "https://web.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status %d", rec.Code)
	}
}

func TestStandbyHandlerWired(t *testing.T) {
	h := New(nil, Options{Token: "t", Standby: true}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/folders", nil))
	if rec.Code != http.StatusLocked || rec.Header().Get(standbyHeader) != "1" {
		t.Fatalf("status %d header %q", rec.Code, rec.Header().Get(standbyHeader))
	}
}

func TestNoStandbyHeaderNormally(t *testing.T) {
	h := New(nil, Options{Token: "t"}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Header().Get(standbyHeader) != "" {
		t.Fatal("standby header set on a normal server")
	}
}
