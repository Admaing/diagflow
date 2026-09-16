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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

// RealCluster is the production cluster adapter (implements cluster.Cluster).
// Cluster metadata comes from the zone's UHadoop access API (resolved through
// ZooKeeper); node operations go over read-only SSH using the credential the
// access API returns. No uhadoop-manage URL is hard-coded anywhere.
type RealCluster struct {
	InstanceID string
	Region     string
	Zone       string
	Access     *ClusterAccess
	Exec       RemoteExec
	Logs       *LogReader

	clusterInfo *ClusterInfo
	nodeInfos   []NodeInfo
	context     map[string]any
}

// Verify compile-time interface satisfaction.
var _ cluster.Cluster = (*RealCluster)(nil)

// NewRealCluster builds a real cluster adapter over an access client and a
// remote executor (SSH today, umrAgent later).
func NewRealCluster(instanceID, region, zone string, access *ClusterAccess, exec RemoteExec) *RealCluster {
	if exec == nil {
		exec = NewSSHExec()
	}
	return &RealCluster{
		InstanceID: instanceID,
		Region:     region,
		Zone:       zone,
		Access:     access,
		Exec:       exec,
		Logs:       &LogReader{Exec: exec},
		context: map[string]any{
			"cluster_id": instanceID,
			"region":     region,
			"zone":       zone,
		},
	}
}

// EnsureNodeData lazily loads cluster + node metadata from the access API.
func (c *RealCluster) EnsureNodeData(ctx context.Context) error {
	if c.clusterInfo != nil {
		return nil
	}
	if c.Access == nil {
		return fmt.Errorf("no cluster access configured (set DIAGFLOW_ZK_HOSTS)")
	}
	info, err := c.Access.DescribeCluster(ctx, c.InstanceID)
	if err != nil {
		return err
	}
	c.clusterInfo = info
	c.nodeInfos = info.NodeInfo
	return nil
}

// ClusterInfo returns the loaded cluster metadata (nil before EnsureNodeData).
func (c *RealCluster) ClusterInfo() *ClusterInfo { return c.clusterInfo }

// FindNode resolves a node reference to a node entry. The returned map keeps
// both the new key names and the legacy ones used by older call sites.
func (c *RealCluster) FindNode(ref string) map[string]any {
	node, err := FindNode(c.nodeInfos, ref)
	if err != nil {
		return nil
	}
	return nodeToMap(*node)
}

// nodeToMap renders a NodeInfo as the map shape the tool layer expects.
func nodeToMap(n NodeInfo) map[string]any {
	m := map[string]any{
		"node_id":     n.NodeID,
		"node_name":   n.NodeName,
		"node_role":   n.NodeType,
		"node_type":   n.NodeType,
		"ip":          n.NodeIP,
		"node_ip":     n.NodeIP,
		"ipv6":        n.NodeIPv6,
		"node_ipv6":   n.NodeIPv6,
		"access_user": n.AccessUser,
		"passwd":      n.Passwd,
		"agent_key":   n.AgentKey,
		"node_state":  n.NodeState.Int(),
	}
	if len(n.ServiceName) > 0 {
		m["service_name"] = n.ServiceName
	}
	return m
}

// nodeHost returns the address to reach a node on: IPv6 when available (the
// cluster's IPv4 range is not routable from Kubernetes), else IPv4.
func nodeHost(n NodeInfo) string {
	if n.NodeIPv6 != "" {
		return n.NodeIPv6
	}
	return n.NodeIP
}

// resolveNodeMeta loads node data and resolves a reference.
func (c *RealCluster) resolveNodeMeta(ctx context.Context, ref string) (*NodeInfo, error) {
	if err := c.EnsureNodeData(ctx); err != nil {
		return nil, err
	}
	return FindNode(c.nodeInfos, ref)
}

// credentialFor returns the SSH credential for a node, borrowing the
// cluster-level password when the node record does not carry one.
func (c *RealCluster) credentialFor(ctx context.Context, node *NodeInfo) (Credential, error) {
	if cred, err := CredentialFor(*node); err == nil {
		return cred, nil
	}
	if c.Access == nil {
		return Credential{}, fmt.Errorf("no cluster access configured")
	}
	return c.Access.CredentialFor(ctx, c.InstanceID, node)
}

// GetNodeLog fetches a node log over read-only SSH. logPath may carry a node
// prefix ("master1:/var/log/x.log"); otherwise the first master is used.
func (c *RealCluster) GetNodeLog(ctx context.Context, logPath, keywords string, maxLines int) (string, error) {
	ref, filename, ok := strings.Cut(logPath, ":")
	if !ok || !strings.HasPrefix(filename, "/") {
		ref, filename = "", logPath
	}
	node, err := c.resolveNodeMeta(ctx, ref)
	if err != nil {
		return "", err
	}
	if c.Logs == nil {
		return "", fmt.Errorf("no node log transport configured")
	}
	cred, err := c.credentialFor(ctx, node)
	if err != nil {
		return "", err
	}
	return c.Logs.ReadLog(ctx, nodeHost(*node), cred, filename, keywords, maxLines)
}

// GetConfig reads a file from the cluster's first master over read-only SSH.
func (c *RealCluster) GetConfig(ctx context.Context, configPath string) (string, error) {
	if !configPathRe.MatchString(configPath) || hasDotDotSegment(configPath) {
		return "", fmt.Errorf("unsafe config path %q", configPath)
	}
	node, err := c.resolveNodeMeta(ctx, "")
	if err != nil {
		return "", err
	}
	cred, err := c.credentialFor(ctx, node)
	if err != nil {
		return "", err
	}
	return c.Exec.Run(ctx, nodeHost(*node), cred, "cat -- "+shellQuote(configPath))
}

// GetMetrics fetches a resource snapshot over read-only SSH.
func (c *RealCluster) GetMetrics(ctx context.Context, metricNames []string) (string, error) {
	node, err := c.resolveNodeMeta(ctx, "")
	if err != nil {
		return "", err
	}
	cred, err := c.credentialFor(ctx, node)
	if err != nil {
		return "", err
	}
	const cmd = "uptime; echo '--- load'; cat /proc/loadavg; echo '--- mem'; free -m; echo '--- disk'; df -h"
	return c.Exec.Run(ctx, nodeHost(*node), cred, cmd)
}

// Context returns the cluster context.
func (c *RealCluster) Context() map[string]any { return c.context }

// SupportsExec reports false: the local (non-MCP) exec path is not used for
// production clusters; node commands go through the MCP gateway instead.
func (c *RealCluster) SupportsExec() bool { return false }

// ExpectedRootCause is meaningless in production.
func (c *RealCluster) ExpectedRootCause() string { return "" }

// Summary renders a human-readable summary.
func (c *RealCluster) Summary() string {
	name := c.InstanceID
	if c.clusterInfo != nil && c.clusterInfo.InstanceName != "" {
		name = c.InstanceID + " (" + c.clusterInfo.InstanceName + ")"
	}
	state := "unknown"
	if c.clusterInfo != nil {
		state = ClusterStateName(c.clusterInfo.ClusterState.Int())
	}
	return fmt.Sprintf("RealCluster(instance_id=%s, state=%s, nodes=%d)", name, state, len(c.nodeInfos))
}

// configPathRe restricts config paths read by GetConfig.
var configPathRe = regexp.MustCompile(`^/[A-Za-z0-9._@/+-]*$`)

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
