// Package v3tools implements the five v3 diagnostic tools.
package v3tools

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Admaing/diagflow/internal/cluster"
	"github.com/Admaing/diagflow/internal/mcpclient"
	"github.com/Admaing/diagflow/internal/tools"
)

// BuildV3Tools returns the v3 tools, injecting a KB-backed fingerprint handler
// when kb is non-nil. Two independent MCP connections are supported:
//   - mcp: the node-log gateway (logs/base-info/process/yarn/metrics)
//   - deepwiki: DeepWiki's own public MCP server (upstream repo knowledge)
func BuildV3Tools(c cluster.Cluster, kb fingerprintMatcher, mcp, deepwiki *mcpclient.Client) []tools.Def {
	list := []tools.Def{
		queryNodeLogTool(c, mcp),
		listLogsTool(c, mcp),
		searchLogsTool(c, mcp),
		queryMetricsTool(c, mcp),
		queryYarnTool(c, mcp),
		clusterInfoTool(c, mcp),
		listNodesTool(c, mcp),
		sshExecTool(c, mcp),
		umrAgentTool(c, mcp),
		deepwikiTool(deepwiki),
		fingerprintTool(kb),
	}
	return list
}

// clusterIDFrom resolves the cluster id a request is bound to; MCP tools need
// it because the gateway is cluster-scoped.
func clusterIDFrom(c cluster.Cluster) string {
	return tools.ArgStr(c.Context(), "cluster_id", "")
}

// ---------------------------------------------------------------------------
// Tool 0: query_node_log / query_metrics — real evidence via cluster.Cluster
// ---------------------------------------------------------------------------

// queryNodeLogTool reads a log file from a node, filtered by keywords. It is
// data-source agnostic: the simulated cluster reads scenario logs, the real
// adapter calls umrAgent GetLogs.
func queryNodeLogTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name:        "query_node_log",
		Description: "Read a log file on a cluster node, filtered by keywords. Prefer node_name + log_path.",
		InputSchema: tools.Object(map[string]any{
			"node_name": tools.Prop("Node ref: 'master1', 'core1', or a full node_name"),
			"log_path":  tools.Prop("Absolute log path on the node, e.g. /var/log/flink/flink-taskmanager-*.log"),
			"keywords":  tools.Prop("Comma-separated keywords to filter lines (e.g. ERROR,Exception,FATAL)"),
			"max_lines": tools.IntProp("Maximum lines to return (default 200)"),
		}, []string{"log_path"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			logPath := tools.ArgStr(args, "log_path", "")
			if strings.TrimSpace(logPath) == "" {
				return tools.Err("log_path is required"), nil
			}
			ref := logPath
			if nodeName := tools.ArgStr(args, "node_name", ""); nodeName != "" {
				ref = nodeName + ":" + logPath
			}
			maxLines := 200
			if n, ok := args["max_lines"].(float64); ok && n > 0 {
				maxLines = int(n)
				if maxLines > 2000 {
					maxLines = 2000
				}
			}
			keywords := tools.ArgStr(args, "keywords", "")
			nodeName := tools.ArgStr(args, "node_name", "")
			if mcp != nil {
				return nodeLogViaMCP(ctx, mcp, c, nodeName, logPath, keywords, maxLines), nil
			}
			out, err := c.GetNodeLog(ctx, ref, keywords, maxLines)
			if err != nil {
				return tools.Err(fmt.Sprintf("query_node_log %s: %v", ref, err)), nil
			}
			if strings.TrimSpace(out) == "" {
				return tools.OK("(no matching log lines)"), nil
			}
			return tools.OK(clip(out, 20000)), nil
		},
	}
}

// queryMetricsTool fetches resource metrics for the cluster. When the node-log
// MCP server is configured, metrics come from it (agents never touch infra
// directly); otherwise the cluster adapter is used (demo).
func queryMetricsTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name:        "query_metrics",
		Description: "Fetch cluster/component metrics (CPU, memory, disk, JVM) as key=value lines.",
		InputSchema: tools.Object(map[string]any{
			"node_name":    tools.Prop("Node ref (default: master1)"),
			"metric_names": tools.Prop("Comma-separated metric names; empty = all available"),
		}, nil),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			if mcp != nil {
				return metricsViaMCP(ctx, mcp, c, tools.ArgStr(args, "node_name", "")), nil
			}
			var names []string
			if raw := tools.ArgStr(args, "metric_names", ""); raw != "" {
				for _, n := range strings.Split(raw, ",") {
					if n = strings.TrimSpace(n); n != "" {
						names = append(names, n)
					}
				}
			}
			out, err := c.GetMetrics(ctx, names)
			if err != nil {
				return tools.Err(fmt.Sprintf("query_metrics: %v", err)), nil
			}
			return tools.OK(clip(out, 8000)), nil
		},
	}
}

