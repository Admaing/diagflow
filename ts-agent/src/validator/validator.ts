const EVIDENCE_KEYWORDS = [
  "log",
  "metric",
  "config",
  "error",
  "oom",
  "timeout",
  "memory",
  "disk",
  "cpu",
  "checkpoint",
  "known",
  "issue",
  "bug",
  "deepwiki",
  "日志",
  "指标",
  "配置",
  "错误",
  "异常",
  "内存",
  "磁盘",
  "网络",
  "超时",
  "已知",
];

export interface ValidationResult {
  ok: boolean;
  feedback: string;
}

/**
 * The same deterministic layers the Go validator runs:
 *   L1 format — non-trivial root cause, suggestions present, evidence present
 *   L2 cross-source — the root cause must reference an evidence keyword
 *
 * There is no independent LLM review layer; the caller retries synthesis once
 * with the returned feedback.
 */
export function validateConclusion(
  rootCause: string,
  suggestions: string[],
  evidenceCount: number,
): ValidationResult {
  const cause = rootCause.trim();
  if (cause.length < 10) {
    return { ok: false, feedback: "root cause too short" };
  }
  if (suggestions.length === 0) {
    return { ok: false, feedback: "no suggestions provided" };
  }
  if (evidenceCount === 0) {
    return { ok: false, feedback: "no evidence — may be speculative" };
  }

  const lower = cause.toLowerCase();
  if (!EVIDENCE_KEYWORDS.some((keyword) => lower.includes(keyword))) {
    return {
      ok: false,
      feedback: "root cause doesn't reference evidence — may be hallucinated",
    };
  }
  return { ok: true, feedback: "" };
}
