import assert from "node:assert/strict";
import test from "node:test";
import { executeStrategy } from "../src/strategy/execute.js";
import { groupByPriority, loadStrategy, renderParams, shouldRun } from "../src/strategy/load.js";
import type { Step } from "../src/strategy/types.js";
import type { ToolExecutor, ToolResult } from "../src/tools/executor.js";

class FakeExecutor implements ToolExecutor {
  readonly calls: Array<{ tool: string; params: Record<string, unknown>; clusterId?: string }> = [];

  async call(
    tool: string,
    params: Record<string, unknown>,
    options?: { clusterId?: string },
  ): Promise<ToolResult> {
    this.calls.push({ tool, params, clusterId: options?.clusterId });
    if (tool === "ssh_exec") {
      return { success: true, data: "java.lang.OutOfMemoryError: Java heap space" };
    }
    if (tool === "query_yarn") {
      return { success: true, data: "application_1 FAILED" };
    }
    return { success: true, data: `${tool} ok` };
  }

  async close(): Promise<void> {
    return undefined;
  }
}

test("loadStrategy reads the repository YAML and preserves priorities", () => {
  const strategy = loadStrategy("flink", "job_failure");
  assert.equal(strategy.component, "flink");
  assert.equal(strategy.problemType, "job_failure");
  assert.ok(strategy.steps.length >= 5);
  const batches = groupByPriority(strategy.steps);
  assert.equal(batches[0]?.[0]?.priority, 0);
  assert.ok((batches[1]?.length ?? 0) >= 2, "priority 1 steps should share a batch");
});

test("loadStrategy falls back to a component default and then to the built-in plan", () => {
  const fallback = loadStrategy("flink", "no_such_problem");
  assert.match(fallback.source, /flink_default\.yaml$/);

  const builtin = loadStrategy("unknown-component", "no_such_problem", "/nonexistent");
  assert.equal(builtin.source, "default");
  assert.ok(builtin.steps.some((step) => step.tool === "get_cluster_info"));
});

test("renderParams resolves nested context templates", () => {
  const step: Step = {
    action: "tool_call",
    tool: "ssh_exec",
    priority: 1,
    params: {
      node_name: "{{ context.node }}",
      cmd: "grep {{ context.keyword }} {{ context.path }}",
      nested: { app_id: "{{ context.job_id }}" },
    },
  };
  const rendered = renderParams(step, { node: "master1", keyword: "ERROR", path: "/tmp/a.log", job_id: "app-1" });
  assert.equal(rendered.node_name, "master1");
  assert.equal(rendered.cmd, "grep ERROR /tmp/a.log");
  assert.deepEqual(rendered.nested, { app_id: "app-1" });
});

test("shouldRun honors if_decision", () => {
  const step: Step = { action: "tool_call", params: {}, priority: 1, if_decision: "other_component" };
  assert.equal(shouldRun(step, "other_component"), true);
  assert.equal(shouldRun(step, "flink_only"), false);
  assert.equal(shouldRun({ ...step, if_decision: undefined }, ""), true);
});

test("executeStrategy runs batches, branches on a decision, and collects evidence", async () => {
  const strategy = loadStrategy("flink", "job_failure");
  const executor = new FakeExecutor();
  const result = await executeStrategy(strategy, {
    input: { component: "flink", problem: "job_failure", clusterId: "uhadoop-test", jobId: "app-1" },
    context: { cluster_id: "uhadoop-test", job_id: "app-1", version: "1.17" },
    executor,
    decide: async () => "flink_only",
    emit: () => undefined,
  });

  assert.ok(executor.calls.some((call) => call.tool === "query_yarn"));
  assert.ok(executor.calls.some((call) => call.tool === "ssh_exec"));
  assert.ok(result.evidence.length > 0);
  assert.equal(result.decisions[Object.keys(result.decisions)[0] ?? ""], "flink_only");
  // Branch A (other_component) steps must not run when the decision is flink_only.
  assert.ok(!executor.calls.some((call) => call.tool === "call_umr_agent" && call.params.action === "GetBaseInfo"));
});

test("executeStrategy records a tool failure as low-confidence evidence", async () => {
  const executor: ToolExecutor = {
    async call(): Promise<ToolResult> {
      return { success: false, data: "", error: "node unreachable" };
    },
    async close() {
      return undefined;
    },
  };
  const strategy = loadStrategy("flink", "job_failure");
  const result = await executeStrategy(strategy, {
    input: { component: "flink", problem: "job_failure", clusterId: "c" },
    context: { cluster_id: "c" },
    executor,
    emit: () => undefined,
  });
  assert.ok(result.evidence.some((item) => item.confidence === 0.2));
});
