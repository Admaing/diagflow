export type Confidence = "high" | "medium" | "low";
export type ReportStatus = "ok" | "degraded";

/**
 * `health_check` means the user asked about current state ("NameNode 现在是
 * 什么情况") rather than reporting a failure. It must not be rendered as a root
 * cause, or a healthy cluster reads as an unanswered incident.
 */
export const INQUIRY_PROBLEM = "health_check";

export function isInquiry(problem: string): boolean {
  return problem.trim().toLowerCase() === INQUIRY_PROBLEM;
}

export interface EvidenceItem {
  source_agent: string;
  category?: string;
  /** One line, shown collapsed. Never a prefix of `detail`. */
  summary: string;
  /** Full text, shown in an expandable block. */
  detail?: string;
  confidence?: number;
}

export interface ToolAction {
  tool: string;
  params: Record<string, unknown>;
  result?: string;
}

export interface InvestigationStep {
  intent: string;
  reason?: string;
  judgment: string;
  suspect_component_bug: boolean;
  actions: ToolAction[];
  snippets: string[];
  matched_historical?: boolean;
}

export interface InvestigationTrace {
  component: string;
  problem_type: string;
  steps: InvestigationStep[];
}

export interface DiagnosisReport {
  status: ReportStatus;
  event_id: string;
  component: string;
  problem_type: string;
  root_cause: string;
  confidence: Confidence;
  evidence_summary: EvidenceItem[];
  suggestions: string[];
  matched_knowledge: boolean;
  duration_ms: number;
  llm_calls: number;
  warnings: string[];
  phases_run: string[];
  trace?: InvestigationTrace;
}

export interface InvestigationInput {
  component: string;
  problem: string;
  clusterId?: string;
  detail?: string;
  jobId?: string;
  alert?: Record<string, unknown>;
  timeRange?: { start: string; end: string };
  context?: Record<string, unknown>;
}

export interface Intent {
  intent: string;
  reason?: string;
}

export interface IntentResult {
  intent: string;
  reason?: string;
  judgment: string;
  suspect_component_bug: boolean;
  actions: ToolAction[];
  snippets: string[];
}

export interface ReportDraft {
  root_cause: string;
  confidence: Confidence;
  suggestions: string[];
}
