// Package simulated mirrors diagflow/simulated — a mock cluster that serves
// pre-generated scenario data for demo without real infrastructure.
package simulated

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/Admaing/diagflow/internal/cluster"
)

// Scenario is the data a scenario function returns.
type Scenario struct {
	Context            map[string]any
	Logs               map[string]string
	Config             map[string]map[string]any
	Metrics            map[string]any
	ExpectedRootCause  string
	ExpectedConfidence string
	ScenarioName       string
}

// Cluster implements cluster.Cluster over a Scenario.
type Cluster struct {
	Scenario *Scenario
}

// Verify compile-time interface satisfaction.
var _ cluster.Cluster = (*Cluster)(nil)

// EnsureNodeData is a no-op for simulated clusters.
func (c *Cluster) EnsureNodeData(ctx context.Context) error { return nil }

// FindNode resolves a node ref (always returns a stub entry for the demo).
func (c *Cluster) FindNode(ref string) map[string]any {
	return map[string]any{"node_name": ref, "node_role": ref}
}

// GetNodeLog reads matching lines from a simulated log file.
func (c *Cluster) GetNodeLog(ctx context.Context, logPath, keywords string, maxLines int) (string, error) {
	content, ok := c.Scenario.Logs[logPath]
	if !ok {
		return "[Simulated] Log file '" + logPath + "' not found on this cluster.", nil
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if keywords != "" {
		kw := strings.ToLower(keywords)
		var kept []string
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l), kw) {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	if len(lines) == 0 {
		return "[Simulated] No lines matching '" + keywords + "' in " + logPath, nil
	}
	return strings.Join(lines, "\n"), nil
}

// GetConfig returns a config file content as key=value lines.
func (c *Cluster) GetConfig(ctx context.Context, configPath string) (string, error) {
	data, ok := c.Scenario.Config[configPath]
	if !ok || len(data) == 0 {
		return "[Simulated] Config file '" + configPath + "' not found.", nil
	}
	var keys []string
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(toString(data[k]))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// GetMetrics returns metrics as key=value lines (optionally filtered).
func (c *Cluster) GetMetrics(ctx context.Context, metricNames []string) (string, error) {
	var keys []string
	for k := range c.Scenario.Metrics {
		if len(metricNames) > 0 && !contains(metricNames, k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(toString(c.Scenario.Metrics[k]))
		b.WriteString("\n")
	}
	if len(keys) == 0 {
		return "[Simulated] No metrics available.", nil
	}
	return b.String(), nil
}

// Context returns the scenario context.
func (c *Cluster) Context() map[string]any { return c.Scenario.Context }

// ExpectedRootCause returns the scenario's expected root cause.
func (c *Cluster) ExpectedRootCause() string { return c.Scenario.ExpectedRootCause }

// Summary renders a human-readable cluster summary.
func (c *Cluster) Summary() string {
	ctx := c.Scenario.Context
	return "Cluster: " + toString(ctx["cluster_id"]) + "\n" +
		"Region: " + toString(ctx["region"]) + "\n" +
		"Component: " + toString(ctx["component"]) + " v" + toString(ctx["version"]) + "\n" +
		"Problem: " + toString(firstNonEmpty(ctx["problem_desc"], ctx["problem"])) + "\n" +
		"Detail: " + toString(ctx["detail"])
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func firstNonEmpty(a, b any) any {
	if toString(a) != "" {
		return a
	}
	return b
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
