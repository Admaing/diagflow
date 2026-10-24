// Package mcpserver exposes DiagFlow's cluster-diagnosis capabilities as an
// MCP (Model Context Protocol) server so multiple agents can share them.
//
// Transport: Streamable HTTP — POST /mcp with single JSON-RPC 2.0 messages
// (stateless; no SSE stream). Supported methods: initialize, tools/list,
// tools/call. Auth: per-agent bearer tokens; every tool call is audit-logged.
//
// Two data planes back the tools:
//   - cluster metadata (cluster info, nodes, services) via the UHadoop access
//     HTTP action API, discovered through ZooKeeper;
//   - node operations (logs, resource snapshots, process checks, YARN) via
//     read-only SSH to the node's IPv4 address, using the credential the
//     access API hands out.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Admaing/diagflow/internal/infra"
	"github.com/Admaing/diagflow/internal/metrics"
)

// DataSource is the backend the MCP tools read from. It keeps the server free
// of transport details (ZooKeeper, HTTP, SSH) and makes the tools testable.
type DataSource interface {
	ClusterInfo(ctx context.Context, clusterID string) (*infra.ClusterInfo, error)
	Nodes(ctx context.Context, clusterID string) ([]infra.NodeInfo, error)
	Apps(ctx context.Context, clusterID string) ([]infra.NodeApp, error)
	Node(ctx context.Context, clusterID, ref string) (*infra.NodeInfo, error)
	// Run executes a read-only command on a node.
	Run(ctx context.Context, clusterID string, node *infra.NodeInfo, command string) (string, error)
	// Log fetches log lines from a node, optionally keyword-filtered.
	Log(ctx context.Context, clusterID string, node *infra.NodeInfo, logPath, keywords string, maxLines int) (string, error)
}

// Server is the MCP server for cluster diagnosis.
type Server struct {
	log       *slog.Logger
	ds        DataSource
	tokens    map[string]string // token → agent name
	fanout    int               // max concurrent node operations
	callTotal *metrics.Counter
	callDur   *metrics.Histogram
	rejected  *metrics.Counter

	acl            ACL
	limiter        *rateLimiter
	maxSearchNodes int
}

// Option configures the server.
type Option func(*Server)

// WithACL restricts which clusters each agent may query. nil = unrestricted.
func WithACL(acl ACL) Option { return func(s *Server) { s.acl = acl } }

// WithRateLimit enables a per-agent token bucket for tools/call.
func WithRateLimit(rps float64, burst int) Option {
	return func(s *Server) { s.limiter = newRateLimiter(rps, burst) }
}

// WithMaxSearchNodes caps how many nodes one search_logs call fans out to.
func WithMaxSearchNodes(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.maxSearchNodes = n
		}
	}
}

// New builds an MCP server. tokens maps bearer token → agent identity (used
// for auth, ACL and audit).
func New(log *slog.Logger, ds DataSource, tokens map[string]string, opts ...Option) *Server {
	s := &Server{
		log:            log,
		ds:             ds,
		tokens:         tokens,
		fanout:         8,
		maxSearchNodes: 32,
		callTotal:      metrics.NewCounter("mcp_tool_calls_total", "MCP tool calls by agent/tool/status", "agent", "tool", "status"),
		callDur:        metrics.NewHistogram("mcp_tool_duration_ms", "MCP tool call wall time in milliseconds"),
		rejected:       metrics.NewCounter("mcp_rejected_total", "MCP requests rejected before execution", "reason"),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ---- MCP protocol ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name            string          `json:"name"`
		Arguments       json.RawMessage `json:"arguments"`
		ProtocolVersion string          `json:"protocolVersion"`
	} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e rpcError) response(id json.RawMessage) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": idOrNull(id), "error": e}
}

func idOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return json.RawMessage(id)
}

var protocolVersion = "2024-11-05"

// supportedProtocols are the MCP revisions this server can speak. The tool
// surface (initialize/list/call) is stable across them, so echoing the client's
// revision avoids a needless downgrade negotiation.
var supportedProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

