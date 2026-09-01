package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Admaing/diagflow/internal/observability/store"
)

func TestAuthMiddleware(t *testing.T) {
	srv := newTestServer()
	srv.authToken = "secret"
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d want 401", rec.Code)
	}

	req := httptest.NewRequest("POST", "/api/v1/diagnose", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d want 401", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/v1/diagnose", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid token rejected")
	}

	// Health endpoints stay unauthenticated.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz behind auth: got %d", rec.Code)
	}
}

func TestSessionCacheLifecycle(t *testing.T) {
	c := newSessionCache(50*time.Millisecond, 3)
	e1 := &store.Entry{EventID: "e1", RootCause: "OOM"}

	if c.History("s1") != nil {
		t.Fatal("empty session should have no history")
	}
	c.Append("s1", e1)
	hist := c.History("s1")
	if len(hist) != 1 || hist[0].EventID != "e1" {
		t.Fatalf("unexpected history: %+v", hist)
	}

	// TTL expiry.
	time.Sleep(60 * time.Millisecond)
	if c.History("s1") != nil {
		t.Fatal("expired session should return nil")
	}

	// Per-session isolation + cap.
	c2 := newSessionCache(time.Hour, 2)
	for i := 0; i < 5; i++ {
		c2.Append("s", &store.Entry{EventID: string(rune('a' + i))})
	}
	c2.Append("other", &store.Entry{EventID: "x"})
	if got := len(c2.History("s")); got != 2 {
		t.Fatalf("cap: got %d entries, want 2", got)
	}
	if got := len(c2.History("other")); got != 1 {
		t.Fatalf("isolation: got %d entries, want 1", got)
	}
}

func TestHistoryReplayFromDB(t *testing.T) {
	srv := newTestServer()
	// dbStore nil → replay returns nil without panicking.
	if hist := srv.history("nonexistent"); hist != nil {
		t.Fatalf("expected nil history with no db, got %+v", hist)
	}
}
