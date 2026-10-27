import { EventEmitter, type EventSink } from "../events.js";
import { NoConclusionError } from "../errors.js";
import { renderReport } from "../report/render.js";
import type {
  DiagnosisReport,
  EvidenceItem,
  InvestigationInput,
  InvestigationStep,
} from "../report/types.js";
import { errorMessage, newEventId, truncate } from "../util.js";
import type { AgentRuntime } from "../runtime/types.js";

export interface InvestigationResult {
  report: DiagnosisReport;
  markdown: string;
}

function buildContext(input: InvestigationInput): Record<string, unknown> {
  return {
    ...(input.context ?? {}),
    component: input.component,
    problem: input.problem,
    ...(input.clusterId ? { cluster_id: input.clusterId } : {}),
    ...(input.jobId ? { job_id: input.jobId } : {}),
    ...(input.detail ? { detail: input.detail } : {}),
    ...(input.alert ? { alert: input.alert } : {}),
    ...(input.timeRange ? { time_range: input.timeRange } : {}),
  };
}

function buildEvidence(steps: InvestigationStep[]): EvidenceItem[] {
  const evidence: EvidenceItem[] = [];
  for (const step of steps) {
    evidence.push({
      source_agent: "investigation",
      category: "judgment",
      summary: truncate(step.judgment, 240),
      detail: step.judgment,
      confidence: step.suspect_component_bug ? 0.6 : 0.7,
    });
    for (const snippet of step.snippets.slice(0, 3)) {
      evidence.push({
        source_agent: step.actions[0]?.tool ?? "investigation",
        category: "tool_result",
        summary: truncate(snippet, 240),
        detail: truncate(snippet, 2_000),
        confidence: 0.8,
      });
    }
  }
  return evidence.slice(0, 20);
}

/**
 * Intent-driven monitoring investigation.
 *
 * Mirrors the Go `Investigate` path: propose intents, execute each one, then
 * synthesize a structured report. A failure to produce a verifiable conclusion
 * is an error, never a fabricated root cause.
 */
export async function runInvestigation(
  runtime: AgentRuntime,
  input: InvestigationInput,
  sink: EventSink,
): Promise<InvestigationResult> {
  const emitter = new EventEmitter(sink);
  const startedAt = Date.now();
  const eventId = newEventId();
  const warnings: string[] = [];
  const context = buildContext(input);

  emitter.step("phase", `开始排查: ${input.component}/${input.problem}`);
  const intents = await runtime.proposeIntents(input, context);
  if (intents.length === 0) {
    warnings.push("排查计划为空");
    throw new NoConclusionError("investigation plan produced no intents");
  }

  const steps: InvestigationStep[] = [];
  for (const [index, intent] of intents.entries()) {
    emitter.step("phase", `意图 ${index + 1}/${intents.length}: ${intent.intent}`);
    try {
      const result = await runtime.runIntent(input, intent, context);
      steps.push({
        intent: result.intent,
        ...(result.reason ? { reason: result.reason } : {}),
        judgment: result.judgment,
        suspect_component_bug: result.suspect_component_bug,
        actions: result.actions,
        snippets: result.snippets,
      });
    } catch (error) {
      const message = `排查步骤失败：${errorMessage(error)}`;
      warnings.push(message);
      emitter.step("warn", message);
    }
  }

  if (steps.length === 0) {
    throw new NoConclusionError("no investigation step completed; no evidence to synthesize");
  }

  emitter.step("phase", "合成诊断报告");
  const draft = await runtime.synthesize(input, steps, context);
  const stats = runtime.getStats();
  const durationMs = Date.now() - startedAt;

  const report: DiagnosisReport = {
    status: warnings.length > 0 ? "degraded" : "ok",
    event_id: eventId,
    component: input.component,
    problem_type: input.problem,
    root_cause: draft.root_cause,
    confidence: draft.confidence,
    evidence_summary: buildEvidence(steps),
    suggestions: draft.suggestions,
    matched_knowledge: false,
    duration_ms: durationMs,
    llm_calls: stats.llmCalls,
    warnings,
    phases_run: ["intent_plan", "intent_execute", "synthesize"],
    trace: {
      component: input.component,
      problem_type: input.problem,
      steps,
    },
  };

  const markdown = renderReport(report);
  emitter.report(report, markdown);
  emitter.done();
  return { report, markdown };
}