func negotiateProtocol(client string) string {
	if supportedProtocols[client] {
		return client
	}
	return protocolVersion
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, http.StatusBadRequest, rpcError{-32700, "parse error"}.response(nil))
		return
	}
	reqID := requestIDFrom(r)

	switch req.Method {
	case "initialize":
		writeRPC(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": idOrNull(req.ID),
			"result": map[string]any{
				"protocolVersion": negotiateProtocol(req.Params.ProtocolVersion),
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "diagflow-cluster-mcp", "version": "0.3.0"},
			},
		})
	case "notifications/initialized", "ping":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeRPC(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": idOrNull(req.ID),
			"result": map[string]any{"tools": toolDefs()},
		})
	case "tools/call":
		agent := agentFrom(r)
		if !s.limiter.allow(agent) {
			s.rejected.Inc("rate_limited")
			w.Header().Set("Retry-After", "1")
			s.log.Warn("mcp rate limited", "agent", agent, "tool", req.Params.Name,
				"request_id", reqID)
			writeRPC(w, http.StatusTooManyRequests, map[string]any{
				"jsonrpc": "2.0", "id": idOrNull(req.ID),
				"error": rpcError{Code: -32005, Message: "rate limit exceeded for agent " + agent},
			})
			return
		}
		var meta struct {
			ClusterID string `json:"cluster_id"`
		}
		if len(req.Params.Arguments) > 0 {
			_ = json.Unmarshal(req.Params.Arguments, &meta)
		}
		start := time.Now()
		result, rpcErr := s.callTool(r.Context(), agent, req.Params.Name, req.Params.Arguments)
		durationMS := time.Since(start).Milliseconds()
		s.callDur.Observe(float64(durationMS))
		if rpcErr != nil {
			status := "error"
			if rpcErr.Code == -32003 {
				status = "forbidden"
				s.rejected.Inc("acl")
			}
			s.callTotal.Inc(agent, req.Params.Name, status)
			s.log.Warn("mcp tool call failed", "agent", agent, "tool", req.Params.Name,
				"cluster_id", meta.ClusterID, "request_id", reqID,
				"duration_ms", durationMS, "error", rpcErr.Message)
			writeRPC(w, http.StatusOK, rpcError{Code: rpcErr.Code, Message: rpcErr.Message}.response(req.ID))
			return
		}
		s.callTotal.Inc(agent, req.Params.Name, "ok")
		// Audit: who fetched what, from which cluster/nodes.
		s.log.Info("mcp tool call",
			"agent", agent, "tool", req.Params.Name, "cluster_id", meta.ClusterID,
			"request_id", reqID,
			"args", truncStr(string(req.Params.Arguments), 300),
			"result_bytes", len(result), "duration_ms", durationMS)
		writeRPC(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": idOrNull(req.ID),
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": result}},
			},
		})
	default:
		writeRPC(w, http.StatusOK, rpcError{-32601, "method not found: " + req.Method}.response(req.ID))
	}
}

type callArgs struct {
	ClusterID   string `json:"cluster_id"`
	NodeRef     string `json:"node_ref"`
	LogPath     string `json:"log_path"`
	Keywords    string `json:"keywords"`
	MaxLines    int    `json:"max_lines"`
	ProcessName string `json:"process_name"`
	Action      string `json:"action"`
	AppID       string `json:"app_id"`
	States      string `json:"states"`
	Role        string `json:"role"`
	State       int    `json:"state"`
	Nodes       string `json:"nodes"`
	Roles       string `json:"roles"`
	Command     string `json:"command"`
	Dir         string `json:"dir"`
}

func (s *Server) callTool(ctx context.Context, agent, name string, rawArgs json.RawMessage) (string, *rpcError) {
	var args callArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
		}
	}
	if args.ClusterID == "" {
		return "", &rpcError{Code: -32602, Message: "cluster_id is required"}
	}
	if !s.acl.allowed(agent, args.ClusterID) {
		return "", &rpcError{Code: -32003, Message: fmt.Sprintf(
			"agent %q is not allowed to query cluster %q", agent, args.ClusterID)}
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	switch name {
	case "get_cluster_info":
		return s.getClusterInfo(ctx, args)
	case "list_nodes":
		return s.listNodes(ctx, args)
	case "get_node_info":
		return s.getNodeInfo(ctx, args)
	case "get_cluster_services":
		return s.getClusterServices(ctx, args)
	case "get_node_log":
		return s.getNodeLog(ctx, args, name)
	case "get_logs": // legacy alias kept for existing DiagFlow callers
		return s.getNodeLog(ctx, args, name)
	case "search_logs":
		return s.searchLogs(ctx, args)
	case "list_logs":
		return s.listLogs(ctx, args)
	case "get_node_metrics":
		return s.getNodeMetrics(ctx, args, name)
	case "get_metrics": // legacy alias
		return s.getNodeMetrics(ctx, args, name)
	case "check_process":
		return s.checkProcess(ctx, args)
	case "get_base_info":
		return s.getBaseInfo(ctx, args)
	case "query_yarn":
		return s.queryYarn(ctx, args)
	case "ssh_exec":
		return s.sshExec(ctx, args)
	default:
		return "", &rpcError{Code: -32602, Message: "unknown tool: " + name}
	}
}

