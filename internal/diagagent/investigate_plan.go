// Investigation planning and execution for the intent-driven path.
package diagagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// intentItem is one proposed investigation intent.
type intentItem struct {
	Intent string `json:"intent"`
	Reason string `json:"reason"`
}

// buildInvestigationPlan asks the LLM to propose 2-5 investigation intents for
// the given component + semi-structured fields.
func (a *Agent) buildInvestigationPlan(ctx context.Context, component string, contextData map[string]any) ([]intentItem, error) {
	prompt := fmt.Sprintf(
		"You are a big-data platform SRE. Given a reported problem on component %q, "+
			"propose 2-5 investigation intents (排查思路) — each a self-contained direction "+
			"to verify (e.g. locate the failing job, fetch its container log, check TaskManager "+
			"heap, verify whether it is a known upstream bug).\n\n"+
			"Cluster context:\n%s\n\n"+
			"Call the propose_intents tool with your ordered list.",
		component, jsonString(stripTopology(contextData)),
	)

	intentsTool := &anthropic.ToolParam{
		Name:        "propose_intents",
		Description: anthropic.String("Propose ordered investigation intents."),
		InputSchema: schemaFromMap(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"intents": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"intent": map[string]any{"type": "string"},
							"reason": map[string]any{"type": "string"},
						},
						"required": []any{"intent"},
					},
				},
			},
			"required": []any{"intents"},
		}),
	}

	a.llmCalls++
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       anthropic.Model(a.model),
		MaxTokens:   512,
		Temperature: anthropic.Float(0.2),
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Tools:       []anthropic.ToolUnionParam{{OfTool: intentsTool}},
		ToolChoice:  anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "propose_intents"}},
	})
	if err != nil {
		return nil, err
	}

	for _, b := range resp.Content {
		if b.Type == "tool_use" && b.Name == "propose_intents" {
			var args struct {
				Intents []intentItem `json:"intents"`
			}
			if json.Unmarshal(b.Input, &args) == nil && len(args.Intents) > 0 {
				return args.Intents, nil
			}
		}
	}
	return nil, fmt.Errorf("no intents proposed")
}

// searchSimilarIntent semantically matches an intent against historical intents.
// Returns (historicalRootCause, matched).
func (a *Agent) searchSimilarIntent(intent string) (string, bool) {
	if a.kb == nil || !a.kb.HasRealEmbeddings() {
		return "", false
	}
	results := a.kb.Search(intent, 1)
	if len(results) == 0 {
		return "", false
	}
	r := results[0]
	matched := false
	if r.FusionScore != nil {
		matched = *r.FusionScore > 0.05
	} else {
		matched = r.Distance < 0.3
	}
	if !matched {
		return "", false
	}
	rootCause, _ := r.Metadata["root_cause"].(string)
	if rootCause == "" {
		rootCause = trunc(r.Document, 200)
	}
	return rootCause, true
}

// buildDeepwikiPrompt constructs a structured DeepWiki prompt when a component
// bug is suspected. Returns plain text suitable for display and forwarding.
func (a *Agent) buildDeepwikiPrompt(component, version string, step *InvestigationStep) string {
	repo := a.repoFor(component)
	var errorClass string
	for _, s := range step.LogSnippets {
		if kw := a.firstErrorKeyword(s); kw != "" {
			errorClass = kw
			break
		}
	}
	var b strings.Builder
	b.WriteString("repo: " + repo + "\n")
	b.WriteString("question: 在 " + component + " " + version + " 上，" +
		errClassOr(errorClass, "出现异常") + "，这是已知 bug 还是配置/操作问题？\n")
	b.WriteString("log snippets:\n")
	for _, s := range step.LogSnippets {
		b.WriteString("  - " + trunc(s, 200) + "\n")
	}
	return b.String()
}

func (a *Agent) repoFor(component string) string {
	if a.repositoryMap == nil {
		return "unknown"
	}
	if repo, ok := a.repositoryMap[strings.ToLower(component)]; ok {
		return repo
	}
	return "unknown"
}

func (a *Agent) firstErrorKeyword(s string) string {
	lower := strings.ToLower(s)
	for _, kw := range a.errorKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return ""
}

func errClassOr(class, def string) string {
	if class == "" {
		return def
	}
	return "出现 " + class
}

// dispatchDeepwiki invokes the registered deepwiki_query tool and returns its
// result text (empty if the tool is unavailable or fails). The prompt stored on
// the step is still shown to the user regardless.
func (a *Agent) dispatchDeepwiki(ctx context.Context, component string, step *InvestigationStep) string {
	tool, ok := a.tools["deepwiki_query"]
	if !ok {
		return ""
	}
	var errorClass string
	for _, s := range step.LogSnippets {
		if kw := a.firstErrorKeyword(s); kw != "" {
			errorClass = kw
			break
		}
	}
	question := component + " 已知 bug: " + errClassOr(errorClass, "异常")
	result, err := tool.Handler(ctx, map[string]any{
		"component": component,
		"question":  question,
		"version":   "",
	})
	if err != nil {
		return ""
	}
	if result.Success {
		return result.Data
	}
	return result.Error
}

