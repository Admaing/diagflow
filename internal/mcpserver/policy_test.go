package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Admaing/diagflow/internal/infra"
)

func optServer(tokens map[string]string, ds DataSource, opts ...Option) *Server {
	return New(slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), ds, tokens, opts...)
}

func TestParseACL(t *testing.T) {
	acl, err := ParseACL("a=uhadoop-1,uhadoop-2;b=*")
	if err != nil {
		t.Fatal(err)
	}
	if !acl.allowed("a", "uhadoop-1") || acl.allowed("a", "uhadoop-3") {
		t.Fatal("agent a allowlist not enforced")
	}
	if !acl.allowed("b", "anything") {
		t.Fatal("wildcard should allow every cluster")
	}
	if acl.allowed("c", "uhadoop-1") {
		t.Fatal("unknown agent must be denied when an ACL is configured")
	}
	if acl.allowed("", "uhadoop-1") {
		t.Fatal("anonymous request must be denied when an ACL is configured")
	}

	if acl, err := ParseACL("  "); err != nil || acl != nil {
		t.Fatalf("empty spec = (%v,%v), want (nil,nil)", acl, err)
	}
	for _, bad := range []string{"nocluster=,", "=uhadoop-1", "uhadoop-1"} {
		if _, err := ParseACL(bad); err == nil {
			t.Errorf("spec %q should be rejected", bad)
		}
	}
}

func TestACLDeniesUnlistedCluster(t *testing.T) {
	h := optServer(map[string]string{"tok": "agent-a"}, &fakeDS{info: readyCluster()},
		WithACL(ACL{"agent-a": {"uhadoop-allowed": {}}})).Handler()

	// fakeDS answers any id; the ACL is what must stop the call.
	if msg := callErr(t, h, "get_cluster_info", `{"cluster_id":"uhadoop-other"}`); !strings.Contains(msg, "not allowed") {
		t.Fatalf("want ACL rejection, got %q", msg)
	}
	if msg := callErr(t, h, "get_cluster_info", `{"cluster_id":"uhadoop-allowed"}`); msg != "" {
		t.Fatalf("allowed cluster rejected: %q", msg)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	h := optServer(map[string]string{"tok": "agent-a"}, &fakeDS{info: readyCluster()},
		WithRateLimit(0.001, 1)).Handler()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_cluster_info","arguments":{"cluster_id":"uhadoop-test"}}}`
	if code, _ := rpc(t, h, body, "tok"); code != http.StatusOK {
		t.Fatalf("first call = %d, want 200", code)
	}
	code, out := rpc(t, h, body, "tok")
	if code != http.StatusTooManyRequests {
		t.Fatalf("second call = %d, want 429", code)
	}
	if e, ok := out["error"].(map[string]any); !ok || e["code"].(float64) != -32005 {
		t.Fatalf("want -32005 error, got %v", out)
	}
}

// readyDS adds the optional readiness probe to fakeDS.
type readyDS struct {
	*fakeDS
	err error
}

func (r readyDS) Ready(context.Context) error { return r.err }

func TestReadyzReflectsUpstream(t *testing.T) {
	okSrv := optServer(nil, readyDS{fakeDS: &fakeDS{info: readyCluster()}})
	rec := httptest.NewRecorder()
	okSrv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200: %s", rec.Code, rec.Body)
	}

	badSrv := optServer(nil, readyDS{fakeDS: &fakeDS{}, err: errors.New("zookeeper unreachable")})
	rec = httptest.NewRecorder()
	badSrv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "zookeeper unreachable") {
		t.Fatalf("reason missing: %s", rec.Body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := optServer(map[string]string{"tok": "agent-a"}, &fakeDS{info: readyCluster()}).Handler()
	callText(t, h, "get_cluster_info", `{"cluster_id":"uhadoop-test"}`)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"mcp_tool_calls_total", "mcp_tool_duration_ms", "mcp_rejected_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s:\n%s", want, body)
		}
	}
}

