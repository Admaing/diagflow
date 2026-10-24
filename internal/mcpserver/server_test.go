package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Admaing/diagflow/internal/infra"
)

// fakeDS is an in-memory DataSource for exercising the tool layer.
type fakeDS struct {
	info  *infra.ClusterInfo
	nodes []infra.NodeInfo
	apps  []infra.NodeApp

	runOut string
	runErr error
	logOut string
	logErr error

	mu       sync.Mutex
	lastCmd  string
	lastPath string
	lastKeyw string
	logNodes []string
	runNodes []string
	runCount int
}

func (f *fakeDS) ClusterInfo(context.Context, string) (*infra.ClusterInfo, error) {
	if f.info == nil {
		return nil, errors.New("cluster not found")
	}
	return f.info, nil
}

func (f *fakeDS) Nodes(context.Context, string) ([]infra.NodeInfo, error) {
	return f.nodes, nil
}

func (f *fakeDS) Apps(context.Context, string) ([]infra.NodeApp, error) { return f.apps, nil }

func (f *fakeDS) Node(_ context.Context, _, ref string) (*infra.NodeInfo, error) {
	return infra.FindNode(f.nodes, ref)
}

func (f *fakeDS) Run(_ context.Context, _ string, n *infra.NodeInfo, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCmd = cmd
	f.runCount++
	f.runNodes = append(f.runNodes, n.NodeName)
	return f.runOut, f.runErr
}

func (f *fakeDS) Log(_ context.Context, _ string, n *infra.NodeInfo, logPath, keywords string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPath, f.lastKeyw = logPath, keywords
	f.logNodes = append(f.logNodes, n.NodeName)
	return f.logOut, f.logErr
}

func readyCluster() *infra.ClusterInfo {
	return &infra.ClusterInfo{
		InstanceID:   "uhadoop-test",
		InstanceName: "gyf-test",
		Framework:    "Hadoop",
		ClusterState: 2,
		MasterNum:    2,
		CoreNum:      3,
		App:          []infra.ClusterApp{{AppName: "Hive", AppVersion: "3.1.3", AppStatus: 1}},
		NodeInfo: []infra.NodeInfo{
			{NodeID: "n1", NodeName: "c-master1", NodeType: "master", NodeIP: "10.0.0.1",
				AccessUser: "root", Passwd: "c2VjcmV0", NodeState: 1, ServiceName: infra.FlexStrings{"NameNode"}},
			{NodeID: "n2", NodeName: "c-master2", NodeType: "master", NodeIP: "10.0.0.2", NodeState: 1},
			{NodeID: "n3", NodeName: "c-core1", NodeType: "core", NodeIP: "10.0.0.3", NodeState: 1},
		},
	}
}

func newTestServer(tokens map[string]string, ds DataSource) *Server {
	return New(slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)), ds, tokens)
}

func rpc(t *testing.T, h http.Handler, body, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// callText runs a tool and returns the text content (fails on RPC error).
func callText(t *testing.T, h http.Handler, name, args string) string {
	t.Helper()
	_, out := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`, "tok")
	if e, ok := out["error"]; ok && e != nil {
		t.Fatalf("tool %s error: %v", name, e)
	}
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool %s: empty content", name)
	}
	return content[0].(map[string]any)["text"].(string)
}

// callErr runs a tool and returns the JSON-RPC error message ("" if none).
func callErr(t *testing.T, h http.Handler, name, args string) string {
	t.Helper()
	_, out := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`, "tok")
	e, ok := out["error"].(map[string]any)
	if !ok {
		return ""
	}
	return e["message"].(string)
}

func TestInitializeAndToolsList(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{info: readyCluster()}).Handler()

	code, out := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "tok")
	if code != 200 || out["result"].(map[string]any)["serverInfo"] == nil {
		t.Fatalf("initialize: %d %v", code, out)
	}

	_, out = rpc(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "tok")
	tools := out["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"get_cluster_info", "list_nodes", "get_node_info", "get_cluster_services",
		"get_node_log", "search_logs", "get_node_metrics", "get_base_info",
		"check_process", "query_yarn", "ssh_exec",
	} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{}).Handler()
	for _, tok := range []string{"", "wrong"} {
		if code, _ := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, tok); code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d want 401", tok, code)
		}
	}
}

func TestNoAuthModeWhenNoTokens(t *testing.T) {
	h := newTestServer(nil, &fakeDS{}).Handler()
	if code, _ := rpc(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""); code != 200 {
		t.Fatalf("token-less dev mode: %d want 200", code)
	}
}