// ---- tool implementations ----

func (s *Server) getClusterInfo(ctx context.Context, args callArgs) (string, *rpcError) {
	info, err := s.ds.ClusterInfo(ctx, args.ClusterID)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "get_cluster_info: " + err.Error()}
	}
	out := map[string]any{
		"instance_id":        info.InstanceID,
		"instance_name":      info.InstanceName,
		"framework":          info.Framework,
		"framework_version":  info.FrameworkVersion,
		"cluster_state":      info.ClusterState.Int(),
		"cluster_state_name": infra.ClusterStateName(info.ClusterState.Int()),
		"state_hint":         infra.ClusterStateHint(info.ClusterState.Int()),
		"master_num":         info.MasterNum.Int(),
		"core_num":           info.CoreNum.Int(),
		"task_num":           info.TaskNum.Int(),
		"client_num":         info.ClientNum.Int(),
		"network_type":       info.NetworkType,
		"create_time":        info.CreateTime,
		"running_time":       info.RunningTime,
		"apps":               appList(info.App),
		"node_count":         len(info.NodeInfo),
		"nodes":              sanitizeNodes(info.NodeInfo),
	}
	if info.Remark != "" {
		out["remark"] = info.Remark
	}
	return marshalText(out)
}

func (s *Server) listNodes(ctx context.Context, args callArgs) (string, *rpcError) {
	info, err := s.ds.ClusterInfo(ctx, args.ClusterID)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "list_nodes: " + err.Error()}
	}
	var nodes []infra.NodeInfo
	if args.State > 0 {
		nodes, err = s.ds.Nodes(ctx, args.ClusterID)
		if err != nil {
			return "", &rpcError{Code: -32603, Message: "list_nodes: " + err.Error()}
		}
		nodes = filterByState(nodes, args.State)
	} else {
		nodes = info.NodeInfo
	}
	nodes = filterByRole(nodes, args.Role)
	items := sanitizeNodes(nodes)
	out := map[string]any{
		"instance_id": info.InstanceID,
		"count":       len(items),
		"nodes":       items,
	}
	if !infra.ClusterReady(info.ClusterState.Int()) {
		out["state_hint"] = infra.ClusterStateHint(info.ClusterState.Int())
	}
	return marshalText(out)
}

func (s *Server) getNodeInfo(ctx context.Context, args callArgs) (string, *rpcError) {
	node, err := s.ds.Node(ctx, args.ClusterID, args.NodeRef)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "get_node_info: " + err.Error()}
	}
	return marshalText(sanitizeNode(*node))
}

func (s *Server) getClusterServices(ctx context.Context, args callArgs) (string, *rpcError) {
	apps, err := s.ds.Apps(ctx, args.ClusterID)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "get_cluster_services: " + err.Error()}
	}
	out := map[string]any{"instance_id": args.ClusterID, "apps": []map[string]any{}}
	list := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		entry := map[string]any{
			"app_name":        app.AppName,
			"app_version":     app.AppVersion,
			"app_status":      app.AppStatus.Int(),
			"app_status_name": infra.AppStateName(app.AppStatus.Int()),
		}
		if len(app.NodeService) > 0 {
			services := make([]map[string]any, 0, len(app.NodeService))
			for _, ns := range app.NodeService {
				services = append(services, map[string]any{
					"node_id":      ns.NodeID,
					"node_ip":      ns.NodeIP,
					"service_name": ns.ServiceName,
				})
			}
			entry["node_service"] = services
		}
		list = append(list, entry)
	}
	out["apps"] = list
	return marshalText(out)
}

