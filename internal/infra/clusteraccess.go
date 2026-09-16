package infra

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ClusterAccess resolves a UHadoop instance ID to its cluster metadata and
// node credentials. Endpoints come from ZooKeeper (UHADOOPAccessHTTP); the
// actual data comes from the access HTTP action API. Nothing here talks to
// uhadoop-manage directly.
type ClusterAccess struct {
	Resolver *ServiceResolver
	Timeout  time.Duration
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*cachedCluster
}

type cachedCluster struct {
	info   *ClusterInfo
	expiry time.Time
}

// NewClusterAccess builds a ClusterAccess over a UHADOOPAccessHTTP resolver.
func NewClusterAccess(resolver *ServiceResolver) *ClusterAccess {
	return &ClusterAccess{
		Resolver: resolver,
		Timeout:  15 * time.Second,
		CacheTTL: 30 * time.Second,
		cache:    map[string]*cachedCluster{},
	}
}

func (a *ClusterAccess) client(ctx context.Context) (*AccessClient, error) {
	if a.Resolver == nil {
		return nil, fmt.Errorf("no access endpoint resolver configured")
	}
	ep, err := a.Resolver.Pick(ctx)
	if err != nil {
		return nil, err
	}
	return NewAccessClient(ep.URL()), nil
}

// DescribeCluster returns cluster metadata (cached briefly to avoid hammering
// the access API on multi-node fan-out calls).
func (a *ClusterAccess) DescribeCluster(ctx context.Context, instanceID string) (*ClusterInfo, error) {
	if a.CacheTTL > 0 {
		a.mu.Lock()
		if c, ok := a.cache[instanceID]; ok && time.Now().Before(c.expiry) {
			info := c.info
			a.mu.Unlock()
			return info, nil
		}
		a.mu.Unlock()
	}

	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	info, err := client.DescribeCluster(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if info.InstanceID == "" {
		info.InstanceID = instanceID
	}
	if a.CacheTTL > 0 {
		a.mu.Lock()
		a.cache[instanceID] = &cachedCluster{info: info, expiry: time.Now().Add(a.CacheTTL)}
		a.mu.Unlock()
	}
	return info, nil
}

// ListNode returns the cluster's nodes, optionally filtered by node state.
func (a *ClusterAccess) ListNode(ctx context.Context, instanceID string, nodeState int) ([]NodeInfo, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListNode(ctx, instanceID, nodeState)
}

// DescribeNode returns one node by node id.
func (a *ClusterAccess) DescribeNode(ctx context.Context, nodeID string) (*NodeInfo, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.DescribeNode(ctx, nodeID)
}

// ClusterApps returns each installed application with its per-node services.
func (a *ClusterAccess) ClusterApps(ctx context.Context, instanceID string) ([]NodeApp, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListClusterApps(ctx, instanceID)
}

// ResolveNode finds a node within a cluster by id, name, IP, or role-style
// reference ("master1", "master", "core2", "core", "task", "client"). The
// cluster is fetched (and cached) as needed.
func (a *ClusterAccess) ResolveNode(ctx context.Context, instanceID, ref string) (*NodeInfo, error) {
	info, err := a.DescribeCluster(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	node, err := FindNode(info.NodeInfo, ref)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// CredentialFor returns an SSH credential for a node, borrowing the password
// from any other node in the cluster when this one did not carry it (the API
// typically only populates access_user/passwd on the first node).
func (a *ClusterAccess) CredentialFor(ctx context.Context, instanceID string, node *NodeInfo) (Credential, error) {
	if cred, err := CredentialFor(*node); err == nil {
		return cred, nil
	}
	info, err := a.DescribeCluster(ctx, instanceID)
	if err != nil {
		return Credential{}, err
	}
	for i := range info.NodeInfo {
		if cred, err := CredentialFor(info.NodeInfo[i]); err == nil {
			return cred, nil
		}
	}
	return Credential{}, fmt.Errorf("cluster %s exposes no SSH credential for node %s", instanceID, node.NodeName)
}

// FindNode resolves a node reference against a node list.
func FindNode(nodes []NodeInfo, ref string) (*NodeInfo, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("cluster reports no nodes")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		// Default to the first master, else the first node.
		for i := range nodes {
			if nodes[i].NodeType == "master" {
				return &nodes[i], nil
			}
		}
		return &nodes[0], nil
	}
	lower := strings.ToLower(ref)
	// Exact matches first.
	for i := range nodes {
		n := &nodes[i]
		if n.NodeID == ref || n.NodeName == ref || n.NodeIP == ref {
			return n, nil
		}
	}
	// Role/style matches: exact node_type, or a role with a trailing index
	// ("master1", "core3") mapping onto the Nth node of that role.
	for i := range nodes {
		if strings.ToLower(nodes[i].NodeType) == lower {
			return &nodes[i], nil
		}
	}
	role, idx := splitRoleIndex(lower)
	if role != "" {
		matched := 0
		for i := range nodes {
			n := &nodes[i]
			if strings.ToLower(n.NodeType) != role {
				continue
			}
			matched++
			if idx == 0 || matched == idx {
				return n, nil
			}
		}
	}
	// Suffix match on the node name ("...-core1").
	for i := range nodes {
		if strings.HasSuffix(strings.ToLower(nodes[i].NodeName), "-"+lower) {
			return &nodes[i], nil
		}
	}
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].NodeName)
	}
	return nil, fmt.Errorf("node %q not found in cluster (known nodes: %s)", ref, strings.Join(names, ", "))
}

// splitRoleIndex splits "core3" into ("core", 3); "core" into ("core", 0).
func splitRoleIndex(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, 0
	}
	idx := 0
	for _, r := range s[i:] {
		idx = idx*10 + int(r-'0')
	}
	return s[:i], idx
}