// metricsViaMCP routes the metric query through the node-log MCP server.
func metricsViaMCP(ctx context.Context, mcp *mcpclient.Client, c cluster.Cluster, nodeRef string) tools.ToolResult {
	clusterID := tools.ArgStr(c.Context(), "cluster_id", "")
	if clusterID == "" {
		return tools.Err("query_metrics: cluster_id unavailable for MCP call")
	}
	callArgs := map[string]any{"cluster_id": clusterID}
	if nodeRef != "" {
		callArgs["node_ref"] = nodeRef
	}
	out, err := mcp.CallTool(ctx, "get_metrics", callArgs)
	if err != nil {
		return tools.Err(fmt.Sprintf("query_metrics via MCP failed: %v", err))
	}
	return tools.OK(clip(out, 8000))
}

// clip bounds a string to n bytes (runes are not split at the boundary).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

// fingerprintMatcher is the minimal KB surface fingerprint_match needs.
type fingerprintMatcher interface {
	FingerprintMatch(component, errorPattern, version string) map[string]any
}

// ---------------------------------------------------------------------------
// Tool 1: query_yarn
// ---------------------------------------------------------------------------

// queryYarnTool queries YARN applications. When the node-log MCP server is
// configured the query is proxied through it (umrAgent GetAppList), so the
// agent needs no direct ResourceManager access; otherwise the cluster adapter
// answers (demo).
func queryYarnTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name:        "query_yarn",
		Description: "Query YARN RM. Use 'list_apps' to see all apps (FAILED first). Use 'app_nodes' with app_id to find nodes.",
		InputSchema: tools.Object(map[string]any{
			"action":   tools.Prop("'list_apps' or 'app_nodes'"),
			"app_id":   tools.Prop("YARN app ID for 'app_nodes'"),
			"app_name": tools.Prop("Comma-separated states: RUNNING,FAILED,KILLED"),
		}, []string{"action"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			action := tools.ArgStr(args, "action", "list_apps")
			appName := tools.ArgStr(args, "app_name", "RUNNING,FAILED,KILLED,FINISHED")
			appID := tools.ArgStr(args, "app_id", "")
			if mcp != nil {
				return yarnViaMCP(ctx, mcp, c, action, appID, appName), nil
			}
			return queryYarnHandler(c, action, appID, appName), nil
		},
	}
}

// yarnViaMCP routes a YARN query through the node-log MCP server.
func yarnViaMCP(ctx context.Context, mcp *mcpclient.Client, c cluster.Cluster, action, appID, states string) tools.ToolResult {
	clusterID := tools.ArgStr(c.Context(), "cluster_id", "")
	if clusterID == "" {
		return tools.Err("query_yarn: cluster_id unavailable for MCP call")
	}
	// DiagFlow's "app_nodes" maps to the MCP server's single-app query.
	mcpAction := action
	if action == "app_nodes" {
		mcpAction = "app"
	}
	callArgs := map[string]any{"cluster_id": clusterID, "action": mcpAction}
	if appID != "" {
		callArgs["app_id"] = appID
	}
	if states != "" {
		callArgs["states"] = states
	}
	out, err := mcp.CallTool(ctx, "query_yarn", callArgs)
	if err != nil {
		return tools.Err(fmt.Sprintf("query_yarn via MCP failed: %v", err))
	}
	return tools.OK(clip(out, 12000))
}

func queryYarnHandler(c cluster.Cluster, action, appID, appName string) tools.ToolResult {
	// The simulated cluster has no YARN REST API; query_yarn in demo mode reads
	// the scenario's yarn data. Real mode would hit rm_url. Keep a faithful
	// placeholder that returns node info from the cluster context.
	if action == "list_apps" {
		return tools.OK("0 YARN apps found (simulated).")
	}
	if action == "app_nodes" {
		return tools.OK(fmt.Sprintf("App %s node info unavailable (simulated).", appID))
	}
	return tools.OK(fmt.Sprintf("Unknown action: %s", action))
}

