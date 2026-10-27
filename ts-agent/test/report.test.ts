import assert from "node:assert/strict";
import test from "node:test";
import { renderReport } from "../src/report/render.js";
import type { DiagnosisReport } from "../src/report/types.js";

test("renderReport includes root cause, suggestions, evidence and trace", () => {
  const report: DiagnosisReport = {
    status: "ok",
    event_id: "ts-diag-1",
    component: "flink",
    problem_type: "job_failure",
    root_cause: "TaskManager heap exhausted (OutOfMemoryError)",
    confidence: "high",
    evidence_summary: [
      {
        source_agent: "mcp__diagflow__get_node_log",
        category: "tool_result",
        summary: "java.lang.OutOfMemoryError: Java heap space",
      },
    ],
    suggestions: ["increase taskmanager.heap.size", "reduce slot count"],
    matched_knowledge: false,
    duration_ms: 1234,
    llm_calls: 3,
    warnings: [],
    phases_run: ["intent_plan", "intent_execute", "synthesize"],
    trace: {
      component: "flink",
      problem_type: "job_failure",
      steps: [
        {
          intent: "check TaskManager log",
          judgment: "TaskManager OOM",
          suspect_component_bug: false,
          actions: [{ tool: "mcp__diagflow__get_node_log", params: {} }],
          snippets: ["OutOfMemoryError"],
        },
      ],
    },
  };

  const markdown = renderReport(report);
  assert.match(markdown, /# 诊断报告: flink\.job_failure/);
  assert.match(markdown, /TaskManager heap exhausted/);
  assert.match(markdown, /increase taskmanager\.heap\.size/);
  assert.match(markdown, /mcp__diagflow__get_node_log/);
  assert.match(markdown, /## 排查过程/);
});
