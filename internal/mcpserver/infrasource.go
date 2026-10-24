package mcpserver

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Admaing/diagflow/internal/infra"
)

// InfraSource is the production DataSource: cluster metadata via the UHadoop
// access HTTP action API (discovered through ZooKeeper), node operations via
// read-only SSH using the per-node credential that API hands out.
type InfraSource struct {
	Access    *infra.ClusterAccess
	Exec      infra.RemoteExec
	OpTimeout time.Duration
}

// NewInfraSource wires an InfraSource onto a resolved cluster accessor and a
// remote executor.
func NewInfraSource(access *infra.ClusterAccess, exec infra.RemoteExec) *InfraSource {
	return &InfraSource{Access: access, Exec: exec, OpTimeout: 40 * time.Second}
}

// Verify compile-time interface satisfaction.
var _ DataSource = (*InfraSource)(nil)

// ClusterInfo returns cluster metadata.
func (s *InfraSource) ClusterInfo(ctx context.Context, clusterID string) (*infra.ClusterInfo, error) {
	return s.Access.DescribeCluster(ctx, clusterID)
}

// Ready verifies the gateway's upstream is reachable: ZooKeeper resolves the
// zone's UHADOOPAccessHTTP instance and that instance's HTTP port accepts a
// connection. A process that started but can reach neither is not ready to
// serve tool calls.
//
// The check deliberately stops at the TCP layer. Probing a specific action
// (ping_http) would make readiness depend on the deployed access-plane
// version: an older binary without that action would keep every MCP pod
// NotReady and take the whole gateway out of service.
func (s *InfraSource) Ready(ctx context.Context) error {
	if s.Access == nil || s.Access.Resolver == nil {
		return fmt.Errorf("no cluster access configured (set DIAGFLOW_ZK_HOSTS)")
	}
	ep, err := s.Access.Resolver.Pick(ctx)
	if err != nil {
		return fmt.Errorf("discover UHADOOPAccessHTTP: %w", err)
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", ep.Addr())
	if err != nil {
		return fmt.Errorf("access API %s unreachable: %w", ep.Addr(), err)
	}
	_ = conn.Close()
	return nil
}

// Nodes returns the cluster's nodes.
func (s *InfraSource) Nodes(ctx context.Context, clusterID string) ([]infra.NodeInfo, error) {
	return s.Access.ListNode(ctx, clusterID, 0)
}

// Apps returns installed applications with their per-node services.
func (s *InfraSource) Apps(ctx context.Context, clusterID string) ([]infra.NodeApp, error) {
	return s.Access.ClusterApps(ctx, clusterID)
}

// Node resolves a node reference within a cluster. As a second step it asks
// the access API for the node's detail record, which is the only place the
// per-node service list (NameNode, ResourceManager, ...) is exposed; if that
// lookup fails, the coarser cluster-level record is returned.
func (s *InfraSource) Node(ctx context.Context, clusterID, ref string) (*infra.NodeInfo, error) {
	node, err := s.Access.ResolveNode(ctx, clusterID, ref)
	if err != nil {
		return nil, err
	}
	if node.NodeID != "" {
		if detail, err := s.Access.DescribeNode(ctx, node.NodeID); err == nil && detail != nil {
			if len(detail.ServiceName) > 0 {
				node.ServiceName = detail.ServiceName
			}
			if detail.AccessUser != "" {
				node.AccessUser = detail.AccessUser
			}
			if detail.Passwd != "" {
				node.Passwd = detail.Passwd
			}
		}
	}
	return node, nil
}

// Run executes a read-only command on a node over SSH.
func (s *InfraSource) Run(ctx context.Context, clusterID string, node *infra.NodeInfo, command string) (string, error) {
	cred, err := s.Access.CredentialFor(ctx, clusterID, node)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout())
	defer cancel()
	host := nodeHost(node)
	out, err := s.Exec.Run(ctx, host, cred, command)
	if err != nil {
		return out, fmt.Errorf("node %s (%s): %w", node.NodeName, host, err)
	}
	return out, nil
}

// Log fetches log lines from a node over SSH.
func (s *InfraSource) Log(ctx context.Context, clusterID string, node *infra.NodeInfo, logPath, keywords string, maxLines int) (string, error) {
	cred, err := s.Access.CredentialFor(ctx, clusterID, node)
	if err != nil {
		return "", err
	}
	cmd, err := infra.BuildLogCommand(logPath, keywords, maxLines)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout())
	defer cancel()
	host := nodeHost(node)
	out, err := s.Exec.Run(ctx, host, cred, cmd)
	if err != nil {
		return out, fmt.Errorf("node %s (%s) %s: %w", node.NodeName, host, logPath, err)
	}
	if strings.Contains(out, infra.NoFileMarker) {
		return "", fmt.Errorf("node %s: log file %s does not exist", node.NodeName, logPath)
	}
	// Empty output is a valid result: the file has no matching lines.
	// Cap over-long lines so one Java classpath banner cannot crowd out the
	// actual log entries.
	return infra.CapLineWidth(out, infra.MaxLogLineChars), nil
}

// nodeHost prefers the node's IPv6 address: the cluster's IPv4 addresses live
// on a network the cluster's own VPC does not route from Kubernetes, while the
// IPv6 addresses are reachable. Falls back to IPv4 when IPv6 is absent.
func nodeHost(node *infra.NodeInfo) string {
	if node.NodeIPv6 != "" {
		return node.NodeIPv6
	}
	return node.NodeIP
}

func (s *InfraSource) opTimeout() time.Duration {
	if s.OpTimeout <= 0 {
		return 40 * time.Second
	}
	return s.OpTimeout
}
