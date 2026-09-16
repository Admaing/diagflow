package v3tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Admaing/diagflow/internal/cluster"
	"github.com/Admaing/diagflow/internal/mcpclient"
	"github.com/Admaing/diagflow/internal/tools"
)

// This file holds the tools that are served by the cluster MCP gateway. Every
// one of them degrades to an explicit message when no gateway is configured
// (demo mode), so the agent never silently fabricates evidence.

const noGateway = "cluster MCP gateway is not configured (set DIAGFLOW_MCP_BASE_URL); " +
	"this tool needs live cluster access"

// callGateway invokes an MCP tool, resolving the cluster id from the request
// context. Callers should have checked mcp != nil.
func callGateway(ctx context.Context, mcp *mcpclient.Client, c cluster.Cluster, name string, args map[string]any) tools.ToolResult {
	clusterID := clusterIDFrom(c)
	if clusterID == "" {
		return tools.Err(name + ": cluster_id unavailable for MCP call")
	}
	args["cluster_id"] = clusterID
	out, err := mcp.CallTool(ctx, name, args)
	if err != nil {
		return tools.Err(fmt.Sprintf("%s via MCP failed: %v", name, err))
	}
	return tools.OK(clip(out, 30000))
}

// nodeLogViaMCP reads one node's log through the gateway.
func nodeLogViaMCP(ctx context.Context, mcp *mcpclient.Client, c cluster.Cluster, nodeName, logPath, keywords string, maxLines int) tools.ToolResult {
	args := map[string]any{"log_path": logPath, "max_lines": maxLines}
	if nodeName != "" {
		args["node_ref"] = nodeName
	}
	if keywords != "" {
		args["keywords"] = keywords
	}
	return callGateway(ctx, mcp, c, "get_node_log", args)
}

// sshExecViaMCP runs one read-only command through the gateway, which owns the
// command allowlist.
func sshExecViaMCP(ctx context.Context, mcp *mcpclient.Client, c cluster.Cluster, nodeName, cmd string) tools.ToolResult {
	args := map[string]any{"command": cmd}
	if nodeName != "" {
		args["node_ref"] = nodeName
	}
	return callGateway(ctx, mcp, c, "ssh_exec", args)
}

// ---------------------------------------------------------------------------
// Cluster topology tools
// ---------------------------------------------------------------------------

func clusterInfoTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name: "get_cluster_info",
		Description: "Get the cluster's overall state, capacity and installed services. " +
			"Call this first to learn the cluster's shape and whether it is healthy.",
		InputSchema: tools.Object(map[string]any{}, nil),
		Handler: func(ctx context.Context, _ map[string]any) (tools.ToolResult, error) {
			if mcp == nil {
				return tools.OK(noGateway), nil
			}
			return callGateway(ctx, mcp, c, "get_cluster_info", map[string]any{}), nil
		},
	}
}

func listNodesTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name: "list_nodes",
		Description: "List cluster nodes (id, name, role, IP, state) and the services each runs. " +
			"Use it to find which node hosts a component before reading its logs.",
		InputSchema: tools.Object(map[string]any{
			"role":  tools.Prop("Filter by role: master, core, task, client (optional)"),
			"state": tools.IntProp("Filter by numeric node state, e.g. 1 = running (optional)"),
		}, nil),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			if mcp == nil {
				return tools.OK(noGateway), nil
			}
			callArgs := map[string]any{}
			if role := tools.ArgStr(args, "role", ""); role != "" {
				callArgs["role"] = role
			}
			if st, ok := args["state"].(float64); ok && st > 0 {
				callArgs["state"] = int(st)
			}
			return callGateway(ctx, mcp, c, "list_nodes", callArgs), nil
		},
	}
}

// ---------------------------------------------------------------------------
// Log discovery and cross-node search
// ---------------------------------------------------------------------------

func listLogsTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name: "list_logs",
		Description: "List the log files on a node so you can discover exact paths before reading them. " +
			"Without 'dir' it probes the standard UHadoop log roots (hdfs, yarn, hive, spark, ...).",
		InputSchema: tools.Object(map[string]any{
			"node_name": tools.Prop("Node: 'master1', 'core1', or full node_name (default: first master)"),
			"dir":       tools.Prop("A directory under /var/log to list (optional)"),
		}, nil),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			if mcp == nil {
				return tools.OK(noGateway), nil
			}
			callArgs := map[string]any{}
			if n := tools.ArgStr(args, "node_name", ""); n != "" {
				callArgs["node_ref"] = n
			}
			if d := tools.ArgStr(args, "dir", ""); d != "" {
				callArgs["dir"] = d
			}
			return callGateway(ctx, mcp, c, "list_logs", callArgs), nil
		},
	}
}

func searchLogsTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name: "search_logs",
		Description: "Search the same log path across multiple nodes in parallel. " +
			"Use it to tell a single-node fault from a cluster-wide one.",
		InputSchema: tools.Object(map[string]any{
			"log_path":  tools.Prop("Absolute log path present on the target nodes"),
			"keywords":  tools.Prop("Keyword/regex filter (required), e.g. 'ERROR|Exception'"),
			"node_name": tools.Prop("Comma-separated node refs to search (optional; overrides roles)"),
			"roles":     tools.Prop("Comma-separated roles to search, e.g. 'master,core' (optional)"),
			"max_lines": tools.IntProp("Max lines per node (default 200)"),
		}, []string{"log_path", "keywords"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			if mcp == nil {
				return tools.OK(noGateway), nil
			}
			logPath := tools.ArgStr(args, "log_path", "")
			keywords := tools.ArgStr(args, "keywords", "")
			if strings.TrimSpace(logPath) == "" || strings.TrimSpace(keywords) == "" {
				return tools.Err("search_logs: log_path and keywords are required"), nil
			}
			callArgs := map[string]any{"log_path": logPath, "keywords": keywords}
			if n := tools.ArgStr(args, "node_name", ""); n != "" {
				callArgs["nodes"] = n
			}
			if r := tools.ArgStr(args, "roles", ""); r != "" {
				callArgs["roles"] = r
			}
			if n, ok := args["max_lines"].(float64); ok && n > 0 {
				callArgs["max_lines"] = int(n)
			}
			return callGateway(ctx, mcp, c, "search_logs", callArgs), nil
		},
	}
}
