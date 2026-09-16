package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCallToolOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"log line 1"},{"type":"text","text":"log line 2"}]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "agent-tok")
	out, err := c.CallTool(context.Background(), "get_logs", map[string]any{"cluster_id": "c1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != "log line 1\nlog line 2" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestCallToolRPCErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"log_path is required"}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").CallTool(context.Background(), "get_logs", nil)
	if err == nil || !strings.Contains(err.Error(), "-32602") {
		t.Fatalf("expected rpc error, got %v", err)
	}
}

func TestCallToolHTTPStatusAndBaseURLNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(strings.TrimSuffix(srv.URL, "/mcp"), "") // no /mcp suffix → appended
	c.Timeout = 2 * time.Second
	_, err := c.CallTool(context.Background(), "get_logs", nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected HTTP 502 error, got %v", err)
	}
}

func TestExtractJSONPayloadPlainJSON(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	if got := extractJSONPayload(body); string(got) != string(body) {
		t.Fatalf("plain json not passed through: %s", got)
	}
}

func TestExtractJSONPayloadSSE(t *testing.T) {
	body := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n")
	got := extractJSONPayload(body)
	if !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("sse payload not extracted: %s", got)
	}
}

// DeepWiki (and Streamable-HTTP MCP servers generally) answer with SSE.
func TestCallToolParsesSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"known issue\"}]}}\n\n"))
	}))
	defer srv.Close()

	out, err := New(srv.URL, "").CallTool(context.Background(), "ask_question", map[string]any{"repoName": "apache/flink"})
	if err != nil {
		t.Fatalf("sse call failed: %v", err)
	}
	if out != "known issue" {
		t.Fatalf("unexpected sse output: %q", out)
	}
}

// TestLiveDeepWiki exercises the real DeepWiki MCP server. Opt-in:
//
//	DIAGFLOW_DEEPWIKI_LIVE=1 go test ./internal/mcpclient/ -run LiveDeepWiki -v
func TestLiveDeepWiki(t *testing.T) {
	if os.Getenv("DIAGFLOW_DEEPWIKI_LIVE") != "1" {
		t.Skip("set DIAGFLOW_DEEPWIKI_LIVE=1 to hit the real DeepWiki MCP server")
	}
	c := New("https://mcp.deepwiki.com/mcp", "")
	c.Timeout = 60 * time.Second
	out, err := c.CallTool(context.Background(), "ask_question", map[string]any{
		"repoName": "apache/flink",
		"question": "Is CheckpointExpiredException a known issue?",
	})
	if err != nil {
		t.Fatalf("live deepwiki call failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "checkpoint") {
		t.Fatalf("unexpected live answer: %.300s", out)
	}
	t.Logf("live answer (first 200 chars): %.200s", out)
}
