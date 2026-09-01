package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
)

func newTestServer() *Server {
	return New(slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), nil)
}

func TestHealthz(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz: got %d want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestDiagnoseValidation(t *testing.T) {
	srv := newTestServer()
	h := srv.Handler()

	// Missing fields → 400.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose",
		strings.NewReader(`{"component":"flink"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing problem: got %d want 400", rec.Code)
	}

	// Invalid JSON → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose", strings.NewReader(`{`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %d want 400", rec.Code)
	}

	// Body over limit → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose",
		strings.NewReader(`{"component":"flink","problem":"x","detail":"`+strings.Repeat("a", srv.cfg.MaxBodyBytes+10)+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d want 400", rec.Code)
	}
}

func TestDiagnoseHappyPathNoLLM(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	body := `{"component":"flink","problem":"Job FAILED","scenario":"flink_oom"}`
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose: got %d want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Report   map[string]any `json:"report"`
		Markdown string         `json:"markdown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Markdown == "" {
		t.Fatal("markdown is empty")
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID header")
	}
}

func TestDiagnoseUnknownScenario(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose",
		strings.NewReader(`{"component":"flink","problem":"x","scenario":"nope"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scenario: got %d want 400", rec.Code)
	}
}

func TestPanicRecovered(t *testing.T) {
	srv := newTestServer()
	// Replace the diagnose handler path with a panicking route via middleware
	// directly: use recoverMiddleware around a panicking handler.
	h := srv.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic recovery: got %d want 500", rec.Code)
	}
}
