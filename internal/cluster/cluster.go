// Package cluster defines the common interface implemented by both the
// simulated and real cluster adapters, so the tool layer is data-source
// agnostic.
package cluster

import "context"

// Cluster is the interface the tool layer depends on.
type Cluster interface {
	// EnsureNodeData lazily loads node metadata (no-op for simulated).
	EnsureNodeData(ctx context.Context) error
	// FindNode resolves a node reference to a node entry.
	FindNode(ref string) map[string]any
	// GetNodeLog fetches a node log (simulated reads memory; real calls umrAgent).
	GetNodeLog(ctx context.Context, logPath, keywords string, maxLines int) (string, error)
	// GetConfig fetches a config file.
	GetConfig(ctx context.Context, configPath string) (string, error)
	// GetMetrics fetches metrics key=value lines.
	GetMetrics(ctx context.Context, metricNames []string) (string, error)
	// Context returns the diagnosis context map.
	Context() map[string]any
	// ExpectedRootCause returns the scenario's expected root cause (simulated only).
	ExpectedRootCause() string
}
