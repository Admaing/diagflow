import type { AgentConfig } from "../config.js";
import type { EventSink } from "../events.js";
import type {
  Intent,
  IntentResult,
  InvestigationInput,
  ReportDraft,
  ToolAction,
} from "../report/types.js";
import type { DecideChoice } from "../strategy/types.js";

export interface RuntimeStats {
  llmCalls: number;
  toolCalls: number;
}

export interface ReactResult {
  text: string;
  actions: ToolAction[];
  snippets: string[];
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
    feedback?: string,
  ): Promise<ReportDraft>;

  /** Optional: pick a YAML strategy branch. */
  decide?(prompt: string, choices: DecideChoice[]): Promise<string>;

  /** Optional: free-form evidence gathering after the deterministic phase. */
  react?(
    task: string,
    context: Record<string, unknown>,
    evidenceSummary: string,
  ): Promise<ReactResult>;

  getStats(): RuntimeStats;

  close(): Promise<void>;
}

export type RuntimeFactory = (config: AgentConfig, sink: EventSink) => Promise<AgentRuntime>;