func TestGetClusterInfoRedactsCredentials(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{info: readyCluster()}).Handler()
	text := callText(t, h, "get_cluster_info", `{"cluster_id":"uhadoop-test"}`)

	if strings.Contains(text, "c2VjcmV0") || strings.Contains(text, "passwd") {
		t.Fatalf("credential leaked into cluster info: %s", text)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	if got["cluster_state_name"] != "CS_RUNNING" {
		t.Errorf("state name = %v, want CS_RUNNING", got["cluster_state_name"])
	}
	if !strings.Contains(got["state_hint"].(string), "node operations available") {
		t.Errorf("state hint = %v", got["state_hint"])
	}
	if got["node_count"].(float64) != 3 {
		t.Errorf("node_count = %v", got["node_count"])
	}
}

func TestGetNodeInfoResolvesRoleAndRedacts(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo}).Handler()
	text := callText(t, h, "get_node_info", `{"cluster_id":"uhadoop-test","node_ref":"core1"}`)
	if !strings.Contains(text, "c-core1") {
		t.Fatalf("role core1 not resolved: %s", text)
	}
	if strings.Contains(text, "c2VjcmV0") {
		t.Fatalf("credential leaked: %s", text)
	}
}

func TestGetNodeLogAndPathValidation(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"},
		&fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, logOut: "ERROR boom"}).Handler()

	text := callText(t, h, "get_node_log",
		`{"cluster_id":"uhadoop-test","node_ref":"master1","log_path":"/var/log/hadoop/hdfs.log","keywords":"ERROR"}`)
	if !strings.Contains(text, "ERROR boom") {
		t.Fatalf("log body missing: %s", text)
	}

	if msg := callErr(t, h, "get_node_log",
		`{"cluster_id":"uhadoop-test","log_path":"/var/log/x; rm -rf /"}`); !strings.Contains(msg, "unsafe log path") {
		t.Fatalf("shell metacharacters not rejected: %q", msg)
	}
}

func TestNodeOpsDegradeWhenClusterNotReady(t *testing.T) {
	info := readyCluster()
	info.ClusterState = 13 // CS_CREATING
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{info: info, nodes: info.NodeInfo}).Handler()

	msg := callErr(t, h, "get_node_log", `{"cluster_id":"uhadoop-test","log_path":"/var/log/x.log"}`)
	if !strings.Contains(msg, "CS_CREATING") {
		t.Fatalf("want explicit CS_CREATING degradation, got %q", msg)
	}
	// get_cluster_info must still succeed, with a hint.
	text := callText(t, h, "get_cluster_info", `{"cluster_id":"uhadoop-test"}`)
	if !strings.Contains(text, "CS_CREATING") {
		t.Fatalf("cluster info should describe the state: %s", text)
	}
}

func TestSshExecAllowlist(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, runOut: "PID NAME"}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()

	text := callText(t, h, "ssh_exec", `{"cluster_id":"uhadoop-test","command":"jps -l"}`)
	if !strings.Contains(text, "PID NAME") {
		t.Fatalf("ssh_exec output missing: %s", text)
	}

	for _, bad := range []string{"rm -rf /", "sudo reboot", "echo $(id)", "cat /etc/passwd | sh", "kill -9 1"} {
		args, _ := json.Marshal(map[string]string{"cluster_id": "uhadoop-test", "command": bad})
		if msg := callErr(t, h, "ssh_exec", string(args)); msg == "" {
			t.Errorf("command %q should be rejected", bad)
		}
	}
}

// mysql is admitted through ssh_exec, but only as a single read-only query:
// this is the "command form" operators use to inspect the cluster's databases.
func TestSshExecReadOnlyMySQL(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, runOut: "Database\nhive"}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()

	text := callText(t, h, "ssh_exec",
		`{"cluster_id":"uhadoop-test","command":"mysql -e \"SHOW DATABASES\""}`)
	if !strings.Contains(text, "hive") {
		t.Fatalf("ssh_exec mysql output missing: %s", text)
	}
	if !strings.Contains(ds.lastCmd, "SHOW DATABASES") {
		t.Fatalf("mysql command not forwarded: %s", ds.lastCmd)
	}

	for _, bad := range []string{
		`mysql -e "DROP TABLE t"`,
		`mysql -e "UPDATE t SET a = 1"`,
		`mysql -e "SELECT 1; DELETE FROM t"`,
		`mysql --init-command="SET GLOBAL x=1" -e "SHOW DATABASES"`,
	} {
		args, _ := json.Marshal(map[string]string{"cluster_id": "uhadoop-test", "command": bad})
		if msg := callErr(t, h, "ssh_exec", string(args)); msg == "" {
			t.Errorf("mysql command %q should be rejected", bad)
		}
	}
}