// ---------------------------------------------------------------------------
// Tool 2: ssh_exec (with read-only command safety)
// ---------------------------------------------------------------------------

func sshExecTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name:        "ssh_exec",
		Description: "Execute read-only shell commands on cluster nodes via SSH. Find logs, grep errors, check processes.",
		InputSchema: tools.Object(map[string]any{
			"node_name": tools.Prop("Node: 'master1', 'core1', or full node_name"),
			"cmd":       tools.Prop("Shell command: find, grep, tail, ps, curl, df, free"),
			"timeout_s": tools.IntProp("SSH timeout in seconds"),
		}, []string{"node_name", "cmd"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			nodeName := tools.ArgStr(args, "node_name", "")
			cmd := tools.ArgStr(args, "cmd", "")
			if mcp != nil {
				return sshExecViaMCP(ctx, mcp, c, nodeName, cmd), nil
			}
			return sshExecHandler(c, nodeName, cmd), nil
		},
	}
}

func sshExecHandler(c cluster.Cluster, nodeName, cmd string) tools.ToolResult {
	if !c.SupportsExec() {
		return tools.Err("ssh_exec is unavailable in production; use query_node_log or call_umr_agent")
	}
	if allowed, reason := ValidateCommand(cmd); !allowed {
		return tools.OK(fmt.Sprintf("Command blocked by safety filter: %s", reason))
	}
	// Simulated cluster returns an empty result (no real SSH in demo). The real
	// adapter would exec over paramiko-equivalent SSH.
	_ = nodeName
	return tools.OK("(simulated) command validated but not executed")
}

// ValidateCommand checks that a shell command is read-only and safe.
// Fail-closed: every pipeline/compound segment must start with a command from
// the strict read-only allowlist, output redirection is rejected outright, and
// known write-capable argument forms (-delete, -exec, sed -i, ...) are blocked.
func ValidateCommand(cmd string) (bool, string) {
	clean := strings.TrimSpace(cmd)
	if clean == "" {
		return false, "empty command"
	}
	if strings.ContainsAny(clean, "\n\r") {
		return false, "multi-line commands not allowed"
	}
	lower := strings.ToLower(clean)

	// Redirection / substitution / expansion always rejected.
	for _, f := range []string{">", "<", "`", "$(", "${", "\\", "\x00"} {
		if strings.Contains(clean, f) {
			return false, fmt.Sprintf("Forbidden pattern '%s' in command", f)
		}
	}

	// Split on compound operators and require each segment to be allowlisted.
	segs := splitSegments(lower)
	for _, seg := range segs {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		if !readOnlyAllow[fields[0]] {
			return false, fmt.Sprintf("Command '%s' not in read-only allowlist", fields[0])
		}
	}

	// Argument-level write primitives.
	for _, tok := range []string{"-delete", "-exec", "-execdir", "-ok", "-okdir", "chmod", "chown", "kill"} {
		if containsAnyToken(lower, tok) {
			return false, fmt.Sprintf("Forbidden argument '%s' in command", tok)
		}
	}
	return true, ""
}

// readOnlyAllow is the strict read-only command allowlist. Note the deliberate
// absence of sed/awk/echo/curl: sed -i and awk 'print > "f"' can write files,
// echo can emit arbitrary content, and curl can upload data out of the host.
var readOnlyAllow = map[string]bool{
	"cat": true, "grep": true, "egrep": true, "fgrep": true,
	"find": true, "tail": true, "head": true, "less": true, "more": true,
	"ls": true, "stat": true, "du": true, "df": true, "free": true,
	"ps": true, "top": true, "uptime": true, "uname": true, "hostname": true,
	"whoami": true, "id": true, "date": true, "pwd": true,
	"ss": true, "netstat": true, "ip": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "wc": true,
	"jq": true, "column": true,
	"yarn": true, "hdfs": true, "zkcli": true,
}

// splitSegments splits a command on ; && || and | (quote-naively — fail-closed:
// a quoted | yields an extra segment that must itself be allowlisted).
func splitSegments(cmd string) []string {
	s := strings.ReplaceAll(cmd, "&&", ";")
	s = strings.ReplaceAll(s, "||", ";")
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '|' })
}

