// Package infra mirrors diagflow/infra/__init__.py — the production
// infrastructure adapter. It bridges DiagFlow to real UHadoop infrastructure
// via umrAgent HTTP (HMAC-SHA1 signed, no SSH) and the uhadoop-manage HTTP API
// (no direct MySQL).
package infra

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Admaing/diagflow/internal/cluster"
)

// UmrAgentClient calls umrAgent on cluster nodes via HTTP (port 65431),
// HMAC-SHA1 signed. Faithful replica of util/agent.js + pkg/client/uagent.
type UmrAgentClient struct {
	Port    int
	Timeout time.Duration
	client  *http.Client
}

// NewUmrAgentClient builds a client with a default port 65431 and 10s timeout.
func NewUmrAgentClient() *UmrAgentClient {
	return &UmrAgentClient{
		Port:    65431,
		Timeout: 10 * time.Second,
	}
}

// Call invokes an umrAgent Action on a node over HTTP.
func (c *UmrAgentClient) Call(ctx context.Context, ipv6, agentKey, action string, params map[string]string) (string, error) {
	all := map[string]string{
		"Action": action,
		"Date":   fmt.Sprintf("%d", time.Now().UnixMilli()),
	}
	for k, v := range params {
		all[k] = v
	}

	signature := Sign(all, agentKey)
	all["Signature"] = signature

	var qs strings.Builder
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			qs.WriteString("&")
		}
		qs.WriteString(k)
		qs.WriteString("=")
		qs.WriteString(url.QueryEscape(all[k]))
	}

	u := fmt.Sprintf("http://[%s]:%d/?%s", ipv6, c.Port, qs.String())
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: c.Timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("umrAgent HTTP %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// Sign computes HMAC-SHA1 over sorted-key values — byte-identical to the
// Node.js util/agent.js auth() and Go pkg/client/uagent setGetSignature().
func Sign(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(params[k])
	}
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(b.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// NodeInfoClient fetches cluster/node metadata from uhadoop-manage HTTP API.
// NO direct MySQL.
type NodeInfoClient struct {
	BaseURL string
	Region  string
	client  *http.Client
}

// NewNodeInfoClient builds a node info client for an uhadoop-manage base URL.
func NewNodeInfoClient(baseURL, region string) *NodeInfoClient {
	return &NodeInfoClient{BaseURL: baseURL, Region: region, client: &http.Client{Timeout: 15 * time.Second}}
}

// DescribeCluster fetches cluster info + nodes in one call.
func (c *NodeInfoClient) DescribeCluster(ctx context.Context, instanceID string) (map[string]any, error) {
	payload := fmt.Sprintf(`{"Action":"describe_cluster_nodes","instance_id":%q,"region":%q}`,
		instanceID, c.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/",
		strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("uhadoop-manage HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse JSON into a map.
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if retCode, ok := data["RetCode"].(float64); ok && retCode != 0 {
		return nil, fmt.Errorf("uhadoop-manage error: %v", data["Error"])
	}
	return data, nil
}

// RealCluster is the production cluster adapter (implements cluster.Cluster).
type RealCluster struct {
	InstanceID string
	NodeClient *NodeInfoClient
	Agent      *UmrAgentClient

	clusterInfo map[string]any
	nodes       []map[string]any
	context     map[string]any
}

// Verify compile-time interface satisfaction.
var _ cluster.Cluster = (*RealCluster)(nil)

// NewRealCluster builds a real cluster adapter.
func NewRealCluster(instanceID string, nodeClient *NodeInfoClient, agent *UmrAgentClient) *RealCluster {
	if agent == nil {
		agent = NewUmrAgentClient()
	}
	return &RealCluster{
		InstanceID: instanceID,
		NodeClient: nodeClient,
		Agent:      agent,
		context:    map[string]any{"cluster_id": instanceID},
	}
}

// EnsureNodeData lazily loads node metadata from the Go API.
func (c *RealCluster) EnsureNodeData(ctx context.Context) error {
	if c.nodes != nil {
		return nil
	}
	if c.NodeClient == nil {
		return fmt.Errorf("no node client configured")
	}
	data, err := c.NodeClient.DescribeCluster(ctx, c.InstanceID)
	if err != nil {
		return err
	}
	c.clusterInfo, _ = data["cluster_info"].(map[string]any)
	if rawNodes, ok := data["nodes"].([]any); ok {
		for _, n := range rawNodes {
			if m, ok := n.(map[string]any); ok {
				c.nodes = append(c.nodes, m)
			}
		}
	}
	return nil
}

// FindNode resolves a node reference to a node entry.
func (c *RealCluster) FindNode(ref string) map[string]any {
	for _, n := range c.nodes {
		if strOr(n["node_name"], "") == ref || strOr(n["node_role"], "") == ref {
			return n
		}
	}
	if len(c.nodes) > 0 {
		return c.nodes[0]
	}
	return nil
}

// GetNodeLog fetches a node log via umrAgent (no SSH).
func (c *RealCluster) GetNodeLog(ctx context.Context, logPath, keywords string, maxLines int) (string, error) {
	if err := c.EnsureNodeData(ctx); err != nil {
		return "", err
	}
	nodeRef, filename, _ := strings.Cut(logPath, ":")
	if filename == "" {
		filename = logPath
	}
	node := c.FindNode(nodeRef)
	if node == nil {
		return "", fmt.Errorf("node '%s' not found", nodeRef)
	}
	ipv6 := strOr(node["ipv6"], "")
	agentKey := agentKeyOf(node)
	return c.Agent.Call(ctx, ipv6, agentKey, "GetLogs", map[string]string{
		"Path":     filename,
		"Keywords": keywords,
		"MaxLines": itoa(maxLines),
	})
}

// GetConfig fetches a config file via umrAgent (uses agent_key or umr_agent_key).
func (c *RealCluster) GetConfig(ctx context.Context, configPath string) (string, error) {
	if err := c.EnsureNodeData(ctx); err != nil {
		return "", err
	}
	master := c.FindNode("master1")
	if master == nil {
		return "", fmt.Errorf("no master node in cluster %s", c.InstanceID)
	}
	return c.Agent.Call(ctx, strOr(master["ipv6"], ""), agentKeyOf(master), "GetLogs", map[string]string{
		"Path": configPath,
	})
}

// GetMetrics fetches metrics via monitor (production placeholder).
func (c *RealCluster) GetMetrics(ctx context.Context, metricNames []string) (string, error) {
	return "[Production] Metrics require service discovery configured", nil
}

// Context returns the cluster context.
func (c *RealCluster) Context() map[string]any { return c.context }

// ExpectedRootCause is meaningless in production.
func (c *RealCluster) ExpectedRootCause() string { return "" }

// Summary renders a human-readable summary.
func (c *RealCluster) Summary() string {
	return fmt.Sprintf("RealCluster(instance_id=%s, nodes=%d)", c.InstanceID, len(c.nodes))
}

// agentKeyOf applies the same compat lookup as get_node_log: either agent_key
// or umr_agent_key.
func agentKeyOf(node map[string]any) string {
	if k := strOr(node["agent_key"], ""); k != "" {
		return k
	}
	return strOr(node["umr_agent_key"], "")
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
