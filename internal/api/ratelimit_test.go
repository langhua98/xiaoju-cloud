package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(60, 3)
	for i := range 3 {
		if !l.allow("a") {
			t.Fatalf("request %d within the burst was refused", i)
		}
	}
	if l.allow("a") {
		t.Fatal("over the burst must be refused")
	}
	if !l.allow("b") {
		t.Fatal("another client has its own allowance")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	if got := limitKey(r); got != "1.2.3.4" {
		t.Fatalf("limit key %q: the client can forge the first X-Forwarded-For entry", got)
	}
}

func TestActivity(t *testing.T) {
	s := &Server{started: time.Now()}
	status := func() map[string]any {
		rec := httptest.NewRecorder()
		s.activityStatus(rec, httptest.NewRequest("GET", "/", nil))
		var out map[string]any
		json.NewDecoder(rec.Body).Decode(&out)
		return out
	}
	if a := status(); a["active"] != float64(0) || a["last_activity"] != nil {
		t.Fatalf("idle: %v", a)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.tracked(func(http.ResponseWriter, *http.Request) { <-release })(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
		close(done)
	}()
	for s.act.active.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if a := status(); a["active"] != float64(1) || a["idle_seconds"] != float64(0) {
		t.Fatalf("busy: %v", a)
	}
	close(release)
	<-done
	if a := status(); a["active"] != float64(0) || a["last_activity"] == nil {
		t.Fatalf("after: %v", a)
	}
}