func (s *Server) getNodeLog(ctx context.Context, args callArgs, tool string) (string, *rpcError) {
	if args.LogPath == "" {
		return "", &rpcError{Code: -32602, Message: "log_path is required"}
	}
	if err := infra.ValidateLogPath(args.LogPath); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, tool)
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	out, err := s.ds.Log(ctx, args.ClusterID, node, args.LogPath, args.Keywords, clampLines(args.MaxLines))
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("%s %s:%s: %v", tool, node.NodeName, args.LogPath, err)}
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("# %s (%s) %s\n(no matching log lines)", node.NodeName, node.NodeIP, args.LogPath), nil
	}
	return fmt.Sprintf("# %s (%s)\n%s", node.NodeName, node.NodeIP, clip(out, 60000)), nil
}

func (s *Server) searchLogs(ctx context.Context, args callArgs) (string, *rpcError) {
	if strings.TrimSpace(args.Keywords) == "" {
		return "", &rpcError{Code: -32602, Message: "keywords is required for search_logs"}
	}
	if args.LogPath == "" {
		return "", &rpcError{Code: -32602, Message: "log_path is required for search_logs"}
	}
	if err := infra.ValidateLogPath(args.LogPath); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	info, err := s.ds.ClusterInfo(ctx, args.ClusterID)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "search_logs: " + err.Error()}
	}
	if !infra.ClusterReady(info.ClusterState.Int()) {
		return degradeText("search_logs", info), nil
	}

	candidates := info.NodeInfo
	if args.Nodes != "" {
		var picked []infra.NodeInfo
		for _, ref := range splitList(args.Nodes) {
			node, err := s.ds.Node(ctx, args.ClusterID, ref)
			if err != nil {
				continue
			}
			picked = append(picked, *node)
		}
		candidates = picked
	} else if args.Roles != "" {
		var picked []infra.NodeInfo
		for _, role := range splitList(args.Roles) {
			picked = append(picked, filterByRole(candidates, role)...)
		}
		candidates = picked
	}
	if len(candidates) == 0 {
		return "", &rpcError{Code: -32603, Message: "search_logs: no matching nodes"}
	}
	candidates = dedupeNodes(candidates)
	totalNodes := len(candidates)
	if s.maxSearchNodes > 0 && len(candidates) > s.maxSearchNodes {
		candidates = candidates[:s.maxSearchNodes]
	}

	maxLines := clampLines(args.MaxLines)
	type hit struct {
		node string
		ip   string
		out  string
		err  error
	}
	results := make([]hit, len(candidates))
	sem := make(chan struct{}, s.fanout)
	var wg sync.WaitGroup
	for i, node := range candidates {
		wg.Add(1)
		go func(i int, node infra.NodeInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := s.ds.Log(ctx, args.ClusterID, &candidates[i], args.LogPath, args.Keywords, maxLines)
			results[i] = hit{node: node.NodeName, ip: node.NodeIP, out: out, err: err}
		}(i, node)
	}
	wg.Wait()

	var b strings.Builder
	fmt.Fprintf(&b, "# search_logs %q in %s across %d node(s)\n", args.Keywords, args.LogPath, len(candidates))
	if totalNodes > len(candidates) {
		fmt.Fprintf(&b, "# sampled %d of %d nodes to stay inside the call budget; pass 'nodes' to target the rest\n",
			len(candidates), totalNodes)
	}
	matched, failed, filtered, omitted := 0, 0, 0, 0
	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(&b, "\n## %s (%s)\n[error] %v\n", r.node, r.ip, r.err)
			continue
		}
		// Raw grep output is dominated by startup banners and classpath dumps
		// whose jar names happen to match a severity regex. Strip them and cap
		// the snippet, so one noisy component cannot consume the whole context
		// budget on every ReAct turn.
		snippet, noise := infra.FilterLogNoise(r.out)
		filtered += noise
		snippet, dropped := infra.CapSearchSnippet(snippet, searchLogLinesPerNode)
		omitted += dropped
		snippet = infra.CapLineWidth(snippet, searchLogLineChars)
		if strings.TrimSpace(snippet) == "" {
			if noise > 0 {
				fmt.Fprintf(&b, "\n## %s (%s)\n(no matching lines; %d configuration line(s) filtered)\n", r.node, r.ip, noise)
			} else {
				fmt.Fprintf(&b, "\n## %s (%s)\n(no matching lines)\n", r.node, r.ip)
			}
			continue
		}
		matched++
		fmt.Fprintf(&b, "\n## %s (%s)\n%s\n", r.node, r.ip, snippet)
		if s := infra.SeveritySummary(snippet); s != "" {
			fmt.Fprintf(&b, "[severity] %s\n", s)
		}
		if dropped > 0 {
			fmt.Fprintf(&b, "[%d older matching line(s) omitted]\n", dropped)
		}
		if noise > 0 {
			fmt.Fprintf(&b, "[%d configuration line(s) filtered]\n", noise)
		}
	}
	fmt.Fprintf(&b, "\n# %d/%d node(s) returned matches, %d failed, %d line(s) omitted, %d config line(s) filtered\n",
		matched, len(candidates), failed, omitted, filtered)
	return clip(b.String(), 24000), nil
}

