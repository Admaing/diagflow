"""Markdown report generation for diagnosis results."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from diagflow.core.diag_agent import DiagnosisReport


def render_report(report: DiagnosisReport) -> str:
    """Render a diagnosis report as formatted Markdown."""
    lines = [
        f"# 🔍 诊断报告: {report.component}.{report.problem_type}",
        "",
        f"- **事件 ID**: {report.event_id}",
        f"- **组件**: {report.component}",
        f"- **问题类型**: {report.problem_type}",
        f"- **耗时**: {report.duration_ms:.0f}ms",
        f"- **匹配知识库**: {'✅' if report.matched_knowledge else '❌'}",
        f"- **时间**: {datetime.now().isoformat()}",
        "",
        "---",
        "",
        "## 📋 根因分析",
        "",
        f"**根因**: {report.root_cause}",
        "",
        f"**置信度**: {_confidence_badge(report.confidence)}",
        "",
        "---",
        "",
        "## 🔧 修复建议",
        "",
    ]

    for i, suggestion in enumerate(report.suggestions, 1):
        lines.append(f"{i}. {suggestion}")

    lines.extend([
        "",
        "---",
        "",
        "## 📊 证据链",
        "",
    ])

    if report.evidence_summary:
        for ev in report.evidence_summary:
            lines.append(f"- **[{ev.get('source_agent', 'unknown')}]** "
                         f"{ev.get('summary', '')} "
                         f"_(置信度: {ev.get('confidence', 0.5):.0%})_")
    else:
        lines.append("_(无证据收集)_")

    lines.extend([
        "",
        "---",
        "",
        "## 🔍 排查过程追溯",
        "",
        f"完整排查日志位于: `/tmp/diagflow/{report.event_id}/`",
        "",
    ])

    return "\n".join(lines)


def _confidence_badge(level: str) -> str:
    badges = {
        "high": "🟢 高",
        "medium": "🟡 中",
        "low": "🔴 低",
    }
    return badges.get(level, level)