// runIntent executes a single investigation intent: it drives the LLM to choose
// tools, collects their outputs as log snippets, then asks for a judgment
// (whether the intent points at a root cause and whether a component bug is
// suspected). Returns (judgment, suspectComponentBug).
func (a *Agent) runIntent(ctx context.Context, step *InvestigationStep, contextData map[string]any) (string, bool) {
	system := "You are a senior SRE executing a single investigation intent. " +
		"Use tools to gather the log/metric evidence for this intent. Keep it focused " +
		"— call at most 4 tools, then stop."

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(
			"Investigation intent: " + step.Intent + "\n\nCluster context:\n" +
				jsonString(stripTopology(contextData)),
		)),
	}

	toolParam := func() []anthropic.ToolUnionParam {
		if len(a.tools) > 0 {
			return a.schemas()
		}
		return nil
	}

	for turn := 0; turn < 4; turn++ {
		a.llmCalls++
		params := anthropic.MessageNewParams{
			Model:       anthropic.Model(a.model),
			MaxTokens:   1024,
			Temperature: anthropic.Float(0.2),
			System:      []anthropic.TextBlockParam{{Text: system}},
			Messages:    messages,
		}
		if tp := toolParam(); len(tp) > 0 {
			params.Tools = tp
		}

		resp, err := a.client.Messages.New(ctx, params)
		if err != nil {
			return "LLM error during investigation", false
		}

		var toolUses []anthropic.ContentBlockUnion
		var textParts []string
		for _, b := range resp.Content {
			switch b.Type {
			case "tool_use":
				toolUses = append(toolUses, b)
			case "text":
				textParts = append(textParts, b.Text)
			}
		}

		if len(toolUses) == 0 {
			judgment := strings.Join(textParts, "\n")
			return judgment, suspectBug(judgment)
		}

		// Record assistant turn.
		assistantBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(resp.Content))
		for _, b := range resp.Content {
			assistantBlocks = append(assistantBlocks, blockToParam(b))
		}
		messages = append(messages, anthropic.NewAssistantMessage(assistantBlocks...))

		var results []anthropic.ContentBlockParamUnion
		for _, tu := range toolUses {
			block := toolUseAsBlock(tu)
			tool, ok := a.tools[block.Name]
			if !ok {
				results = append(results, anthropic.NewToolResultBlock(block.ID, "unknown tool", false))
				continue
			}
			a.emit(fmt.Sprintf("[intent] %s(%s)", block.Name, trunc(string(block.Input), 200)))
			result, _ := tool.Handler(ctx, jsonInputToMap(block.Input))
			resultText := result.Data
			if !result.Success {
				resultText = result.Error
			}
			step.Actions = append(step.Actions, InvestigationAction{
				Tool: block.Name, Params: jsonInputToMap(block.Input), Result: trunc(resultText, 500),
			})
			if strings.TrimSpace(resultText) != "" {
				step.LogSnippets = append(step.LogSnippets, resultText)
			}
			results = append(results, anthropic.NewToolResultBlock(block.ID, resultText, !result.Success))
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}

	// Final: ask for a judgment if loop exhausted.
	a.llmCalls++
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       anthropic.Model(a.model),
		MaxTokens:   512,
		Temperature: anthropic.Float(0.1),
		System:      []anthropic.TextBlockParam{{Text: "Summarize your judgment for this intent in one sentence."}},
		Messages:    messages,
	})
	if err != nil {
		return "unable to conclude", false
	}
	judgment := firstText(resp)
	return judgment, suspectBug(judgment)
}

