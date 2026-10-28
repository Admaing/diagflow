import type { InvestigationInput } from "../report/types.js";

/** Shared request → context mapping for both workflows. */
export function buildContext(input: InvestigationInput): Record<string, unknown> {
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