// listLogs lists the log directories on a node so agents can discover log
// file names instead of guessing them.
func (s *Server) listLogs(ctx context.Context, args callArgs) (string, *rpcError) {
	dirs := infra.DefaultLogDirs
	if strings.TrimSpace(args.Dir) != "" {
		// Callers routinely pass the bare component name ("hadoop-hdfs") rather
		// than a full path. Rejecting that burned an LLM turn every run, so
		// resolve it against the /var/log root instead.
		dir := strings.TrimSpace(args.Dir)
		if !strings.HasPrefix(dir, "/") {
			dir = "/var/log/" + strings.Trim(dir, "/")
		}
		if err := infra.ValidateLogDir(dir); err != nil {
			return "", &rpcError{Code: -32602, Message: err.Error()}
		}
		dirs = []string{dir}
	}
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, "list_logs")
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	cmd, err := infra.BuildListLogsCommand(dirs)
	if err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("list_logs %s: %v", node.NodeName, err)}
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("# %s (%s)\n(no known log directories present)", node.NodeName, node.NodeIP), nil
	}
	return fmt.Sprintf("# %s (%s) log directories\n%s", node.NodeName, node.NodeIP, clip(out, 30000)), nil
}

func (s *Server) getNodeMetrics(ctx context.Context, args callArgs, tool string) (string, *rpcError) {
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, tool)
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	// Read-only resource snapshot; no external agent required.
	const cmd = "uptime; echo '--- load'; cat /proc/loadavg; echo '--- mem'; free -m; echo '--- disk'; df -h"
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("%s %s: %v", tool, node.NodeName, err)}
	}
	return fmt.Sprintf("# %s (%s)\n%s", node.NodeName, node.NodeIP, clip(out, 12000)), nil
}

func (s *Server) checkProcess(ctx context.Context, args callArgs) (string, *rpcError) {
	if strings.TrimSpace(args.ProcessName) == "" {
		return "", &rpcError{Code: -32602, Message: "process_name is required"}
	}
	if err := infra.ValidatePattern(args.ProcessName); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, "check_process")
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	cmd := fmt.Sprintf("ps -eo pid,user,pcpu,pmem,args | grep -i -- %s | grep -v grep || true",
		infra.ShellQuote(args.ProcessName))
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("check_process %s: %v", node.NodeName, err)}
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("# %s: process %q not running", node.NodeName, args.ProcessName), nil
	}
	return fmt.Sprintf("# %s (%s) process %q\n%s", node.NodeName, node.NodeIP, args.ProcessName, clip(out, 8000)), nil
}

func (s *Server) getBaseInfo(ctx context.Context, args callArgs) (string, *rpcError) {
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, "get_base_info")
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	const cmd = "hostname; uname -a; echo '--- os'; (cat /etc/redhat-release 2>/dev/null || head -3 /etc/os-release 2>/dev/null); " +
		"echo '--- cpu'; nproc; echo '--- mem'; free -m | head -2"
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("get_base_info %s: %v", node.NodeName, err)}
	}
	return fmt.Sprintf("# %s (%s)\n%s", node.NodeName, node.NodeIP, clip(out, 8000)), nil
}

func (s *Server) queryYarn(ctx context.Context, args callArgs) (string, *rpcError) {
	states := args.States
	if err := infra.ValidatePattern(states); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	if err := infra.ValidatePattern(args.AppID); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, "query_yarn")
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	var cmd string
	switch args.Action {
	case "", "list_apps", "list":
		cmd = "yarn application -list"
		if strings.TrimSpace(states) != "" {
			cmd += " -appStates " + infra.ShellQuote(states)
		}
	case "app":
		if strings.TrimSpace(args.AppID) == "" {
			return "", &rpcError{Code: -32602, Message: "app_id is required for action='app'"}
		}
		cmd = "yarn application -status " + infra.ShellQuote(args.AppID)
	default:
		return "", &rpcError{Code: -32602, Message: "action must be 'list_apps' or 'app'"}
	}
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("query_yarn %s: %v", node.NodeName, err)}
	}
	return fmt.Sprintf("# yarn on %s (%s)\n%s", node.NodeName, node.NodeIP, clip(out, 20000)), nil
}

