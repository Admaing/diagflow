package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Admaing/diagflow/internal/diagagent"
)

// parseSSE splits a recorded stream into (event, data) pairs.
func parseSSE(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, chunk := range strings.Split(body, "\n\n") {
		var name string
		var data []string
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case line == "" || strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if name != "" && len(data) > 0 {
			out = append(out, [2]string{name, strings.Join(data, "\n")})
		}
	}
	return out
}

// stubDiagnose replaces the real pipeline so the streaming transport can be
// tested without a live model. The integration path is covered by the
// scenario tests, which do call it.
func stubDiagnose(t *testing.T, steps []string, err error) diagnoseFunc {
	t.Helper()
	return func(ctx context.Context, req *DiagnoseRequest, onEvent func(string)) (*diagagent.Report, string, error) {
		for _, s := range steps {
			onEvent(s)
		}
		if err != nil {
			return nil, "", err
		}
		return &diagagent.Report{EventID: "diag-test", Component: req.Component}, "## stub report", nil
	}
}

func TestDiagnoseStreamHappyPath(t *testing.T) {
	srv := newTestServer()
	srv.diagnose = stubDiagnose(t, []string{
		"[diag-test] starting flink/Job FAILED",
		"[diag-test] Phase 3: SDK ReAct",
		"[react] ssh_exec({\"cmd\":\"ls\"})",
		"[react] ssh_exec error: mcp error -32602",
	}, nil)
	rec := httptest.NewRecorder()
	body := `{"component":"flink","problem":"Job FAILED"}`
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose/stream", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	// A buffering proxy would defeat the point of streaming.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events were emitted")
	}
	var gotReport, gotDone bool
	var step stepEvent
	for _, ev := range events {
		switch ev[0] {
		case sseStep:
			if err := json.Unmarshal([]byte(ev[1]), &step); err != nil {
				t.Fatalf("step payload is not JSON: %v", err)
			}
		case sseReport:
			gotReport = true
			var payload struct {
				Report   map[string]any `json:"report"`
				Markdown string         `json:"markdown"`
			}
			if err := json.Unmarshal([]byte(ev[1]), &payload); err != nil {
				t.Fatalf("report payload: %v", err)
			}
			if payload.Markdown == "" {
				t.Fatal("report markdown is empty")
			}
		case sseDone:
			gotDone = true
		case sseFailed:
			t.Fatalf("unexpected failure event: %s", ev[1])
		}
	}
	if !gotReport {
		t.Fatal("stream ended without a report event")
	}
	if !gotDone {
		t.Fatal("stream ended without a done event")
	}
	if step.Text == "" {
		t.Fatal("step events carried no text")
	}
	// The last event must be the terminator, so a client that reads to EOF
	// knows the run finished rather than merely stalled.
	if events[len(events)-1][0] != sseDone {
		t.Fatalf("last event = %q, want %q", events[len(events)-1][0], sseDone)
	}
}

