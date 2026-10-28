import assert from "node:assert/strict";
import test from "node:test";
import { resolveStrategyTool } from "../src/mcp/tool-client.js";

test("call_umr_agent GetLogs maps to get_node_log", () => {
  const resolved = resolveStrategyTool(
    "call_umr_agent",
    {
      node_name: "core1",
      action: "GetLogs",
      params: { Path: "/data/flink/log/taskmanager.log", Keywords: "ERROR,OOM", MaxLines: "100" },
    },
    { clusterId: "uhadoop-1" },
  );
  assert.deepEqual(resolved, {
    server: "diagflow",
    tool: "get_node_log",
    args: {
      cluster_id: "uhadoop-1",
      node_ref: "core1",
      log_path: "/data/flink/log/taskmanager.log",
      keywords: "ERROR,OOM",
      max_lines: 100,
    },
  });
});

test("query_metrics maps to get_node_metrics and injects cluster_id", () => {
  const resolved = resolveStrategyTool("query_metrics", { node_name: "master1" }, { clusterId: "c" });
  assert.deepEqual(resolved, {
    server: "diagflow",
    tool: "get_node_metrics",
    args: { cluster_id: "c", node_ref: "master1" },
  });
});

test("query_yarn maps app_name to states", () => {
  const resolved = resolveStrategyTool(
    "query_yarn",
    { action: "list_apps", app_name: "RUNNING,FAILED" },
    { clusterId: "c" },
  );
  assert.deepEqual(resolved, {
    server: "diagflow",
    tool: "query_yarn",
    args: { cluster_id: "c", action: "list_apps", states: "RUNNING,FAILED" },
  });
});

test("ssh_exec maps cmd/command and requires a cluster id", () => {
  const resolved = resolveStrategyTool(
    "ssh_exec",
    { node_name: "master1", cmd: "grep ERROR /tmp/a.log" },
    { clusterId: "c" },
  );
  assert.deepEqual(resolved, {
    server: "diagflow",
    tool: "ssh_exec",
    args: { cluster_id: "c", command: "grep ERROR /tmp/a.log", node_ref: "master1" },
  });

  const missing = resolveStrategyTool("ssh_exec", { cmd: "grep x /tmp/a.log" });
  assert.equal(typeof missing, "string");
  assert.match(missing as string, /cluster_id is required/);
});

test("prom tools stay on the prom server", () => {
  const resolved = resolveStrategyTool("prom_query", { query: "up" });
  assert.deepEqual(resolved, { server: "prom", tool: "prom_query", args: { query: "up" } });
});

test("deepwiki_query is reported as unsupported", () => {
  const resolved = resolveStrategyTool("deepwiki_query", { component: "flink" }, { clusterId: "c" });
  assert.equal(typeof resolved, "string");
  assert.match(resolved as string, /not wired/);
});