// sshExec runs one whitelisted, read-only command on a node.
func (s *Server) sshExec(ctx context.Context, args callArgs) (string, *rpcError) {
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return "", &rpcError{Code: -32602, Message: "command is required for ssh_exec"}
	}
	node, rpcErr := s.resolveNodeForOps(ctx, args.ClusterID, args.NodeRef, "ssh_exec")
	if rpcErr != nil || node == nil {
		return "", rpcErr
	}
	if err := infra.ValidateReadOnlyCommand(cmd); err != nil {
		return "", &rpcError{Code: -32602, Message: err.Error()}
	}
	out, err := s.ds.Run(ctx, args.ClusterID, node, cmd)
	if err != nil {
		return "", &rpcError{Code: -32603, Message: fmt.Sprintf("ssh_exec %s: %v", node.NodeName, err)}
	}
	return fmt.Sprintf("# %s (%s) $ %s\n%s", node.NodeName, node.NodeIP, cmd, clip(out, 20000)), nil
}

// resolveNodeForOps resolves a node and refuses when the cluster is not in a
// state where nodes are reachable, returning a clear degraded message.
func (s *Server) resolveNodeForOps(ctx context.Context, clusterID, ref, tool string) (*infra.NodeInfo, *rpcError) {
	info, err := s.ds.ClusterInfo(ctx, clusterID)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: tool + ": " + err.Error()}
	}
	if !infra.ClusterReady(info.ClusterState.Int()) {
		return nil, &rpcError{Code: -32603, Message: fmt.Sprintf(
			"%s: cluster %s is in state %s — node operations are unavailable for now",
			tool, clusterID, infra.ClusterStateHint(info.ClusterState.Int()))}
	}
	node, err := s.ds.Node(ctx, clusterID, ref)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: tool + ": " + err.Error()}
	}
	return node, nil
}

func degradeText(tool string, info *infra.ClusterInfo) string {
	return fmt.Sprintf("# %s unavailable\ncluster %s is in state %s — node operations are unavailable for now",
		tool, info.InstanceID, infra.ClusterStateHint(info.ClusterState.Int()))
}

// ---- serialization helpers (never leak credentials) ----

// sanitizeNode renders a node without password / agent key / access user.
func sanitizeNode(n infra.NodeInfo) map[string]any {
	out := map[string]any{
		"node_id":         n.NodeID,
		"node_name":       n.NodeName,
		"node_type":       n.NodeType,
		"node_ip":         n.NodeIP,
		"node_state":      n.NodeState.Int(),
		"node_state_name": infra.NodeStateName(n.NodeState.Int()),
	}
	if n.NodeIPv6 != "" {
		out["node_ipv6"] = n.NodeIPv6
	}
	if len(n.ServiceName) > 0 {
		out["service_name"] = n.ServiceName
	}
	if n.UHostID != "" {
		out["uhost_id"] = n.UHostID
	}
	if n.Remark != "" {
		out["remark"] = n.Remark
	}
	return out
}

func sanitizeNodes(nodes []infra.NodeInfo) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, sanitizeNode(n))
	}
	return out
}

func appList(apps []infra.ClusterApp) []map[string]any {
	out := make([]map[string]any, 0, len(apps))
	for _, a := range apps {
		out = append(out, map[string]any{
			"app_name":        a.AppName,
			"app_version":     a.AppVersion,
			"app_status":      a.AppStatus.Int(),
			"app_status_name": infra.AppStateName(a.AppStatus.Int()),
		})
	}
	return out
}

func marshalText(v any) (string, *rpcError) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", &rpcError{Code: -32603, Message: "encode result: " + err.Error()}
	}
	return string(b), nil
}

func filterByRole(nodes []infra.NodeInfo, role string) []infra.NodeInfo {
	if strings.TrimSpace(role) == "" {
		return nodes
	}
	role = strings.ToLower(strings.TrimSpace(role))
	var out []infra.NodeInfo
	for _, n := range nodes {
		if strings.ToLower(n.NodeType) == role ||
			strings.Contains(strings.ToLower(n.NodeName), role) {
			out = append(out, n)
		}
	}
	return out
}

