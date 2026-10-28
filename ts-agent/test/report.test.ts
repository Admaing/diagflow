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

function report(overrides: Partial<DiagnosisReport>): DiagnosisReport {
  return {
    status: "ok",
    event_id: "ts-diag-1",
    component: "hdfs",
    problem_type: "job_failure",
    root_cause: "x",
    confidence: "low",
    evidence_summary: [],
    suggestions: [],
    matched_knowledge: false,
    duration_ms: 1,
    llm_calls: 1,
    warnings: [],
    phases_run: ["synthesize"],
    ...overrides,
  };
}

test("a health_check report is rendered as a status report, not a root cause", () => {
  const markdown = renderReport(
    report({
      problem_type: "health_check",
      root_cause: "NameNode 主备均存活，未见 safemode",
      confidence: "high",
      suggestions: ["暂无需要处理的项目"],
    }),
  );
  assert.match(markdown, /# 状态报告: hdfs/);
  assert.match(markdown, /\*\*问题类型\*\*: 状态查看/);
  assert.match(markdown, /## 当前状态/);
  assert.match(markdown, /\*\*状态\*\*: NameNode 主备均存活/);
  assert.match(markdown, /## 注意事项/);
  assert.doesNotMatch(markdown, /根因分析/);
  assert.doesNotMatch(markdown, /修复建议/);
});

test("evidence whose summary repeats the detail is printed once", () => {
  const judgment = "master1 与 master2 上 NameNode 进程均存活，两节点均为 NS_RUNNING。";
  const markdown = renderReport(
    report({
      evidence_summary: [
        { source_agent: "investigation", category: "judgment", summary: judgment, detail: judgment },
      ],
    }),
  );
  const occurrences = markdown.split("NameNode 进程均存活").length - 1;
  assert.equal(occurrences, 1, "the same text must not be rendered as both summary and detail");
});

test("a short summary keeps the full detail as an expandable block", () => {
  const detail = "第一行结论。\n第二行：详细堆栈\njava.lang.OutOfMemoryError: Java heap space";
  const markdown = renderReport(
    report({
      evidence_summary: [
        { source_agent: "react", category: "agent_analysis", summary: "第一行结论。", detail },
      ],
    }),
  );
  assert.match(markdown, /第一行结论。/);
  assert.match(markdown, /<details>/);
  assert.match(markdown, /java\.lang\.OutOfMemoryError/);
});

test("firstSentence keeps summaries distinct from their detail", async () => {
  const { firstSentence, isRedundantSummary } = await import("../src/util.js");
  const long = "名称节点正常。第二句同样很长，不应该出现在摘要里。";
  assert.equal(firstSentence(long), "名称节点正常。");
  assert.equal(isRedundantSummary(firstSentence(long), long), false);

  const single = "没有任何句号的一整句话";
  assert.equal(firstSentence(single), single);
  assert.equal(isRedundantSummary(firstSentence(single), single), true);

  assert.equal(firstSentence("  多空格\n换行  的文本。 后面还有  "), "多空格 换行 的文本。");
  assert.equal(firstSentence(""), "");
});
