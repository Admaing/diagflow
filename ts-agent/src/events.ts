import type { DiagnosisReport } from "./report/types.js";

export type StepKind =
  | "phase"
  | "tool_call"
  | "tool_result"
  | "tool_error"
  | "warn"
  | "log";

export interface StepEvent {
  type: "step";
  kind: StepKind;
  text: string;
  tool?: string;
  elapsed_ms: number;
}

export interface ReportEvent {
  type: "report";
  report: DiagnosisReport;
  markdown: string;
}

export interface FailedEvent {
  type: "failed";
  code: string;
  message: string;
}

export interface DoneEvent {
  type: "done";
  duration_ms: number;
}

export type AgentEvent = StepEvent | ReportEvent | FailedEvent | DoneEvent;

export type EventSink = (event: AgentEvent) => void;

export class EventEmitter {
  private readonly startedAt = Date.now();

  constructor(private readonly sink: EventSink) {}

  step(kind: StepKind, text: string, tool?: string): void {
    this.sink({
      type: "step",
      kind,
      text,
      ...(tool ? { tool } : {}),
      elapsed_ms: Date.now() - this.startedAt,
    });
  }

  report(report: DiagnosisReport, markdown: string): void {
    this.sink({ type: "report", report, markdown });
  }

  failed(code: string, message: string): void {
    this.sink({ type: "failed", code, message });
  }

  done(): void {
    this.sink({ type: "done", duration_ms: Date.now() - this.startedAt });
  }
}

export function noopSink(): EventSink {
  return () => undefined;
}
