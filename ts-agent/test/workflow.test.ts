import assert from "node:assert/strict";
import test from "node:test";
import { NoConclusionError } from "../src/errors.js";
import type { AgentEvent } from "../src/events.js";
import type { Intent, IntentResult, InvestigationInput, ReportDraft } from "../src/report/types.js";
import type { AgentRuntime, RuntimeStats } from "../src/runtime/types.js";
import { runInvestigation } from "../src/workflow/investigate.js";

class FakeRuntime implements AgentRuntime {
  constructor(
    private readonly draft: ReportDraft | Error = {
      root_cause: "TaskManager heap exhausted (OutOfMemoryError)",
      confidence: "high",
      suggestions: ["increase heap", "reduce slots"],
    },
  ) {}

  async proposeIntents(): Promise<Intent[]> {
    return [{ intent: "check TaskManager log", reason: "OOM reported" }];
  }

  async runIntent(_input: InvestigationInput, intent: Intent): Promise<IntentResult> {
    return {
      intent: intent.intent,
      judgment: "TaskManager OOM",
      suspect_component_bug: false,
      actions: [{ tool: "mcp__diagflow__get_node_log", params: { log_path: "/tmp/tm.log" } }],
      snippets: ["java.lang.OutOfMemoryError: Java heap space"],
    };
  }

  async synthesize(): Promise<ReportDraft> {
    if (this.draft instanceof Error) throw this.draft;
    return this.draft;
  }

  getStats(): RuntimeStats {
    return { llmCalls: 3, toolCalls: 1 };
  }

  async close(): Promise<void> {
    return undefined;
  }
}

test("runInvestigation produces a report with evidence and trace", async () => {
  const events: AgentEvent[] = [];
  const { report, markdown } = await runInvestigation(
    new FakeRuntime(),
    { component: "flink", problem: "job_failure", clusterId: "uhadoop-test" },
    (event) => events.push(event),
  );

  assert.equal(report.status, "ok");
  assert.equal(report.component, "flink");
  assert.equal(report.root_cause, "TaskManager heap exhausted (OutOfMemoryError)");
  assert.equal(report.trace?.steps.length, 1);
  assert.ok(report.evidence_summary.length >= 2);
  assert.ok(markdown.includes("TaskManager heap exhausted"));
  assert.ok(events.some((event) => event.type === "report"));
  assert.ok(events.some((event) => event.type === "done"));
});

test("runInvestigation refuses to fabricate a conclusion", async () => {
  await assert.rejects(
    runInvestigation(
      new FakeRuntime(new NoConclusionError("synthesis returned no verifiable conclusion")),
      { component: "flink", problem: "job_failure" },
      () => undefined,
    ),
    (error: unknown) => error instanceof NoConclusionError,
  );
});