func TestSearchLogsCapsFanOutAndKeepsCacheIntact(t *testing.T) {
	info := readyCluster()
	info.NodeInfo = nil
	for i := 0; i < 40; i++ {
		// Duplicate ids on purpose: dedupe used to compact the shared slice.
		id := fmt.Sprintf("n%d", i%38)
		info.NodeInfo = append(info.NodeInfo, infra.NodeInfo{
			NodeID: id, NodeName: fmt.Sprintf("c-core%d", i), NodeType: "core", NodeState: 1,
		})
	}
	before := append([]infra.NodeInfo(nil), info.NodeInfo...)

	ds := &fakeDS{info: info, logOut: "ERROR boom"}
	h := optServer(map[string]string{"tok": "a"}, ds, WithMaxSearchNodes(5)).Handler()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_logs","arguments":{"cluster_id":"uhadoop-test","log_path":"/var/log/x.log","keywords":"ERROR"}}}`
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// No t.Fatalf from a non-test goroutine: just drive the handler so
			// the race detector can observe shared-cache access.
			req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer tok")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	if len(info.NodeInfo) != len(before) {
		t.Fatalf("cached NodeInfo mutated: len %d → %d", len(before), len(info.NodeInfo))
	}
	for i := range before {
		if info.NodeInfo[i].NodeID != before[i].NodeID || info.NodeInfo[i].NodeName != before[i].NodeName {
			t.Fatalf("cached NodeInfo[%d] mutated: %+v → %+v", i, before[i], info.NodeInfo[i])
		}
	}
}

func TestSearchLogsReportsSampling(t *testing.T) {
	info := readyCluster()
	info.NodeInfo = nil
	for i := 0; i < 10; i++ {
		info.NodeInfo = append(info.NodeInfo, infra.NodeInfo{
			NodeID: fmt.Sprintf("n%d", i), NodeName: fmt.Sprintf("c-core%d", i), NodeType: "core", NodeState: 1,
		})
	}
	ds := &fakeDS{info: info, logOut: "ERROR boom"}
	h := optServer(map[string]string{"tok": "a"}, ds, WithMaxSearchNodes(3)).Handler()

	text := callText(t, h, "search_logs",
		`{"cluster_id":"uhadoop-test","log_path":"/var/log/x.log","keywords":"ERROR"}`)
	if !strings.Contains(text, "sampled 3 of 10 nodes") {
		t.Fatalf("sampling not reported: %s", text)
	}
	if len(ds.logNodes) != 3 {
		t.Fatalf("fanned out to %d nodes, want 3", len(ds.logNodes))
	}
}

func TestSshExecRequiresCommand(t *testing.T) {
	h := optServer(map[string]string{"tok": "a"},
		&fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, runOut: "x"}).Handler()

	if msg := callErr(t, h, "ssh_exec", `{"cluster_id":"uhadoop-test","action":"jps"}`); !strings.Contains(msg, "command is required") {
		t.Fatalf("action-only request should be rejected, got %q", msg)
	}
}

func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	h := optServer(map[string]string{"tok": "a"}, &fakeDS{}).Handler()

	_, out := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "tok")
	if got := out["result"].(map[string]any)["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("protocolVersion = %v, want 2025-06-18", got)
	}

	_, out = rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, "tok")
	if got := out["result"].(map[string]any)["protocolVersion"]; got != protocolVersion {
		t.Fatalf("unknown revision = %v, want fallback %s", got, protocolVersion)
	}
}

func TestRequestIDHeader(t *testing.T) {
	h := optServer(map[string]string{"tok": "a"}, &fakeDS{}).Handler()

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("X-Request-ID", "caller-supplied")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "caller-supplied" {
		t.Fatalf("request id not propagated: %q", got)
	}
}

func TestACLString(t *testing.T) {
	acl, err := ParseACL("b=z,a; a=y,x ")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := acl.String(), "a=x,y;b=a,z"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	var nilACL ACL
	if got := nilACL.String(); got != "(unrestricted)" {
		t.Fatalf("nil ACL String = %q", got)
	}
}

func TestACLJSONRoundTripDoesNotLeak(t *testing.T) {
	// Guard against accidentally serialising the token map into readiness.
	s := optServer(map[string]string{"secret-token": "agent-a"}, readyDS{fakeDS: &fakeDS{info: readyCluster()}},
		WithACL(ACL{"agent-a": {"uhadoop-test": {}}}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "secret-token") {
		t.Fatalf("token leaked in readyz: %s", rec.Body)
	}
}
