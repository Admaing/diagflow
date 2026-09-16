// Package mcpclient is a minimal MCP client (Streamable HTTP, stateless)
// used by DiagFlow tools to call the node-log MCP server.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls an MCP server over HTTP.
type Client struct {
	BaseURL string // e.g. http://diagflow-mcp:8090/mcp
	Token   string // bearer token identifying this agent
	Timeout time.Duration
	client  *http.Client
}

// New builds a client (default timeout 30s).
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, Timeout: 30 * time.Second}
}

// CallTool invokes a tool and returns the concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	callArgs, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		return "", err
	}
	base := c.BaseURL
	if !strings.Contains(base, "/mcp") {
		base = strings.TrimSuffix(base, "/") + "/mcp"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(callArgs))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// Streamable-HTTP MCP servers (incl. DeepWiki) require the client to accept
	// both response shapes.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mcp HTTP %d: %s", resp.StatusCode, trunc(body))
	}

	payload := extractJSONPayload(body)
	if len(payload) == 0 {
		return "", fmt.Errorf("mcp: empty or unparseable response body")
	}

	var out struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("mcp decode: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("mcp error %d: %s", out.Error.Code, out.Error.Message)
	}
	var parts []string
	for _, c := range out.Result.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// extractJSONPayload returns the JSON-RPC object from a response body. Our
// node-log server replies with plain JSON; Streamable-HTTP MCP servers such as
// DeepWiki reply with Server-Sent Events ("event: message\ndata: {...}"), so
// both shapes must be accepted.
func extractJSONPayload(body []byte) []byte {
	if t := bytes.TrimSpace(body); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		return t
	}
	var last []byte
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		if d := bytes.TrimSpace([]byte(rest)); len(d) > 0 {
			last = d
		}
	}
	return last
}

func trunc(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}
