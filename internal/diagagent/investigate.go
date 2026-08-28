// Investigation model for internal alert investigation (内部告警排查).
//
// DiagFlow's investigation path is intent-driven rather than fingerprint-driven:
// the LLM proposes 2-5 investigation intents, each intent is semantically matched
// against historical intents (via the KB), and non-matching intents are executed
// step-by-step — collecting logs, rendering a judgment, and (only when a component
// bug is suspected) constructing a structured DeepWiki prompt.
package diagagent

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Admaing/diagflow/internal/memory"
)

// InvestigationAction is a single tool invocation within an investigation step.
type InvestigationAction struct {
	Tool   string
	Params map[string]any
	Result string
}

// InvestigationStep records one investigation intent and its outcome.
type InvestigationStep struct {
	Intent              string
	Actions             []InvestigationAction
	LogSnippets         []string
	Judgment            string
	SuspectComponentBug bool
	MatchedHistorical   bool
	HistoricalRootCause string
	DeepwikiPrompt      string
	DeepwikiResult      string
}

// InvestigationTrace holds the full sequence of investigation steps.
type InvestigationTrace struct {
	Component   string
	ProblemType string
	Steps       []InvestigationStep
}

// AddStep appends a step and returns it as a pointer for inline filling.
func (t *InvestigationTrace) AddStep(intent string) *InvestigationStep {
	t.Steps = append(t.Steps, InvestigationStep{Intent: intent})
	return &t.Steps[len(t.Steps)-1]
}

// toEvidencePool flattens the trace into an EvidencePool for synthesis.
func (t *InvestigationTrace) toEvidencePool() *memory.EvidencePool {
	pool := &memory.EvidencePool{}
	for _, s := range t.Steps {
		if s.MatchedHistorical {
			pool.Add(memory.NewEvidence("investigation", "historical", s.Intent, s.HistoricalRootCause, 0.9))
			continue
		}
		for _, snip := range s.LogSnippets {
			pool.Add(memory.NewEvidence("investigation", "log", s.Intent, snip, 0.6))
		}
	}
	return pool
}

// RenderMarkdown renders the trace as an intent-by-intent Markdown report.
func (t *InvestigationTrace) RenderMarkdown() string {
	var b strings.Builder
	b.WriteString("## 排查过程\n\n")
	for i, step := range t.Steps {
		b.WriteString("### 思路 " + strconv.Itoa(i+1) + ": " + step.Intent + "\n\n")

		if step.MatchedHistorical {
			b.WriteString("- **命中历史排查思路** → 根因: " + step.HistoricalRootCause + "\n\n")
			continue
		}

		if len(step.Actions) > 0 {
			b.WriteString("**动作**:\n")
			for _, a := range step.Actions {
				b.WriteString("- `" + a.Tool + "` " + yamlish(a.Params) + "\n")
			}
			b.WriteString("\n")
		}

		if len(step.LogSnippets) > 0 {
			b.WriteString("**日志片段**:\n```\n")
			for _, s := range step.LogSnippets {
				b.WriteString(trunc(s, 500) + "\n")
			}
			b.WriteString("```\n\n")
		}

		if step.Judgment != "" {
			b.WriteString("**判断**: " + step.Judgment + "\n\n")
		}

		if step.DeepwikiPrompt != "" {
			b.WriteString("**DeepWiki 提示词**:\n```\n" + step.DeepwikiPrompt + "\n```\n\n")
			if step.DeepwikiResult != "" {
				b.WriteString("**DeepWiki 结果**: " + step.DeepwikiResult + "\n\n")
			}
		}
	}
	return b.String()
}

// yamlish renders a param map as a compact key=value string.
func yamlish(m map[string]any) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+fmtAny(v))
	}
	return strings.Join(parts, " ")
}

func fmtAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