func TestDiagnoseStreamValidatesBeforeOpeningStream(t *testing.T) {
	srv := newTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose/stream",
		strings.NewReader(`{"component":"flink"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400", rec.Code)
	}
	// A bad request must still be a normal JSON error, not an SSE stream.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestClassifyEvent(t *testing.T) {
	cases := []struct{ msg, kind, tool string }{
		{"[react] ssh_exec({\"cmd\":\"ls\"})", "tool_call", "ssh_exec"},
		{"[react] ssh_exec result: ok", "tool_result", "ssh_exec"},
		{"[react] deepwiki_query error: tls", "tool_error", "deepwiki_query"},
		{"[strategy] get_cluster_info(map[])", "strategy", "strategy"},
		{"[agent] synthesis failed", "warn", "agent"},
		{"[diag-abc] Phase 3: SDK ReAct", "phase", "diag-abc"},
		{"[diag-abc] done in 1234ms", "phase", "diag-abc"},
		{"[diag-abc] starting hdfs/x", "notice", "diag-abc"},
		{"plain message", "log", ""},
	}
	for _, c := range cases {
		got := classifyEvent(c.msg, 1500*time.Millisecond)
		if got.Kind != c.kind || got.Tool != c.tool {
			t.Errorf("classifyEvent(%q) = (%s,%s), want (%s,%s)",
				c.msg, got.Kind, got.Tool, c.kind, c.tool)
		}
		if got.ElapsedMS != 1500 {
			t.Errorf("classifyEvent(%q) elapsed = %d, want 1500", c.msg, got.ElapsedMS)
		}
		if c.kind != "log" && strings.HasPrefix(got.Text, "[") {
			t.Errorf("classifyEvent(%q) left the tag in the text: %q", c.msg, got.Text)
		}
	}
}

func TestClassifyEventClipsLongText(t *testing.T) {
	got := classifyEvent("[react] ssh_exec result: "+strings.Repeat("x", 5000), 0)
	if len(got.Text) > 700 {
		t.Fatalf("text length = %d, want it clipped", len(got.Text))
	}
}

func TestStreamEventPayloadIsJSONSafe(t *testing.T) {
	// A newline inside a log line would break SSE framing if the payload were
	// written raw; JSON encoding keeps it on one line.
	rec := httptest.NewRecorder()
	writeSSE(rec, rec, sseStep, stepEvent{Kind: "log", Text: "line1\nline2"})
	if n := strings.Count(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n"); n != 1 {
		t.Fatalf("payload spans %d lines, want 1: %q", n+1, rec.Body.String())
	}
}

func TestDiagnoseStreamReportsFailure(t *testing.T) {
	srv := newTestServer()
	srv.diagnose = stubDiagnose(t, []string{"[diag-test] starting flink/x"}, errors.New("model exploded"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/diagnose/stream",
		strings.NewReader(`{"component":"flink","problem":"x"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("stream must open before the run can fail: got %d", rec.Code)
	}
	var failed, done bool
	for _, ev := range parseSSE(t, rec.Body.String()) {
		switch ev[0] {
		case sseFailed:
			failed = true
			if !strings.Contains(ev[1], "model exploded") {
				t.Fatalf("failure payload = %s", ev[1])
			}
		case sseReport:
			t.Fatal("a failed run must not emit a report")
		case sseDone:
			done = true
		}
	}
	if !failed || !done {
		t.Fatalf("failed=%v done=%v, want both", failed, done)
	}
}

// TestDiagnoseStreamDeliversProgressively is the test that actually matters for
// this feature: events must reach the client while the run is still going, not
// arrive in one batch at the end. ResponseRecorder cannot show that, so this
// one drives a real HTTP server and reads the body incrementally.
func TestDiagnoseStreamDeliversProgressively(t *testing.T) {
	srv := newTestServer()
	release := make(chan struct{})
	srv.diagnose = func(ctx context.Context, req *DiagnoseRequest, onEvent func(string)) (*diagagent.Report, string, error) {
		onEvent("[diag-test] Phase 2: strategy execution")
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		return &diagagent.Report{EventID: "diag-test"}, "## done", nil
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	var closeOnce sync.Once
	unblock := func() { closeOnce.Do(func() { close(release) }) }
	defer unblock()

	resp, err := http.Post(ts.URL+"/api/v1/diagnose/stream", "application/json",
		strings.NewReader(`{"component":"flink","problem":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The stub is still blocked, so anything readable now is proof of streaming.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first line: %v", err)
	}
	if strings.TrimSpace(line) != "event: "+sseStep {
		t.Fatalf("first line = %q, want %q", line, "event: "+sseStep)
	}

	// Let the run finish and confirm the terminal event arrives.
	unblock()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if !strings.Contains(string(rest), "event: "+sseReport) {
		t.Fatalf("no report event after completion: %s", rest)
	}
}
