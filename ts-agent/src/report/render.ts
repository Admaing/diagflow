import { isInquiry, type DiagnosisReport, type EvidenceItem } from "./types.js";
import { isRedundantSummary, truncate } from "../util.js";

function confidenceBadge(level: string): string {
  switch (level) {
    case "high":
      return "🟢 高";
    case "medium":
      return "🟡 中";
    case "low":
      return "🔴 低";
    default:
      return level;
  }
}

function renderEvidence(items: EvidenceItem[]): string {
  if (items.length === 0) return "_(无证据收集)_";
  return items
    .map((item) => {
      const source = item.source_agent || "unknown";
      const summary = truncate(item.summary, 240);
      // A summary that just repeats the detail would print the same sentence
      // twice; keep only the expandable full text in that case.
      const body = item.detail && !isRedundantSummary(item.summary, item.detail)
        ? `${summary}\n  <details><summary>详情</summary>\n\n  ${item.detail.replace(/\n/g, "\n  ")}\n\n  </details>`
        : item.detail
          ? item.detail
          : summary;
      return `- **[${source}]** ${body}`;
    })
    .join("\n");
}

export function renderReport(report: DiagnosisReport): string {
  const lines: string[] = [];
  const inquiry = isInquiry(report.problem_type);
  lines.push(
    inquiry
      ? `# 状态报告: ${report.component}`
      : `# 诊断报告: ${report.component}.${report.problem_type}`,
  );
  lines.push("");
  lines.push(`- **事件 ID**: ${report.event_id}`);
  lines.push(`- **组件**: ${report.component}`);
  lines.push(`- **问题类型**: ${inquiry ? "状态查看" : report.problem_type}`);
  lines.push(`- **状态**: ${report.status}`);
  lines.push(`- **耗时**: ${Math.round(report.duration_ms)}ms`);
  lines.push(`- **匹配知识库**: ${report.matched_knowledge ? "✅" : "❌"}`);
  lines.push("");
  if (report.warnings.length > 0) {
    lines.push("> **降级警告**");
    for (const warning of report.warnings) lines.push(`> - ${warning}`);
    lines.push("");
  }
  lines.push("---");
  lines.push("");
  lines.push(inquiry ? "## 当前状态" : "## 根因分析");
  lines.push("");
  lines.push(inquiry ? `**状态**: ${report.root_cause}` : `**根因**: ${report.root_cause}`);
  lines.push("");
  lines.push(`**置信度**: ${confidenceBadge(report.confidence)}`);
  lines.push("");
  lines.push("---");
  lines.push("");
  lines.push(inquiry ? "## 注意事项" : "## 修复建议");
  lines.push("");
  report.suggestions.forEach((suggestion, index) => lines.push(`${index + 1}. ${suggestion}`));
  lines.push("");
  lines.push("---");
  lines.push("");
  lines.push("## 证据链");
  lines.push("");
  lines.push(renderEvidence(report.evidence_summary));
  lines.push("");
  if (report.trace && report.trace.steps.length > 0) {
    lines.push("---");
    lines.push("");
    lines.push("## 排查过程");
    lines.push("");
    report.trace.steps.forEach((step, index) => {
      lines.push(`### ${index + 1}. ${step.intent}`);
      if (step.reason) lines.push(`- 原因: ${step.reason}`);
      lines.push(`- 判断: ${step.judgment}`);
      if (step.suspect_component_bug) lines.push("- 是否怀疑组件 Bug: 是");
      if (step.actions.length > 0) {
        lines.push(`- 工具调用: ${step.actions.map((action) => action.tool).join(", ")}`);
      }
      lines.push("");
    });
  }
  return lines.join("\n").trim() + "\n";
}
