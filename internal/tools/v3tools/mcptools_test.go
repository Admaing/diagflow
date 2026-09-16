package v3tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Admaing/diagflow/internal/infra"
	"github.com/Admaing/diagflow/internal/mcpclient"
	"github.com/Admaing/diagflow/internal/simulated"
)

func testCluster(t *testing.T) *simulated.Cluster {
	t.Helper()
	c, err := simulated.NewCluster("flink_oom")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestQueryNodeLogRoutesThroughMCP(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	srv := fakeMCPServer(t, "ERROR boom", &gotName, &gotArgs)
	defer srv.Close()

	c := testCluster(t)
	def := findDef(t, BuildV3Tools(c, nil, mcpclient.New(srv.URL, "tok"), nil), "query_node_log")
	res, err := def.Handler(context.Background(), map[string]any{
		"node_name": "core1",
		"log_path":  "/var/log/hadoop-hdfs/hdfs.log",
		"keywords":  "ERROR",
		"max_lines": float64(50),
	})
	if err != nil || !res.Success {
		t.Fatalf("query_node_log failed: %v / %s", err, res.Error)
	}
	if gotName != "get_node_log" {
		t.Fatalf("expected MCP get_node_log, got %s", gotName)
	}
	if gotArgs["cluster_id"] != c.Context()["cluster_id"] {
		t.Fatalf("cluster_id not forwarded: %v", gotArgs)
	}
	if gotArgs["node_ref"] != "core1" || gotArgs["keywords"] != "ERROR" {
		t.Fatalf("args not forwarded: %v", gotArgs)
	}
	if !strings.Contains(res.Data, "ERROR boom") {
		t.Fatalf("expected log body, got %q", res.Data)
	}
}

func TestClusterInfoRoutesThroughMCP(t *testing.T) {
	var gotName string
	srv := fakeMCPServer(t, `{"cluster_state_name":"CS_RUNNING"}`, &gotName, nil)
	defer srv.Close()

	def := findDef(t, BuildV3Tools(testCluster(t), nil, mcpclient.New(srv.URL, "tok"), nil), "get_cluster_info")
	res, err := def.Handler(context.Background(), map[string]any{})
	if err != nil || !res.Success {
		t.Fatalf("get_cluster_info failed: %v / %s", err, res.Error)
	}
	if gotName != "get_cluster_info" {
		t.Fatalf("expected MCP get_cluster_info, got %s", gotName)
	}
	if !strings.Contains(res.Data, "CS_RUNNING") {
		t.Fatalf("expected cluster state, got %q", res.Data)
	}
}

func TestListNodesAndSearchLogsRouteThroughMCP(t *testing.T) {
	for _, tc := range []struct {
		tool     string
		args     map[string]any
		wantTool string
	}{
		{"list_nodes", map[string]any{"role": "master"}, "list_nodes"},
		{"search_logs", map[string]any{"log_path": "/var/log/x.log", "keywords": "ERROR", "roles": "core"}, "search_logs"},
		{"list_logs", map[string]any{"node_name": "master1"}, "list_logs"},
		{"ssh_exec", map[string]any{"node_name": "master1", "cmd": "jps -l"}, "ssh_exec"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var gotName string
			var gotArgs map[string]any
			srv := fakeMCPServer(t, "gateway reply", &gotName, &gotArgs)
			defer srv.Close()

			def := findDef(t, BuildV3Tools(testCluster(t), nil, mcpclient.New(srv.URL, "tok"), nil), tc.tool)
			res, err := def.Handler(context.Background(), tc.args)
			if err != nil || !res.Success {
				t.Fatalf("%s failed: %v / %s", tc.tool, err, res.Error)
			}
			if gotName != tc.wantTool {
				t.Fatalf("expected MCP tool %s, got %s", tc.wantTool, gotName)
			}
			if gotArgs["cluster_id"] == "" || gotArgs["cluster_id"] == nil {
				t.Fatalf("cluster_id not forwarded: %v", gotArgs)
			}
		})
	}
}

func TestSearchLogsRequiresKeywords(t *testing.T) {
	srv := fakeMCPServer(t, "unused", nil, nil)
	defer srv.Close()
	def := findDef(t, BuildV3Tools(testCluster(t), nil, mcpclient.New(srv.URL, "tok"), nil), "search_logs")
	res, _ := def.Handler(context.Background(), map[string]any{"log_path": "/var/log/x.log"})
	if res.Success {
		t.Fatal("search_logs without keywords should fail")
	}
}

// Without a gateway the cluster tools must say so explicitly rather than
// silently returning fabricated evidence.
func TestClusterToolsDegradeWithoutGateway(t *testing.T) {
	c := testCluster(t)
	for _, name := range []string{"get_cluster_info", "list_nodes", "search_logs", "list_logs"} {
		def := findDef(t, BuildV3Tools(c, nil, nil, nil), name)
		res, err := def.Handler(context.Background(), map[string]any{
			"log_path": "/var/log/x.log", "keywords": "ERROR",
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(res.Data, "not configured") {
			t.Fatalf("%s should degrade explicitly, got %q", name, res.Data)
		}
	}
}

func TestRealClusterNodeMapShapes(t *testing.T) {
	// FindNode on an unloaded cluster must not panic and must report nothing.
	real := infra.NewRealCluster("inst-1", "test03", "test03-01", nil, nil)
	if n := real.FindNode("master1"); n != nil {
		t.Fatalf("expected nil node before load, got %v", n)
	}
	if !strings.Contains(real.Summary(), "inst-1") {
		t.Fatalf("summary: %s", real.Summary())
	}
}
