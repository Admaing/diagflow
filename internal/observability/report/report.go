// Package report mirrors diagflow/observability/report.py — Markdown rendering
// of a diagnosis report.
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/Admaing/diagflow/internal/diagagent"
)

// Render renders a diagnosis report as Markdown.
func Render(r *diagagent.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 诊断报告: %s.%s\n\n", r.Component, r.ProblemType)
	fmt.Fprintf(&b, "- **事件 ID**: %s\n", r.EventID)
	fmt.Fprintf(&b, "- **组件**: %s\n", r.Component)
	fmt.Fprintf(&b, "- **问题类型**: %s\n", r.ProblemType)
	fmt.Fprintf(&b, "- **耗时**: %.0fms\n", r.DurationMS)
	matched := "❌"
	if r.MatchedKnowledge {
		matched = "✅"
	}
	fmt.Fprintf(&b, "- **匹配知识库**: %s\n", matched)
	fmt.Fprintf(&b, "- **时间**: %s\n\n", time.Now().Format(time.RFC3339))
	b.WriteString("---\n\n## 根因分析\n\n")
	fmt.Fprintf(&b, "**根因**: %s\n\n", r.RootCause)
	fmt.Fprintf(&b, "**置信度**: %s\n\n", confidenceBadge(r.Confidence))
	b.WriteString("---\n\n## 修复建议\n\n")
	for i, s := range r.Suggestions {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	b.WriteString("\n---\n\n## 证据链\n\n")
	if len(r.EvidenceSummary) == 0 {
		b.WriteString("_(无证据收集)_\n\n")
	} else {
		for _, ev := range r.EvidenceSummary {
			source, _ := ev["source_agent"].(string)
			summary, _ := ev["summary"].(string)
			fmt.Fprintf(&b, "- **[%s]** %s\n", source, summary)
		}
	}
	b.WriteString("\n")
	return b.String()
}

func confidenceBadge(level string) string {
	switch level {
	case "high":
		return "🟢 高"
	case "medium":
		return "🟡 中"
	case "low":
		return "🔴 低"
	default:
		return level
	}
}
