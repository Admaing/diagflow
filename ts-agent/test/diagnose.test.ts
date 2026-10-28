import assert from "node:assert/strict";
import test from "node:test";
import { NoConclusionError } from "../src/errors.js";
import { FingerprintKB } from "../src/kb/fingerprint.js";
import type { ToolExecutor, ToolResult } from "../src/tools/executor.js";
import type { AgentRuntime, ReactResult, RuntimeStats } from "../src/runtime/types.js";
import type { Intent, IntentResult, InvestigationInput, ReportDraft } from "../src/report/types.js";
import { runDiagnosis } from "../src/workflow/diagnose.js";

class FakeRuntime implements AgentRuntime {
  constructor(
    private readonly options: { failReact?: boolean; failSynthesis?: boolean } = {},
  ) {}

  async proposeIntents(): Promise<Intent[]> {
    return [];
  }

  async runIntent(): Promise<IntentResult> {
    throw new Error("not used by diagnose");
  }

  async synthesize(): Promise<ReportDraft> {
    if (this.options.failSynthesis) {
      throw new NoConclusionError("synthesis returned no verifiable conclusion");
    }
    return {
      root_cause: "TaskManager heap exhausted (OutOfMemoryError)",
      confidence: "high",
      suggestions: ["increase taskmanager.heap.size", "reduce slots"],
    };
  }

  async decide(): Promise<string> {
    return "flink_only";
  }

  async react(): Promise<ReactResult> {
    if (this.options.failReact) throw new Error("react provider down");
    return {
      text: "TaskManager OOM confirmed by strategy logs",
      actions: [],
      snippets: ["java.lang.OutOfMemoryError: Java heap space"],
    };
  }

  getStats(): RuntimeStats {
    return { llmCalls: 4, toolCalls: 2 };
  }

  async close(): Promise<void> {
    return undefined;
  }
}

class FakeExecutor implements ToolExecutor {
  calls = 0;

  async call(tool: string): Promise<ToolResult> {
    this.calls++;
    return { success: true, data: `${tool} => java.lang.OutOfMemoryError` };
  }

  async close(): Promise<void> {
    return undefined;
  }
}

const input: InvestigationInput = {
  component: "flink",
  problem: "job_failure",
  clusterId: "uhadoop-test",
  jobId: "application_1",
  context: { version: "1.17" },
};

test("runDiagnosis runs strategy, ReAct, synthesis and KB indexing", async () => {
  const executor = new FakeExecutor();
  const { report, markdown } = await runDiagnosis(
    new FakeRuntime(),
    executor,
    input,
    () => undefined,
  );

  assert.equal(report.status, "ok");
  assert.equal(report.root_cause, "TaskManager heap exhausted (OutOfMemoryError)");
  assert.ok(report.phases_run.includes("strategy"));
  assert.ok(report.phases_run.includes("react"));
  assert.ok(report.phases_run.includes("validate"));
  assert.ok(report.phases_run.includes("kb_index"));
  assert.ok(report.evidence_summary.length > 0);
  assert.ok(executor.calls > 0);
  assert.ok(markdown.includes("TaskManager heap exhausted"));
});

test("runDiagnosis returns the fingerprint fast path without touching tools", async () => {
  const kb = new FingerprintKB();
  kb.add(
    "flink",
    "OutOfMemoryError",
    "1.17",
    "Known Flink OOM: heap too small",
    ["increase heap"],
  );
  const executor = new FakeExecutor();
  const { report } = await runDiagnosis(
    new FakeRuntime(),
    executor,
    { ...input, context: { version: "1.17", error_pattern: "OutOfMemoryError" } },
    () => undefined,
    { kb },
  );

  assert.equal(report.matched_knowledge, true);
  assert.deepEqual(report.phases_run, ["kb_fast_path"]);
  assert.equal(report.root_cause, "Known Flink OOM: heap too small");
  assert.equal(executor.calls, 0);
});

test("runDiagnosis degrades when ReAct fails but still synthesizes", async () => {
  const { report } = await runDiagnosis(
    new FakeRuntime({ failReact: true }),
    new FakeExecutor(),
    input,
    () => undefined,
  );
  assert.equal(report.status, "degraded");
  assert.ok(report.warnings.some((warning) => warning.includes("ReAct")));
  assert.ok(!report.phases_run.includes("kb_index"));
});

test("runDiagnosis propagates a no-conclusion synthesis", async () => {
  await assert.rejects(
    runDiagnosis(
      new FakeRuntime({ failSynthesis: true }),
      new FakeExecutor(),
      input,
      () => undefined,
    ),
    (error: unknown) => error instanceof NoConclusionError,
  );
});