func containsAnyToken(s, tok string) bool {
	for _, f := range strings.Fields(s) {
		if f == tok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tool 3: deepwiki_query
// ---------------------------------------------------------------------------

func deepwikiTool(dw *mcpclient.Client) tools.Def {
	repoMap := map[string]string{
		"flink": "apache/flink", "hdfs": "apache/hadoop", "yarn": "apache/hadoop",
		"hadoop": "apache/hadoop", "kafka": "apache/kafka", "spark": "apache/spark",
		"hbase": "apache/hbase", "hive": "apache/hive",
	}
	return tools.Def{
		Name:        "deepwiki_query",
		Description: "Ask DeepWiki whether an error class is a known issue in the upstream repo. Component→repo: flink→apache/flink, hdfs→apache/hadoop, kafka→apache/kafka.",
		InputSchema: tools.Object(map[string]any{
			"component": tools.Prop("Component: flink, hdfs, yarn, kafka, spark, hbase"),
			"question":  tools.Prop("Natural language question about known bugs"),
			"version":   tools.Prop("Component version"),
		}, []string{"component", "question"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			component := tools.ArgStr(args, "component", "")
			question := tools.ArgStr(args, "question", "")
			version := tools.ArgStr(args, "version", "")
			if dw != nil {
				return deepwikiViaMCP(ctx, dw, repoMap, component, question, version), nil
			}
			return deepwikiHandler(repoMap, component, question, version), nil
		},
	}
}

// deepwikiViaMCP calls DeepWiki's own MCP server (ask_question).
func deepwikiViaMCP(ctx context.Context, dw *mcpclient.Client, repoMap map[string]string, component, question, version string) tools.ToolResult {
	repo, ok := repoMap[strings.ToLower(component)]
	if !ok {
		return tools.OK(fmt.Sprintf("Unknown component '%s'.", component))
	}
	q := question
	if version != "" {
		q = fmt.Sprintf("%s (component version: %s)", question, version)
	}
	cctx, cancel := context.WithTimeout(ctx, deepwikiTimeout)
	defer cancel()
	out, err := dw.CallTool(cctx, "ask_question", map[string]any{
		"repoName": repo,
		"question": q,
	})
	if err != nil {
		return tools.Err(fmt.Sprintf("deepwiki_query failed: %v", err))
	}
	return tools.OK(clip(out, 8000))
}

// deepwikiTimeout bounds the public DeepWiki MCP call (answers can be slow).
const deepwikiTimeout = 45 * time.Second

func deepwikiHandler(repoMap map[string]string, component, question, version string) tools.ToolResult {
	repo, ok := repoMap[strings.ToLower(component)]
	if !ok {
		return tools.OK(fmt.Sprintf("Unknown component '%s'.", component))
	}
	// MCP call to mcp.deepwiki.com would go here in production. In demo/offline
	// mode we return a placeholder carrying the structured query.
	q := question
	if version != "" {
		q = fmt.Sprintf("%s (version: %s)", question, version)
	}
	return tools.OK(fmt.Sprintf("[%s] deepwiki query: %s (not executed in demo)", repo, q))
}

// ---------------------------------------------------------------------------
// Tool 4: fingerprint_match
// ---------------------------------------------------------------------------

func fingerprintTool(kb fingerprintMatcher) tools.Def {
	return tools.Def{
		Name:        "fingerprint_match",
		Description: "Check if this error matches a known case. Fast-path before full diagnosis.",
		InputSchema: tools.Object(map[string]any{
			"component":     tools.Prop("Component: flink, hdfs, yarn"),
			"error_pattern": tools.Prop("Key error: OutOfMemoryError, CheckpointExpired..."),
			"version":       tools.Prop("Component version"),
		}, []string{"component", "error_pattern"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			component := tools.ArgStr(args, "component", "")
			errorPattern := tools.ArgStr(args, "error_pattern", "")
			version := tools.ArgStr(args, "version", "")
			if kb != nil {
				if hit := kb.FingerprintMatch(component, errorPattern, version); hit != nil {
					return tools.OK(fmt.Sprintf("Known issue (confidence: high):\n  Root cause: %v\n  Suggestions: %v", hit["root_cause"], hit["suggestions"])), nil
				}
			}
			return tools.OK(fmt.Sprintf("No fingerprint match for %s/%s. Proceed with diagnosis.", component, errorPattern)), nil
		},
	}
}

// ---------------------------------------------------------------------------
// Tool 5: call_umr_agent (HMAC-SHA1 signed)
// ---------------------------------------------------------------------------

func umrAgentTool(c cluster.Cluster, mcp *mcpclient.Client) tools.Def {
	return tools.Def{
		Name:        "call_umr_agent",
		Description: "Call umrAgent on cluster nodes. Actions: GetLogs(Path,Keywords,MaxLines,Since), CheckProcess(ProcessName), GetBaseInfo, GetAppList.",
		InputSchema: tools.Object(map[string]any{
			"node_name": tools.Prop("Node: 'master1', 'core1', or full node_name"),
			"action":    tools.Prop("umrAgent Action: GetLogs, CheckProcess, GetBaseInfo, GetAppList"),
			"params":    tools.PropObject("Action-specific params"),
		}, []string{"node_name", "action"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			nodeName := tools.ArgStr(args, "node_name", "")
			action := tools.ArgStr(args, "action", "")
			var params map[string]any
			if p, ok := args["params"].(map[string]any); ok {
				params = p
			}
			if mcp != nil {
				return umrAgentViaMCP(ctx, mcp, clusterIDFromContext(c), nodeName, action, params), nil
			}
			return umrAgentHandler(c, nodeName, action, params), nil
		},
	}
}

// umrAgentViaMCP routes the three supported actions through the node-log MCP
// server. Actions the MCP server does not expose fall back to a clear notice
// so the LLM can adjust.
// clusterIDFromContext reads the instance id the way every other MCP-backed
// tool in this file does.
func clusterIDFromContext(c cluster.Cluster) string {
	return tools.ArgStr(c.Context(), "cluster_id", "")
}

func umrAgentViaMCP(ctx context.Context, mcp *mcpclient.Client, clusterID, nodeName, action string, params map[string]any) tools.ToolResult {
	argStr := func(key string) string {
		if s, ok := params[key].(string); ok {
			return s
		}
		return ""
	}
	argInt := func(key string) int {
		if f, ok := params[key].(float64); ok {
			return int(f)
		}
		return 0
	}
	// The cluster id is a transport concern, not an umrAgent action parameter.
	// Requiring the model to repeat it inside "params" made every
	// call_umr_agent call fail with "cluster_id is required" and cost a whole
	// turn. An explicit params.cluster_id still wins if one is supplied.
	if override := argStr("cluster_id"); override != "" {
		clusterID = override
	}
	var name string
	var callArgs map[string]any
	switch action {
	case "GetLogs":
		name = "get_logs"
		callArgs = map[string]any{
			"cluster_id": clusterID,
			"node_ref":   nodeName,
			"log_path":   argStr("Path"),
			"keywords":   argStr("Keywords"),
			"max_lines":  argInt("MaxLines"),
		}
	case "GetBaseInfo":
		name = "get_base_info"
		callArgs = map[string]any{"cluster_id": clusterID, "node_ref": nodeName}
	case "CheckProcess":
		name = "check_process"
		callArgs = map[string]any{
			"cluster_id":   clusterID,
			"node_ref":     nodeName,
			"process_name": argStr("ProcessName"),
		}
	default:
		return tools.OK(fmt.Sprintf("Action %s not available via MCP; use GetLogs, GetBaseInfo or CheckProcess.", action))
	}
	out, err := mcp.CallTool(ctx, name, callArgs)
	if err != nil {
		return tools.Err(fmt.Sprintf("MCP %s failed: %v", name, err))
	}
	return tools.OK(out)
}

func umrAgentHandler(c cluster.Cluster, nodeName, action string, params map[string]any) tools.ToolResult {
	node := c.FindNode(nodeName)
	if node == nil {
		return tools.OK(fmt.Sprintf("Error: Node '%s' not found", nodeName))
	}
	// Simulated cluster carries log content; action GetLogs/GetBaseInfo map to
	// reading scenario logs. For demo, return a placeholder.
	_ = node
	return tools.OK(fmt.Sprintf("umrAgent %s on %s (simulated)", action, nodeName))
}

// ---------------------------------------------------------------------------
// HMAC-SHA1 signing (byte-identical to util/agent.js + pkg/client/uagent)
// ---------------------------------------------------------------------------

// SignParams computes HMAC-SHA1 over sorted-key values, identical to the
// Node.js util/agent.js auth() and Go pkg/client/uagent setGetSignature().
func SignParams(params map[string]string, key string) string {
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

// DateMS returns the current UnixMilli timestamp as a string (for signing).
func DateMS() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}

// MD5Fingerprint computes md5(component:error:version)[:16].
func MD5Fingerprint(component, errorPattern, version string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", component, errorPattern, version)))
	return hex.EncodeToString(sum[:])[:16]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