// suspectBug heuristically flags whether a judgment suggests a component bug.
func suspectBug(judgment string) bool {
	low := strings.ToLower(judgment)
	for _, kw := range []string{"known bug", "upstream bug", "组件 bug", "开源 bug", "apache/flink"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// Investigate runs the intent-driven internal alert investigation path.
//
// Flow:
//  1. LLM proposes 2-5 investigation intents.
//  2. Each intent is semantically matched against historical intents; matches
//     short-circuit with the historical root cause.
//  3. Non-matching intents are executed step-by-step (tool calls → log snippets
//     → judgment); if a component bug is suspected, a structured DeepWiki prompt
//     is built and recorded.
//  4. A final synthesis produces the root cause + suggestions.
func (a *Agent) Investigate(ctx context.Context, component, problemType string, contextData map[string]any) (*Report, error) {
	a.eventID = fmt.Sprintf("diag-%d-%s", time.Now().Unix(), shortID())
	a.llmCalls = 0
	start := time.Now()

	a.emit(fmt.Sprintf("[%s] internal investigation: %s/%s", a.eventID, component, problemType))

	trace := &InvestigationTrace{Component: component, ProblemType: problemType}

	intents, err := a.buildInvestigationPlan(ctx, component, contextData)
	if err != nil {
		// Fall back to a single default intent if the LLM fails to propose.
		intents = []intentItem{{Intent: "locate the failing job and collect its error logs"}}
		a.emit(fmt.Sprintf("[%s] intent proposal failed, using default — %v", a.eventID, err))
	}

	var matchedRootCauses []string
	for _, it := range intents {
		// 2. Semantic match against historical intents.
		if rootCause, ok := a.searchSimilarIntent(it.Intent); ok {
			step := trace.AddStep(it.Intent)
			step.MatchedHistorical = true
			step.HistoricalRootCause = rootCause
			matchedRootCauses = append(matchedRootCauses, rootCause)
			a.emit(fmt.Sprintf("[%s] intent matched: %s → %s", a.eventID, it.Intent, trunc(rootCause, 80)))
			continue
		}

		// 3. Execute the intent.
		a.emit(fmt.Sprintf("[%s] executing intent: %s", a.eventID, it.Intent))
		step := trace.AddStep(it.Intent)
		judgment, suspect := a.runIntent(ctx, step, contextData)
		step.Judgment = judgment
		step.SuspectComponentBug = suspect
		if suspect {
			step.DeepwikiPrompt = a.buildDeepwikiPrompt(component, strOr(contextData["version"], ""), step)
			if result := a.dispatchDeepwiki(ctx, component, step); result != "" {
				step.DeepwikiResult = result
			}
			a.emit(fmt.Sprintf("[%s] component-bug suspected → DeepWiki consulted", a.eventID))
		}
	}

	// 4. Final synthesis.
	rootCause, suggestions, confidence := a.synthesizeFromTrace(ctx, trace, contextData, component, problemType)

	durationMS := float64(time.Since(start).Milliseconds())
	a.emit(fmt.Sprintf("[%s] investigation done in %.0fms", a.eventID, durationMS))

	return &Report{
		EventID:          a.eventID,
		Component:        component,
		ProblemType:      problemType,
		RootCause:        rootCause,
		Confidence:       confidence,
		EvidenceSummary:  traceEvidence(trace),
		Suggestions:      suggestions,
		MatchedKnowledge: len(matchedRootCauses) > 0,
		DurationMS:       durationMS,
		PhasesRun:        []string{"intent_plan", "intent_execute", "synthesize"},
		Trace:            trace,
	}, nil
}

// synthesizeFromTrace builds the final conclusion from an investigation trace,
// reusing the existing synthesize() path by constructing an evidence pool.
func (a *Agent) synthesizeFromTrace(ctx context.Context, trace *InvestigationTrace, contextData map[string]any, component, problemType string) (string, []string, string) {
	// If any intent matched a historical root cause, that already answers the
	// problem — short-circuit without a further LLM synthesis (matches the
	// "命中历史即复用结论" intent, and avoids a divergent hallucinated root cause).
	var historical []string
	for _, s := range trace.Steps {
		if s.MatchedHistorical && s.HistoricalRootCause != "" {
			historical = append(historical, s.HistoricalRootCause)
		}
	}
	if len(historical) > 0 {
		return historical[0], []string{"复用历史排查结论"}, "high"
	}

	pool := trace.toEvidencePool()
	if pool.Len() == 0 {
		return "未能收集到足够证据", []string{"扩大日志范围后重试"}, "low"
	}
	return a.synthesize(ctx, pool, contextData, component, problemType, "")
}

// traceEvidence flattens a trace into the report's evidence-summary list.
func traceEvidence(trace *InvestigationTrace) []map[string]any {
	var out []map[string]any
	for _, s := range trace.Steps {
		if s.MatchedHistorical {
			out = append(out, map[string]any{
				"source_agent": "investigation",
				"summary":      "命中历史排查思路: " + s.Intent,
				"detail":       s.HistoricalRootCause,
			})
			continue
		}
		for _, snip := range s.LogSnippets {
			out = append(out, map[string]any{
				"source_agent": "investigation",
				"summary":      s.Intent,
				"detail":       trunc(snip, 300),
			})
		}
	}
	return out
}

// ConfirmCorrect writes the executed investigation intents (and final root
// cause) back into the KB as historical intents — only after the user confirms
// this diagnosis was correct. Matched-historical steps are already known, so
// they are skipped.
func (a *Agent) ConfirmCorrect(report *Report) {
	if a.kb == nil || report.Trace == nil {
		return
	}
	version := ""
	for _, s := range report.Trace.Steps {
		if s.MatchedHistorical {
			continue
		}
		if s.Intent == "" {
			continue
		}
		a.kb.AddInvestigationIntent(s.Intent, report.RootCause, report.Component, version)
	}
}