func filterByState(nodes []infra.NodeInfo, state int) []infra.NodeInfo {
	var out []infra.NodeInfo
	for _, n := range nodes {
		if n.NodeState.Int() == state {
			out = append(out, n)
		}
	}
	return out
}

func dedupeNodes(nodes []infra.NodeInfo) []infra.NodeInfo {
	seen := make(map[string]bool, len(nodes))
	// Copy: callers pass the cached ClusterInfo.NodeInfo slice, and compacting
	// it in place corrupted the 30s cluster cache and raced between requests.
	out := make([]infra.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if seen[n.NodeID] {
			continue
		}
		seen[n.NodeID] = true
		out = append(out, n)
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// search_logs results feed every ReAct turn, so they are capped harder than a
// single-file read: a handful of recent matches plus a severity histogram is
// enough to triage, and the counters keep the omissions visible.
const (
	searchLogLinesPerNode = 30
	searchLogLineChars    = 500
)

func clampLines(n int) int {
	if n <= 0 {
		return 200
	}
	if n > 2000 {
		return 2000
	}
	return n
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

// ---- HTTP plumbing ----

// Handler returns the routed handler (healthz/readyz/metrics + mcp).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(metrics.Render()))
	})
	mux.HandleFunc("POST /mcp", s.auth(s.handleMCP))
	// /mcp speaks JSON-RPC over POST only. Browsers (and curious humans) open
	// it with GET, so return a usable hint instead of a bare 405.
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "POST")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("DiagFlow cluster MCP server.\n\n" +
			"This endpoint speaks JSON-RPC 2.0 over HTTP POST only; a browser GET cannot\n" +
			"talk to it. Configure it as a Streamable HTTP MCP server in your client:\n\n" +
			"  url:    http://<host>:8090/mcp\n" +
			"  header: Authorization: Bearer <token>\n\n" +
			"Try a call by hand:\n\n" +
			"  curl -s -X POST http://<host>:8090/mcp \\\n" +
			"    -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' \\\n" +
			"    -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}'\n\n" +
			"Health: GET /healthz\n"))
	})
	return withRequestID(mux)
}

// readyChecker is implemented by data sources that can report whether their
// upstream (ZooKeeper + the zone's access API) is reachable. Without it,
// readiness falls back to "the process started", which is not useful.
type readyChecker interface {
	Ready(ctx context.Context) error
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	info := map[string]any{"status": "ready", "agents": len(s.tokens)}
	if s.acl != nil {
		// Count only: probe endpoints are unauthenticated, and the ACL
		// itself names agents and clusters.
		info["acl_agents"] = len(s.acl)
	}
	if rc, ok := s.ds.(readyChecker); ok {
		// ZooKeeper's Children() blocks until it has a session, which can take
		// longer than the probe's timeout. Race the check so a dead ensemble
		// yields a prompt 503 instead of stalling the probe.
		done := make(chan error, 1)
		go func() { done <- rc.Ready(ctx) }()
		select {
		case err := <-done:
			if err != nil {
				info["status"] = "not ready"
				info["reason"] = err.Error()
				writeJSONStatus(w, http.StatusServiceUnavailable, info)
				return
			}
		case <-ctx.Done():
			info["status"] = "not ready"
			info["reason"] = "readiness probe timed out: " + ctx.Err().Error()
			writeJSONStatus(w, http.StatusServiceUnavailable, info)
			return
		}
	}
	writeJSONStatus(w, http.StatusOK, info)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// withRequestID stamps every request with an id that is echoed in the response
// header and every audit line, so a tool call can be correlated with the
// diagnosis that triggered it.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type requestIDKey struct{}

func requestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// ListenAndServe runs until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	s.log.Info("mcp server listening", "port", port)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		return err
	}
	return ctx.Err()
}

// auth enforces per-agent bearer tokens. Missing token config disables auth
// (dev only — main.go refuses to start token-less in production).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.tokens) > 0 {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			agent, ok := s.tokens[tok]
			if !ok {
				s.log.Warn("mcp unauthorized", "remote", r.RemoteAddr)
				writeRPC(w, http.StatusUnauthorized, map[string]any{
					"jsonrpc": "2.0", "id": nil,
					"error": rpcError{Code: -32000, Message: "unauthorized"},
				})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), agentKey{}, agent))
		}
		next(w, r)
	}
}

type agentKey struct{}

func agentFrom(r *http.Request) string {
	name, _ := r.Context().Value(agentKey{}).(string)
	return name
}

func writeRPC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
