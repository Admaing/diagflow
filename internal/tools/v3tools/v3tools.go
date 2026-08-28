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
	"github.com/Admaing/diagflow/internal/tools"
)

// BuildV3Tools returns the five v3 tools, injecting a KB-backed fingerprint
// handler when kb is non-nil.
func BuildV3Tools(c cluster.Cluster, kb fingerprintMatcher) []tools.Def {
	list := []tools.Def{
		queryYarnTool(c),
		sshExecTool(c),
		umrAgentTool(c),
		deepwikiTool(),
		fingerprintTool(kb),
	}
	return list
}

// fingerprintMatcher is the minimal KB surface fingerprint_match needs.
type fingerprintMatcher interface {
	FingerprintMatch(component, errorPattern, version string) map[string]any
}

// ---------------------------------------------------------------------------
// Tool 1: query_yarn
// ---------------------------------------------------------------------------

func queryYarnTool(c cluster.Cluster) tools.Def {
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
			return queryYarnHandler(c, action, appID, appName), nil
		},
	}
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

func sshExecTool(c cluster.Cluster) tools.Def {
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
			return sshExecHandler(c, nodeName, cmd), nil
		},
	}
}

func sshExecHandler(c cluster.Cluster, nodeName, cmd string) tools.ToolResult {
	if allowed, reason := ValidateCommand(cmd); !allowed {
		return tools.OK(fmt.Sprintf("Command blocked by safety filter: %s", reason))
	}
	// Simulated cluster returns an empty result (no real SSH in demo). The real
	// adapter would exec over paramiko-equivalent SSH.
	_ = nodeName
	return tools.OK("(simulated) command validated but not executed")
}

// ValidateCommand checks that a shell command is read-only and safe.
// Returns (allowed, reason).
func ValidateCommand(cmd string) (bool, string) {
	forbidden := []string{"rm ", "shutdown", "reboot", "mv ", "dd ", "mkfs", ">", ">>", "| sh", "$(", "`"}
	allowed := []string{
		"cat", "grep", "find", "tail", "head", "ls", "ps",
		"df", "free", "curl", "du", "wc", "echo", "stat",
		"ss", "netstat", "ip", "hostname", "uptime", "uname",
		"awk", "sed", "sort", "uniq", "cut", "tr", "yarn", "pwd",
	}

	clean := strings.TrimSpace(cmd)
	lower := strings.ToLower(clean)

	for _, f := range forbidden {
		if strings.Contains(lower, f) {
			return false, fmt.Sprintf("Forbidden pattern '%s' in command", f)
		}
	}

	fields := strings.Fields(lower)
	firstWord := ""
	if len(fields) > 0 {
		firstWord = fields[0]
	}
	if contains(allowed, firstWord) {
		return true, ""
	}

	// Allow piped commands whose first segment starts with an allowed word.
	if strings.Contains(clean, "|") {
		first := strings.TrimSpace(strings.Split(clean, "|")[0])
		f := strings.Fields(strings.ToLower(first))
		if len(f) > 0 && contains(allowed, f[0]) {
			return true, ""
		}
	}

	return false, fmt.Sprintf("Command '%s' not in read-only allowlist", firstWord)
}

// ---------------------------------------------------------------------------
// Tool 3: deepwiki_query
// ---------------------------------------------------------------------------

func deepwikiTool() tools.Def {
	repoMap := map[string]string{
		"flink": "apache/flink", "hdfs": "apache/hadoop", "yarn": "apache/hadoop",
		"hadoop": "apache/hadoop", "kafka": "apache/kafka", "spark": "apache/spark",
		"hbase": "apache/hbase", "hive": "apache/hive",
	}
	return tools.Def{
		Name:        "deepwiki_query",
		Description: "Query known issues in open-source repos. Component→repo: flink→apache/flink, hdfs→apache/hadoop, kafka→apache/kafka.",
		InputSchema: tools.Object(map[string]any{
			"component": tools.Prop("Component: flink, hdfs, yarn, kafka, spark, hbase"),
			"question":  tools.Prop("Natural language question about known bugs"),
			"version":   tools.Prop("Component version"),
		}, []string{"component", "question"}),
		Handler: func(ctx context.Context, args map[string]any) (tools.ToolResult, error) {
			component := tools.ArgStr(args, "component", "")
			question := tools.ArgStr(args, "question", "")
			version := tools.ArgStr(args, "version", "")
			return deepwikiHandler(repoMap, component, question, version), nil
		},
	}
}

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

func umrAgentTool(c cluster.Cluster) tools.Def {
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
			return umrAgentHandler(c, nodeName, action, params), nil
		},
	}
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
