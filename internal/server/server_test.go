package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"github.com/Admaing/diagflow/internal/config"
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

func productionServer(t *testing.T) *Server {
	t.Helper()
	config.Reset()
	t.Setenv("DIAGFLOW_MODE", "production")
	t.Setenv("DIAGFLOW_REGION", "test03")
	t.Setenv("DIAGFLOW_ZONE", "test03-01")
	t.Setenv("DIAGFLOW_ZK_HOSTS", "zk-a:2181,zk-b:2181")
	t.Cleanup(config.Reset)
	return New(slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), nil)
}

func TestReadyzReportsZoneInProduction(t *testing.T) {
	srv := productionServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz: got %d want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["zone"] != "test03-01" {
		t.Fatalf("expected zone in readiness, got %v", body)
	}
	if body["mode"] != "production" {
		t.Fatalf("expected production mode, got %v", body["mode"])
	}
	if body["zk_zone"] != "test03-01" {
		t.Fatalf("expected the discovery zone, got %v", body["zk_zone"])
	}
}

// Production without ZooKeeper hosts cannot discover any cluster endpoint, so
// the instance must report itself not ready.
func TestReadyzFailsWithoutZooKeeper(t *testing.T) {
	config.Reset()
	t.Setenv("DIAGFLOW_MODE", "production")
	t.Setenv("DIAGFLOW_REGION", "test03")
	t.Setenv("DIAGFLOW_ZONE", "test03-01")
	t.Cleanup(config.Reset)
	srv := New(slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: got %d want 503, body=%s", rec.Code, rec.Body.String())
	}
}

func TestProductionDiagnoseRequiresClusterID(t *testing.T) {
	srv := productionServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose",
		strings.NewReader(`{"component":"flink","problem":"Job FAILED"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected failure without cluster_id, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cluster_id") {
		t.Fatalf("expected cluster_id error, got %s", rec.Body.String())
	}
}

// The console is served from the binary at "/", and must not shadow the
// dedicated health/metrics routes.
func TestWebUIServed(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"DiagFlow", "/api/v1/diagnose", "id=\"problem\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("console HTML missing %q", want)
		}
	}
}

func TestWebUIDoesNotShadowRoutes(t *testing.T) {
	srv := newTestServer()
	for _, path := range []string{"/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d want 200", path, rec.Code)
		}
		// The console is HTML; these routes must keep their own content type.
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s served the console instead of its own handler (content-type %q)", path, ct)
		}
	}
}

// Unknown paths under the catch-all must 404 rather than serve the console.
func TestWebUIUnknownPathIs404(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope: got %d want 404", rec.Code)
	}
}
