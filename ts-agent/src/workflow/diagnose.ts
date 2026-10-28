import { EventEmitter, type EventSink, type StepKind } from "../events.js";
import { NoConclusionError } from "../errors.js";
import { FingerprintKB } from "../kb/fingerprint.js";
import { renderReport } from "../report/render.js";
import type {
  DiagnosisReport,
  EvidenceItem,
  IntentResult,
  InvestigationInput,
} from "../report/types.js";
import type { AgentRuntime } from "../runtime/types.js";
import { detectErrorPattern, executeStrategy } from "../strategy/execute.js";
import { buildTaskPrompt, loadStrategy } from "../strategy/load.js";
import type { ToolExecutor } from "../tools/executor.js";
import { errorMessage, newEventId, truncate } from "../util.js";
import { validateConclusion } from "../validator/validator.js";
import { buildContext } from "./context.js";

export interface DiagnosisResult {
  report: DiagnosisReport;
  markdown: string;
}

export interface DiagnosisOptions {
  strategiesDir?: string;
  kb?: FingerprintKB;
}

function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function stringContext(context: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(context)) {
    if (typeof value === "string") out[key] = value;
  }
  return out;
}

function summarizeEvidence(evidence: EvidenceItem[]): string {
  return evidence
    .slice(-10)
    .map((item) => `- [${item.source_agent}] ${truncate(item.detail ?? item.summary, 500)}`)
    .join("\n");
}

/**
 * Five-phase deterministic-first diagnosis:
 *   1. fingerprint fast path
 *   2. YAML strategy execution
 *   3. ReAct (LLM fills gaps)
 *   4. synthesis + validation (runtime.synthesize)
 *   5. KB index for high-confidence, non-degraded results
 */
export async function runDiagnosis(
  runtime: AgentRuntime,
  executor: ToolExecutor,
  input: InvestigationInput,
  sink: EventSink,
  options: DiagnosisOptions = {},
): Promise<DiagnosisResult> {
  const emitter = new EventEmitter(sink);
  const startedAt = Date.now();
  const eventId = newEventId("ts-diagnose");
  const context = buildContext(input);
  const warnings: string[] = [];
  const phases: string[] = [];
  const kb = options.kb ?? new FingerprintKB();
  const version = stringOf(context.version);
  const pattern = detectErrorPattern(input, context);

  // ---- Phase 1: fingerprint fast path ----
  if (pattern) {
    const hit = kb.match(input.component, pattern, version);
    if (hit && hit.rootCause && hit.suggestions.length > 0) {
      emitter.step("phase", "KB fingerprint hit — skipping LLM");
      const report: DiagnosisReport = {
        status: "ok",
        event_id: eventId,
        component: input.component,
        problem_type: input.problem,
        root_cause: hit.rootCause,
        confidence: "high",
        evidence_summary: [
          {
            source_agent: "knowledge_base",
            category: "fingerprint_match",
            summary: "命中历史指纹",
            detail: hit.rootCause,
            confidence: 1.0,
          },
        ],
        suggestions: hit.suggestions,
        matched_knowledge: true,
        duration_ms: Date.now() - startedAt,
        llm_calls: 0,
        warnings: [],
        phases_run: ["kb_fast_path"],
      };
      const markdown = renderReport(report);
      emitter.report(report, markdown);
      emitter.done();
      return { report, markdown };
    }
  }

  // ---- Phase 2: YAML strategy ----
  const strategy = loadStrategy(input.component, input.problem, options.strategiesDir);
  emitter.step(
    "phase",
    `strategy: ${strategy.source} (${strategy.steps.length} steps)`,
  );
  const strategyResult = await executeStrategy(strategy, {
    input,
    context,
    executor,
    kb,
    ...(runtime.decide
      ? { decide: (prompt, choices) => runtime.decide!(prompt, choices) }
      : {}),
    emit: (kind: StepKind, text: string, tool?: string) => emitter.step(kind, text, tool),
  });
  phases.push("strategy");
  warnings.push(...strategyResult.warnings);

  const evidence: EvidenceItem[] = [...strategyResult.evidence];
  const actions = [...strategyResult.actions];
  const snippets = [...strategyResult.snippets];

  // ---- Phase 3: ReAct (only gaps) ----
  let reactText = "";
  if (runtime.react) {
    phases.push("react");
    try {
      const react = await runtime.react(
        buildTaskPrompt(strategy, stringContext(context)),
        context,
        summarizeEvidence(evidence),
      );
      reactText = react.text;
      actions.push(...react.actions);
      snippets.push(...react.snippets);
      if (reactText) {
        evidence.push({
          source_agent: "react",
          category: "agent_analysis",
          summary: truncate(reactText, 240),
          detail: reactText,
          confidence: 0.7,
        });
      }
    } catch (error) {
      const message = `ReAct 阶段失败：${errorMessage(error)}`;
      warnings.push(message);
      emitter.step("warn", message);
    }
  }

  // ---- Phase 4: synthesis + deterministic validation ----
  phases.push("validate");
  const trace: IntentResult[] = [
    {
      intent: "deterministic strategy + ReAct",
      judgment: reactText || "策略取证完成",
      suspect_component_bug: false,
      actions,
      snippets,
    },
  ];
  let draft = await runtime.synthesize(input, trace, context);
  let check = validateConclusion(draft.root_cause, draft.suggestions, evidence.length);
  if (!check.ok) {
    emitter.step("warn", `validation failed (${check.feedback}) — retrying`);
    draft = await runtime.synthesize(input, trace, context, check.feedback);
    check = validateConclusion(draft.root_cause, draft.suggestions, evidence.length);
    if (!check.ok) {
      throw new NoConclusionError(`conclusion failed validation: ${check.feedback}`);
    }
  }

  // ---- Phase 5: KB index ----
  const status = warnings.length > 0 ? "degraded" : "ok";
  if (draft.confidence === "high" && status === "ok") {
    kb.add(
      input.component,
      pattern || truncate(draft.root_cause, 80),
      version,
      draft.root_cause,
      draft.suggestions,
    );
    phases.push("kb_index");
  }

  const report: DiagnosisReport = {
    status,
    event_id: eventId,
    component: input.component,
    problem_type: input.problem,
    root_cause: draft.root_cause,
    confidence: draft.confidence,
    evidence_summary: evidence.slice(0, 30),
    suggestions: draft.suggestions,
    matched_knowledge: false,
    duration_ms: Date.now() - startedAt,
    llm_calls: runtime.getStats().llmCalls,
    warnings,
    phases_run: phases,
    trace: {
      component: input.component,
      problem_type: input.problem,
      steps: trace,
    },
  };

  const markdown = renderReport(report);
  emitter.report(report, markdown);
  emitter.done();
  return { report, markdown };
}