func TestSearchLogsFanOut(t *testing.T) {
	info := readyCluster()
	ds := &fakeDS{info: info, nodes: info.NodeInfo, logOut: "ERROR disk full"}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()

	text := callText(t, h, "search_logs",
		`{"cluster_id":"uhadoop-test","log_path":"/var/log/hdfs.log","keywords":"ERROR","roles":"master,core"}`)
	if n := strings.Count(text, "ERROR disk full"); n != 3 {
		t.Fatalf("expected matches on 3 nodes, got %d:\n%s", n, text)
	}
	if len(ds.logNodes) != 3 {
		t.Fatalf("expected 3 node reads, got %v", ds.logNodes)
	}
}

func TestCheckProcessValidation(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, runOut: "1234 root 0.1 2.0 NameNode"}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()

	if text := callText(t, h, "check_process", `{"cluster_id":"uhadoop-test","process_name":"NameNode"}`); !strings.Contains(text, "NameNode") {
		t.Fatalf("check_process output: %s", text)
	}
	if msg := callErr(t, h, "check_process", `{"cluster_id":"uhadoop-test","process_name":"x; rm -rf /"}`); msg == "" {
		t.Fatal("unsafe process name should be rejected")
	}
}

func TestUnreachableCluster(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{}).Handler()
	if msg := callErr(t, h, "get_cluster_info", `{"cluster_id":"c1"}`); !strings.Contains(msg, "cluster not found") {
		t.Fatalf("want cluster-not-found error, got %q", msg)
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	h := newTestServer(map[string]string{"tok": "diagflow"}, &fakeDS{info: readyCluster()}).Handler()
	if msg := callErr(t, h, "nope", `{"cluster_id":"c1"}`); !strings.Contains(msg, "unknown tool") {
		t.Fatalf("unknown tool: %q", msg)
	}
	if code, _ := rpc(t, h, `{"jsonrpc":"2.0","id":5,"method":"bogus"}`, "tok"); code != 200 {
		t.Fatalf("bogus method: %d", code)
	}
}

func TestNodeHostPrefersIPv6(t *testing.T) {
	n := infra.NodeInfo{NodeIP: "10.59.252.99", NodeIPv6: "2003:da8::1"}
	if got := nodeHost(&n); got != "2003:da8::1" {
		t.Fatalf("nodeHost = %q, want IPv6", got)
	}
	n.NodeIPv6 = ""
	if got := nodeHost(&n); got != "10.59.252.99" {
		t.Fatalf("nodeHost fallback = %q", got)
	}
}

func TestGetNodeLogNoMatchesIsNotAnError(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, logOut: ""}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()
	text := callText(t, h, "get_node_log",
		`{"cluster_id":"uhadoop-test","log_path":"/var/log/x.log","keywords":"NOPE"}`)
	if !strings.Contains(text, "no matching log lines") {
		t.Fatalf("empty result should be reported as no matches: %s", text)
	}
}

func TestSearchLogsReportsNoMatchSeparatelyFromFailure(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo, logOut: ""}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()
	text := callText(t, h, "search_logs",
		`{"cluster_id":"uhadoop-test","log_path":"/var/log/x.log","keywords":"NOPE"}`)
	if !strings.Contains(text, "0/3 node(s) returned matches, 0 failed") {
		t.Fatalf("no-match search should report 0 failed: %s", text)
	}
}

func TestListLogs(t *testing.T) {
	ds := &fakeDS{info: readyCluster(), nodes: readyCluster().NodeInfo,
		runOut: "== /var/log/hadoop-yarn\n-rw-r--r-- 1 hadoop hadoop 55116 yarn-resourcemanager.log"}
	h := newTestServer(map[string]string{"tok": "diagflow"}, ds).Handler()

	text := callText(t, h, "list_logs", `{"cluster_id":"uhadoop-test","node_ref":"master1"}`)
	if !strings.Contains(text, "yarn-resourcemanager.log") {
		t.Fatalf("listing missing: %s", text)
	}
	if !strings.Contains(ds.lastCmd, "/var/log/hadoop-hdfs") {
		t.Fatalf("default dirs not probed: %q", ds.lastCmd)
	}

	// An explicit directory is honoured, and anything outside /var/log rejected.
	callText(t, h, "list_logs", `{"cluster_id":"uhadoop-test","dir":"/var/log/hive"}`)
	if !strings.Contains(ds.lastCmd, "/var/log/hive") || strings.Contains(ds.lastCmd, "hadoop-hdfs") {
		t.Fatalf("explicit dir not used: %q", ds.lastCmd)
	}
	if msg := callErr(t, h, "list_logs", `{"cluster_id":"uhadoop-test","dir":"/etc"}`); !strings.Contains(msg, "unsafe log directory") {
		t.Fatalf("unsafe dir not rejected: %q", msg)
	}
}
