import type { AgentConfig } from "../config.js";
import type { EventSink } from "../events.js";
import type {
  Intent,
  IntentResult,
  InvestigationInput,
  ReportDraft,
} from "../report/types.js";

export interface RuntimeStats {
  llmCalls: number;
  toolCalls: number;
}

export interface AgentRuntime {
  proposeIntents(
    input: InvestigationInput,
    context: Record<string, unknown>,
  ): Promise<Intent[]>;

  runIntent(
    input: InvestigationInput,
    intent: Intent,
    context: Record<string, unknown>,
  ): Promise<IntentResult>;

  synthesize(
    input: InvestigationInput,
    trace: IntentResult[],
    context: Record<string, unknown>,
  ): Promise<ReportDraft>;

  getStats(): RuntimeStats;

  close(): Promise<void>;
}

export type RuntimeFactory = (config: AgentConfig, sink: EventSink) => Promise<AgentRuntime>;
