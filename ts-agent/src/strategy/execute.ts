import type { StepKind } from "../events.js";
import type { FingerprintKB } from "../kb/fingerprint.js";
import type { EvidenceItem, InvestigationInput, ToolAction } from "../report/types.js";
import type { ToolExecutor } from "../tools/executor.js";
import { errorMessage, firstSentence, truncate } from "../util.js";
import { groupByPriority, renderParams, shouldRun } from "./load.js";
import type { DecideChoice, Step, Strategy } from "./types.js";

export interface StrategyRunOptions {
  input: InvestigationInput;
  context: Record<string, unknown>;
  executor: ToolExecutor;
  kb?: FingerprintKB;
  decide?: (prompt: string, choices: DecideChoice[]) => Promise<string>;
  emit: (kind: StepKind, text: string, tool?: string) => void;
}

export interface StrategyRunResult {
  strategy: Strategy;
  evidence: EvidenceItem[];
  actions: ToolAction[];
  snippets: string[];
  decisions: Record<string, string>;
  warnings: string[];
}

function stringOf(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

export function detectErrorPattern(
  input: InvestigationInput,
  context: Record<string, unknown>,
): string {
  for (const key of ["error_pattern", "error", "exception"]) {
    const value = stringOf(context[key]).trim();
    if (value) return value;
  }
  const haystack = [
    input.problem,
    input.detail ?? "",
    stringOf(context.detail),
    stringOf(context.problem_desc),
  ]
    .join(" ")
    .toLowerCase();
  const keywords = [
    "OutOfMemoryError",
    "OOM",
    "FATAL",
    "CheckpointExpired",
    "NoSpaceLeft",
    "disk full",
    "backpressure",
    "timeout",
    "GC overhead",
    "Connection refused",
    "NoRouteToHost",
    "FileNotFound",
    "Permission denied",
  ];
  return keywords.find((keyword) => haystack.includes(keyword.toLowerCase())) ?? "";
}

async function runToolStep(
  step: Step,
  options: StrategyRunOptions,
  evidence: EvidenceItem[],
  actions: ToolAction[],
  snippets: string[],
): Promise<void> {
  if (!step.tool) {
    const message = "strategy step has no tool";
    evidence.push({ source_agent: "strategy", category: "error", summary: message, confidence: 0.0 });
    options.emit("warn", message);
    return;
  }

  const params = renderParams(step, options.context);
  const clusterId = stringOf(options.context.cluster_id) || undefined;
  options.emit("tool_call", `${step.tool}(${truncate(JSON.stringify(params), 300)})`, step.tool);

  try {
    const result = await options.executor.call(step.tool, params, { clusterId });
    const detail = result.success ? result.data : (result.error ?? "tool failed");
    actions.push({ tool: step.tool, params, result: truncate(detail, 1_000) });
    if (result.success) {
      snippets.push(truncate(detail, 8_000));
      evidence.push({
        source_agent: `strategy:${step.tool}`,
        category: `tool:${step.tool}`,
        summary: firstSentence(detail, 120),
        detail,
        confidence: 0.8,
      });
      options.emit("tool_result", `${step.tool}: ${truncate(detail, 300)}`, step.tool);
    } else {
      evidence.push({
        source_agent: `strategy:${step.tool}`,
        category: `tool:${step.tool}`,
        summary: `${step.tool} failed: ${truncate(detail, 200)}`,
        detail,
        confidence: 0.2,
      });
      options.emit("tool_error", `${step.tool}: ${truncate(detail, 300)}`, step.tool);
    }
  } catch (error) {
    const message = `${step.tool} failed: ${errorMessage(error)}`;
    evidence.push({
      source_agent: `strategy:${step.tool}`,
      category: `tool:${step.tool}`,
      summary: message,
      confidence: 0.0,
    });
    options.emit("tool_error", message, step.tool);
  }
}

function runFingerprintStep(
  options: StrategyRunOptions,
  evidence: EvidenceItem[],
): void {
  if (!options.kb) return;
  const component = options.input.component;
  const pattern = detectErrorPattern(options.input, options.context);
  const version = stringOf(options.context.version);
  const hit = options.kb.match(component, pattern, version);
  if (!hit) {
    options.emit("log", `fingerprint_match: no known case for ${component}`);
    return;
  }
  evidence.push({
    source_agent: "strategy:fingerprint_match",
    category: "fingerprint_match",
    summary: `命中历史指纹: ${truncate(hit.rootCause, 160)}`,
    detail: hit.rootCause,
    confidence: 0.95,
  });
  options.emit("log", `fingerprint_match hit: ${truncate(hit.rootCause, 120)}`);
}

/**
 * Execute a YAML strategy: steps in the same priority batch run concurrently,
 * then any `llm_decide` step in that batch picks the branch for later steps.
 */
export async function executeStrategy(
  strategy: Strategy,
  options: StrategyRunOptions,
): Promise<StrategyRunResult> {
  const evidence: EvidenceItem[] = [];
  const actions: ToolAction[] = [];
  const snippets: string[] = [];
  const decisions: Record<string, string> = {};
  const warnings: string[] = [];
  let currentDecision = "";

  for (const batch of groupByPriority(strategy.steps)) {
    const runnable = batch.filter((step) => shouldRun(step, currentDecision));
    const toolSteps = runnable.filter((step) => step.action === "tool_call");
    const fingerprintSteps = runnable.filter((step) => step.action === "fingerprint_match");
    const decideSteps = runnable.filter((step) => step.action === "llm_decide");

    for (const step of fingerprintSteps) runFingerprintStep(options, evidence);
    await Promise.all(
      toolSteps.map((step) => runToolStep(step, options, evidence, actions, snippets)),
    );

    for (const step of decideSteps) {
      const prompt = [
        step.decide_prompt ?? "Choose the most likely branch.",
        "",
        "Evidence collected so far:",
        evidence
          .slice(-8)
          .map((item) => `- [${item.source_agent}] ${truncate(item.detail ?? item.summary, 400)}`)
          .join("\n") || "(none)",
        "",
        "Choices:",
        ...(step.decide_choices ?? []).map((choice) => `- ${choice.value}: ${choice.description ?? ""}`),
      ].join("\n");

      if (!options.decide) {
        warnings.push("llm_decide skipped: no decision provider");
        continue;
      }
      try {
        const decision = await options.decide(prompt, step.decide_choices ?? []);
        if (decision) {
          currentDecision = decision;
          decisions[step.description ?? step.decide_prompt ?? "decision"] = decision;
          options.emit("phase", `LLM decided: ${decision}`);
        }
      } catch (error) {
        warnings.push(`llm_decide failed: ${errorMessage(error)}`);
      }
    }
  }

  return { strategy, evidence, actions, snippets, decisions, warnings };
}
