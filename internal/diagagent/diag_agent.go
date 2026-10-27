// Package diagagent is the single-agent diagnostic engine. Diagnose runs the
// five-phase pipeline; Investigate runs the intent-driven path. Both use the
// Anthropic SDK's tool_use blocks under explicit turn and tool-call budgets.
package diagagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/memory"
	"github.com/Admaing/diagflow/internal/metrics"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
	"github.com/Admaing/diagflow/internal/strategy"
	"github.com/Admaing/diagflow/internal/tools"
	"github.com/Admaing/diagflow/internal/validator"
)

// KnowledgeBase is the minimal KB surface DiagAgent needs.
type KnowledgeBase interface {
	Search(query string, n int) []vectorstore.SearchResult
	FingerprintMatch(component, errorPattern, version string) map[string]any
	AddCase(component, errorPattern, version, rootCause string, suggestions []string) string
	AddInvestigationIntent(intentDesc, rootCause, component, version string)
	HasRealEmbeddings() bool
}

// Report is the structured diagnosis result (mirrors DiagnosisReport).
type Report struct {
	// Status is "ok" when a reliable conclusion was produced and "degraded"
	// when part of the pipeline failed but the conclusion still rests on
	// valid evidence. Failures that leave no trustworthy conclusion are
	// returned as errors instead of a Report.
	Status           string
	Error            string
	Warnings         []string
	EventID          string
	Component        string
	ProblemType      string
	RootCause        string
	Confidence       string
	EvidenceSummary  []map[string]any
	Suggestions      []string
	MatchedKnowledge bool
	DurationMS       float64
	LLMCalls         int
	LLMTokens        int64
	PhasesRun        []string
	Trace            *InvestigationTrace
}

const (
	// ReportStatusOK means every dependency needed for the conclusion worked.
	ReportStatusOK = "ok"
	// ReportStatusDegraded means a non-critical phase failed, but the final
	// conclusion was still produced from valid evidence.
	ReportStatusDegraded = "degraded"
)

var (
	// ErrLLMUnavailable identifies a model/gateway failure. Callers must not
	// turn this into a diagnosis; the HTTP layer maps it to 502.
	ErrLLMUnavailable = errors.New("llm dependency unavailable")
	// ErrNoConclusion means the model responded, but its output could not be
	// validated into a trustworthy diagnosis.
	ErrNoConclusion = errors.New("no reliable diagnosis conclusion")
)

// EventFunc is the streaming trace callback (mirrors on_event).
type EventFunc func(msg string)

// toolTimeout bounds each individual tool invocation (strategy + ReAct).
const toolTimeout = 30 * time.Second

var (
	llmCallsTotal  = metrics.NewCounter("diagflow_llm_calls_total", "LLM calls (ok)")
	llmErrors      = metrics.NewCounter("diagflow_llm_errors_total", "LLM call errors")
	llmTokensTotal = metrics.NewCounter("diagflow_llm_tokens_total", "LLM tokens consumed (input+output)")
	toolCallsTotal = metrics.NewCounter("diagflow_tool_calls_total", "Tool calls by tool/status", "tool", "status")
)

func statusLabel(success bool) string {
	if success {
		return "ok"
	}
	return "error"
}

// Agent is the single diagnostic agent.
type Agent struct {
	client         anthropic.Client
	explicitClient bool
	model          string
	maxTokens      int64
	// language is the BCP-47-ish code (e.g. "zh-CN") every LLM-generated,
	// user-facing field is written in. See languageDirective.
	language      string
	strategiesDir string
	kb            KnowledgeBase
	validator     *validator.Validator
	onEvent       EventFunc

	// callLLMHook, when set, replaces the real LLM call. It exists so the
	// ReAct loop (turn budget, stall detection, finalization) can be tested
	// without a live model.
	callLLMHook func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error)

	errorKeywords []string
	repositoryMap map[string]string

	tools     map[string]tools.Def
	eventID   string
	llmCalls  int
	llmTokens int64
	stepOrder int
}

// New builds an Agent.
func New(opts ...Option) *Agent {
	a := &Agent{
		// Single source of truth: the default comes from config, so the LLM
		// model is changed in one place (DIAGFLOW_LLM__MODEL).
		model:     config.Get().LLM.Model,
		maxTokens: 4096,
		validator: validator.New(),
		tools:     make(map[string]tools.Def),
	}
	cfg := config.Get()
	a.model = cfg.LLM.Model
	a.maxTokens = int64(cfg.LLM.MaxTokens)
	a.language = cfg.LLM.Language
	a.strategiesDir = cfg.StrategiesDir
	a.errorKeywords = cfg.RAG.ErrorKeywords
	a.repositoryMap = cfg.Components.RepoMap

	for _, o := range opts {
		o(a)
	}

	apiKey := cfg.LLM.APIKey
	baseURL := cfg.LLM.BaseURL
	if !a.explicitClient {
		opts := []option.RequestOption{
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
			option.WithMaxRetries(3),
		}
		a.client = anthropic.NewClient(opts...)
	}
	return a
}

// Option configures an Agent.
type Option func(*Agent)

// WithClient overrides the Anthropic client.
func WithClient(c anthropic.Client) Option {
	return func(a *Agent) { a.client = c; a.explicitClient = true }
}

// WithModel overrides the model.
func WithModel(m string) Option { return func(a *Agent) { a.model = m } }

// WithKB injects a knowledge base.
func WithKB(kb KnowledgeBase) Option { return func(a *Agent) { a.kb = kb } }

// WithStrategiesDir overrides the strategies directory.
func WithStrategiesDir(d string) Option { return func(a *Agent) { a.strategiesDir = d } }

// WithEventFunc sets the streaming callback.
func WithEventFunc(f EventFunc) Option { return func(a *Agent) { a.onEvent = f } }

// RegisterTools registers a set of tools.
func (a *Agent) RegisterTools(defs []tools.Def) {
	for _, d := range defs {
		a.tools[d.Name] = d
	}
}

// Diagnose runs the five-phase pipeline.
func (a *Agent) Diagnose(ctx context.Context, component, problemType string, contextData map[string]any) (*Report, error) {
	a.eventID = fmt.Sprintf("diag-%d-%s", time.Now().Unix(), shortID())
	a.llmCalls = 0
	a.stepOrder = 0
	start := time.Now()
	warnings := []string{}

	a.emit(fmt.Sprintf("[%s] starting %s/%s", a.eventID, component, problemType))

	strat := strategy.Load(component, problemType, a.strategiesDir)
	a.emit(fmt.Sprintf("[%s] strategy: %d steps", a.eventID, len(strat.Steps)))

	pool := &memory.EvidencePool{}
	phases := []string{}

	// ---- Phase 1: KB fast-path ----
	// Fingerprints do not need an embedding model, so an exact repeat of a
	// known error can short-circuit even when semantic search is unavailable.
	if hit := a.fingerprintHit(component, contextData); hit != nil {
		rootCause := strOr(hit["root_cause"], "")
		suggestions := toStrSlice(hit["suggestions"])
		if rootCause != "" && len(suggestions) > 0 {
			a.emit(fmt.Sprintf("[%s] KB fingerprint hit — skipping LLM", a.eventID))
			return &Report{
				Status:           ReportStatusOK,
				EventID:          a.eventID,
				Component:        component,
				ProblemType:      problemType,
				RootCause:        rootCause,
				Confidence:       "high",
				EvidenceSummary:  []map[string]any{{"source_agent": "knowledge_base", "category": "fingerprint_match", "summary": "命中历史指纹", "detail": rootCause, "confidence": 1.0}},
				Suggestions:      suggestions,
				MatchedKnowledge: true,
				DurationMS:       float64(time.Since(start).Milliseconds()),
				PhasesRun:        []string{"kb_fast_path"},
			}, nil
		}
		a.emit(fmt.Sprintf("[%s] fingerprint hit ignored: incomplete case data", a.eventID))
	}
	if a.kb != nil && a.kb.HasRealEmbeddings() {
		if hit := a.kbMatch(contextData); hit != nil {
			a.emit(fmt.Sprintf("[%s] KB hit — skipping LLM", a.eventID))
			rootCause := strOr(hit["root_cause"], "")
			suggestions := toStrSlice(hit["suggestions"])
			if rootCause == "" || len(suggestions) == 0 {
				a.emit(fmt.Sprintf("[%s] KB hit ignored: incomplete root cause or suggestions", a.eventID))
			} else {
				report := &Report{
					Status:           ReportStatusOK,
					EventID:          a.eventID,
					Component:        component,
					ProblemType:      problemType,
					RootCause:        rootCause,
					Confidence:       "high",
					EvidenceSummary:  []map[string]any{},
					Suggestions:      suggestions,
					MatchedKnowledge: true,
					DurationMS:       float64(time.Since(start).Milliseconds()),
					PhasesRun:        []string{"kb_fast_path"},
				}
				return report, nil
			}
		}
	}

	// ---- Phase 2: strategy deterministic execution ----
	a.emit(fmt.Sprintf("[%s] Phase 2: strategy execution", a.eventID))
	phases = append(phases, "strategy")
	a.runStrategy(ctx, strat, contextData, pool)
	a.emit(fmt.Sprintf("[%s] evidence: %d items", a.eventID, pool.Len()))

	// Phase 2.5: KB evidence match (removed — see note in RED-LINES; we keep the
	// semantic/fingerprint fast-path in Phase 1 only).

	// ---- Phase 3: SDK ReAct ----
	a.emit(fmt.Sprintf("[%s] Phase 3: SDK ReAct", a.eventID))
	phases = append(phases, "react")
	taskPrompt := strat.BuildTaskPrompt(stringMap(contextData))
	agentOutput, reactErr := a.runReact(ctx, taskPrompt, pool, contextData, 12)
	if reactErr != nil {
		warning := "ReAct 阶段失败：" + reactErr.Error()
		warnings = append(warnings, warning)
		a.emit(fmt.Sprintf("[%s] %s", a.eventID, warning))
	} else if meaningfulConclusion(agentOutput) {
		pool.Add(memory.NewEvidence("react", "agent_analysis", trunc(agentOutput, 300), agentOutput, 0.7))
	}

	// ---- Phase 4: synthesize + validate ----
	a.emit(fmt.Sprintf("[%s] Phase 4: synthesis + validation", a.eventID))
	phases = append(phases, "validate")
	rootCause, suggestions, confidence, synthErr := a.synthesize(ctx, pool, contextData, component, problemType, "")
	if ok, feedback := a.validator.ValidateLayer(rootCause, suggestions, pool.Len()); !ok {
		a.emit(fmt.Sprintf("[%s] validation failed (%s) — retrying", a.eventID, feedback))
		rootCause, suggestions, confidence, synthErr = a.synthesize(ctx, pool, contextData, component, problemType, feedback)
		if ok, feedback = a.validator.ValidateLayer(rootCause, suggestions, pool.Len()); !ok {
			if synthErr == nil {
				synthErr = fmt.Errorf("%w: conclusion failed validation: %s", ErrNoConclusion, feedback)
			}
		}
	}
	if synthErr != nil {
		return nil, synthErr
	}
	if !meaningfulConclusion(rootCause) {
		return nil, fmt.Errorf("%w: synthesis returned an empty root cause", ErrNoConclusion)
	}
	status := ReportStatusOK
	if len(warnings) > 0 {
		status = ReportStatusDegraded
	}

	durationMS := float64(time.Since(start).Milliseconds())
	a.emit(fmt.Sprintf("[%s] done in %.0fms", a.eventID, durationMS))

	// ---- Phase 5: auto-index (high-confidence only) ----
	if a.kb != nil && confidence == "high" && status == ReportStatusOK {
		phases = append(phases, "kb_index")
		a.kbIndex(rootCause, suggestions, pool, contextData)
	}

	report := &Report{
		Status:           status,
		Warnings:         warnings,
		EventID:          a.eventID,
		Component:        component,
		ProblemType:      problemType,
		RootCause:        rootCause,
		Confidence:       confidence,
		EvidenceSummary:  mapEvidence(pool.All()),
		Suggestions:      suggestions,
		MatchedKnowledge: false,
		DurationMS:       durationMS,
		LLMCalls:         a.llmCalls,
		LLMTokens:        a.llmTokens,
		PhasesRun:        phases,
	}
	return report, nil
}

// fingerprintHit checks the deterministic KB fast-path. It deliberately does
// not require semantic embeddings: an exact component/error/version match is
// useful even when the embedding provider is disabled.
func (a *Agent) fingerprintHit(component string, contextData map[string]any) map[string]any {
	if a.kb == nil {
		return nil
	}
	pattern := errorPatternFromContext(contextData, a.errorKeywords)
	if pattern == "" {
		return nil
	}
	return a.kb.FingerprintMatch(component, pattern, strOr(contextData["version"], ""))
}

func errorPatternFromContext(contextData map[string]any, keywords []string) string {
	for _, key := range []string{"error_pattern", "error", "exception"} {
		if s := strings.TrimSpace(strOr(contextData[key], "")); s != "" {
			return s
		}
	}
	var text strings.Builder
	for _, key := range []string{"problem", "problem_desc", "detail", "message"} {
		text.WriteString(" ")
		text.WriteString(strOr(contextData[key], ""))
	}
	haystack := text.String()
	for _, kw := range keywords {
		if strings.Contains(haystack, kw) {
			return kw
		}
	}
	return ""
}

func meaningfulConclusion(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && s != "no conclusion" && s != "证据不足，无法给出诊断结论。" && s != "无法得出结论"
}

// kbMatch mirrors _kb_match (Phase 1 semantic search).
func (a *Agent) kbMatch(contextData map[string]any) map[string]any {
	query := joinNonEmpty(
		strOr(contextData["component"], ""),
		strOr(contextData["problem"], ""),
		strOr(contextData["detail"], ""),
		strOr(contextData["problem_desc"], ""),
	)
	if strings.TrimSpace(query) == "" {
		return nil
	}
	results := a.kb.Search(query, 3)
	for _, r := range results {
		matched := false
		if r.FusionScore != nil {
			matched = *r.FusionScore > 0.01
		} else {
			matched = r.Distance < 0.3
		}
		if matched {
			rootCause, _ := r.Metadata["root_cause"].(string)
			if rootCause == "" {
				rootCause = trunc(r.Document, 200)
			}
			return map[string]any{
				"root_cause":  rootCause,
				"suggestions": metadataSuggestions(r.Metadata["suggestions"]),
			}
		}
	}
	return nil
}

func metadataSuggestions(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return strings.Split(t, ",")
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *Agent) kbIndex(rootCause string, suggestions []string, pool *memory.EvidencePool, contextData map[string]any) {
	component := strOr(contextData["component"], "unknown")
	version := strOr(contextData["version"], "")
	errorPattern := errorPatternFromContext(contextData, a.errorKeywords)
	if errorPattern == "" {
		for _, ev := range pool.All() {
			if kw := a.firstErrorKeyword(ev.Detail); kw != "" {
				errorPattern = kw
				break
			}
		}
	}
	if errorPattern == "" {
		errorPattern = trunc(rootCause, 80)
	}
	a.kb.AddCase(component, errorPattern, version, rootCause, suggestions)
}

// runReact mirrors _run_react (SDK ReAct loop with maxTurns + 3-turn stall).
func (a *Agent) runReact(ctx context.Context, task string, evidence *memory.EvidencePool, contextData map[string]any, maxTurns int) (string, error) {
	system := a.buildSystemPrompt(evidence, contextData)

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(task)),
	}

	// Convergence state. maxTurns bounds LLM turns, but a single turn can
	// carry a batch of parallel calls, so track calls and repeats too.
	dryTurns := 0
	executed := make(map[string]bool)
	seenResults := make(map[string]bool)
	toolCalls := 0
	for turns := 0; turns < maxTurns; turns++ {
		params := anthropic.MessageNewParams{
			Model:       anthropic.Model(a.model),
			MaxTokens:   a.maxTokens,
			Temperature: anthropic.Float(0.3),
			System:      []anthropic.TextBlockParam{{Text: system}},
			Messages:    messages,
		}
		if len(a.tools) > 0 {
			params.Tools = a.schemas()
		}

		resp, err := a.callLLM(ctx, params)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrLLMUnavailable, err)
		}

		var toolUses []anthropic.ContentBlockUnion
		var textParts []string
		for _, block := range resp.Content {
			switch block.Type {
			case "tool_use":
				toolUses = append(toolUses, block)
			case "text":
				textParts = append(textParts, block.Text)
			}
		}

		if len(toolUses) == 0 {
			if len(textParts) > 0 {
				return strings.Join(textParts, "\n"), nil
			}
			return "", nil
		}

		// Append assistant turn with tool_use blocks.
		assistantBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(resp.Content))
		for _, b := range resp.Content {
			assistantBlocks = append(assistantBlocks, blockToParam(b))
		}
		messages = append(messages, anthropic.NewAssistantMessage(assistantBlocks...))

		var toolResults []anthropic.ContentBlockParamUnion
		novel := 0
		for _, tu := range toolUses {
			block := toolUseAsBlock(tu)
			tool, ok := a.tools[block.Name]
			if !ok {
				toolResults = append(toolResults,
					anthropic.NewToolResultBlock(block.ID, "Unknown tool: "+block.Name, false))
				continue
			}
			// An identical call cannot return anything new: these are
			// read-only probes against a run that has not changed. Answer it
			// from the transcript instead of spending another round trip and
			// another copy of the payload.
			key := callFingerprint(block.Name, block.Input)
			if executed[key] {
				a.emit(fmt.Sprintf("[react] %s skipped: identical call already executed", block.Name))
				toolResults = append(toolResults,
					anthropic.NewToolResultBlock(block.ID, duplicateCallNotice, false))
				continue
			}
			executed[key] = true
			toolCalls++

			a.emit(fmt.Sprintf("[react] %s(%s)", block.Name, trunc(string(block.Input), 200)))
			toolCtx, cancel := context.WithTimeout(ctx, toolTimeout)
			result, toolErr := tool.Handler(toolCtx, jsonInputToMap(block.Input))
			cancel()
			if toolErr != nil {
				result = tools.ToolResult{Data: "", Success: false, Error: toolErr.Error()}
			}
			resultText := result.Data
			if !result.Success {
				resultText = result.Error
				a.emit(fmt.Sprintf("[react] %s error: %s", block.Name, resultText))
			} else {
				a.emit(fmt.Sprintf("[react] %s result: %s", block.Name, trunc(resultText, 200)))
			}
			resultText = capToolResult(resultText, maxReActResultChars)
			toolResults = append(toolResults,
				anthropic.NewToolResultBlock(block.ID, resultText, !result.Success))
			if result.Success && strings.TrimSpace(resultText) != "" {
				if fp := contentFingerprint(resultText); !seenResults[fp] {
					seenResults[fp] = true
					novel++
				}
			}
		}

		if len(toolResults) > 0 {
			messages = append(messages, anthropic.NewUserMessage(toolResults...))
		}

		// Novelty is judged on the content, not on "the call returned
		// something". Re-reading the same log returns the same bytes, and
		// counting that as progress is why the stall rule never fired.
		if novel > 0 {
			dryTurns = 0
		} else {
			dryTurns++
		}

		if toolCalls >= maxReActToolCalls {
			return a.finalize(ctx, system, messages,
				fmt.Sprintf("Tool-call budget (%d calls) exhausted. STOP calling tools and produce "+
					"your diagnosis NOW using only the evidence already gathered. State the root "+
					"cause, your confidence, and what you could not verify.", maxReActToolCalls))
		}

		if dryTurns >= 3 {
			return a.finalize(ctx, system, messages,
				"You've explored for 3 turns without finding new evidence. "+
					"STOP now and produce a diagnosis with what you HAVE.")
		}
	}

	// Budget exhausted. Returning a bare "max turns" string throws away every
	// tool result gathered so far, so force a conclusion instead — same path
	// as the stall case, just a different nudge.
	return a.finalize(ctx, system, messages,
		"Tool-call budget exhausted. STOP calling tools and produce your diagnosis NOW, "+
			"using only the evidence already gathered. State the root cause, your confidence, "+
			"and say explicitly what you could not verify.")
}

// finalize makes one last tool-free LLM call so the run always ends in a
// diagnosis rather than an empty report.
func (a *Agent) finalize(ctx context.Context, system string, messages []anthropic.MessageParam, nudge string) (string, error) {
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(nudge)))
	final, err := a.callLLM(ctx, anthropic.MessageNewParams{
		Model:       anthropic.Model(a.model),
		MaxTokens:   a.maxTokens,
		Temperature: anthropic.Float(0.3),
		System:      []anthropic.TextBlockParam{{Text: system}},
		Messages:    messages,
	})
	if err != nil {
		a.emit(fmt.Sprintf("[agent] finalize LLM call failed: %v", err))
		return "", fmt.Errorf("%w: %w", ErrLLMUnavailable, err)
	}
	return firstText(final), nil
}

// synthesize mirrors _synthesize (structured output via tool_use).
func (a *Agent) synthesize(ctx context.Context, pool *memory.EvidencePool, contextData map[string]any, component, problemType, feedback string) (string, []string, string, error) {
	prompt := fmt.Sprintf(
		"You are a diagnostic synthesiser. Review the evidence and produce a structured diagnosis.\n\n"+
			"Component: %s — Problem: %s\n\n"+
			"=== Evidence Collected ===\n%s\n\n"+
			"=== Context ===\n%s",
		component, problemType, pool.Summary(), jsonString(stripTopology(contextData)),
	)
	prompt += languageDirective(a.language)
	if feedback != "" {
		prompt += "\n\n=== Previous validation feedback ===\n" + feedback
	}
	prompt += "\n\nCall the report_diagnosis tool with your structured conclusion."

	// deepseek-v4.1-flash runs in thinking mode, which rejects a forced
	// tool_choice ({"type":"tool"}) with a 400. "any" is accepted and still
	// guarantees a tool call, so the structured output stays deterministic.
	resp, err := a.callLLM(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model(a.model),
		// Thinking-mode models bill their reasoning against max_tokens. A small
		// budget gets spent entirely on thinking, so the API returns a
		// tool_use block with an EMPTY input object and stop_reason=max_tokens.
		// That silently produced a report with no root cause, so give the
		// synthesiser the same headroom as the ReAct loop.
		MaxTokens:   a.maxTokens,
		Temperature: anthropic.Float(0.2),
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Tools:       []anthropic.ToolUnionParam{{OfTool: a.reportDiagnosisTool()}},
		ToolChoice:  anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}},
	})
	if err != nil {
		a.emit(fmt.Sprintf("[agent] synthesis LLM call failed: %v", err))
		return "", nil, "", fmt.Errorf("%w: %w", ErrLLMUnavailable, err)
	}
	for _, b := range resp.Content {
		if b.Type == "tool_use" && b.Name == "report_diagnosis" {
			var args struct {
				RootCause   string   `json:"root_cause"`
				Confidence  string   `json:"confidence"`
				Suggestions []string `json:"suggestions"`
			}
			if json.Unmarshal(b.Input, &args) == nil && meaningfulConclusion(args.RootCause) && len(args.Suggestions) > 0 {
				return args.RootCause, args.Suggestions, normalizeConfidence(args.Confidence), nil
			}
			// An empty input means the model ran out of output budget
			// mid-thought; say so instead of looking like "no findings".
			a.emit(fmt.Sprintf("[agent] synthesis returned an unusable report_diagnosis input (%d bytes, stop_reason=%s)",
				len(b.Input), resp.StopReason))
			return "", nil, "", fmt.Errorf("%w: report_diagnosis input was empty or incomplete", ErrNoConclusion)
		}
	}

	// Fallback: only accept an explicit, parseable text conclusion. Arbitrary
	// prose (an error message, a refusal, a truncated answer) is not a root
	// cause and must never be rendered as one.
	text := firstText(resp)
	rootCause, suggestions, confidence := parseTextFallback(text)
	if !meaningfulConclusion(rootCause) {
		return "", nil, "", fmt.Errorf("%w: synthesis returned no structured conclusion", ErrNoConclusion)
	}
	return rootCause, suggestions, confidence, nil
}

func normalizeConfidence(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(confidence))
	default:
		return "low"
	}
}

func parseTextFallback(text string) (string, []string, string) {
	rootCause, confidence, suggestions := "", "", []string{}
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		up := strings.ToUpper(s)
		if strings.HasPrefix(up, "ROOT_CAUSE:") {
			rootCause = s[len("ROOT_CAUSE:"):]
			rootCause = strings.TrimSpace(rootCause)
		} else if strings.HasPrefix(up, "CONFIDENCE:") {
			confidence = strings.ToLower(strings.TrimSpace(s[len("CONFIDENCE:"):]))
		} else if strings.HasPrefix(s, "- ") && rootCause != "" {
			suggestions = append(suggestions, strings.TrimPrefix(s, "- "))
		}
	}
	if len(suggestions) == 0 {
		suggestions = []string{"请复核已收集的证据后重新诊断"}
	}
	return rootCause, suggestions, normalizeConfidence(confidence)
}

func (a *Agent) runStrategy(ctx context.Context, strat strategy.Strategy, contextData map[string]any, pool *memory.EvidencePool) {
	currentDecision := ""
	for _, batch := range strat.GroupByPriority() {
		// Execute runnable tool_call steps in the batch concurrently.
		var wg sync.WaitGroup
		for _, step := range batch {
			if !step.ShouldRun(currentDecision) {
				continue
			}
			wg.Add(1)
			go func(s strategy.Step) {
				defer wg.Done()
				defer func() {
					if rec := recover(); rec != nil {
						pool.Add(memory.NewEvidence("strategy", "panic",
							fmt.Sprintf("%s panicked: %v", s.Tool, rec),
							fmt.Sprint(rec), 0.0))
						a.emit(fmt.Sprintf("[strategy] step %s panicked: %v", s.Tool, rec))
					}
				}()
				a.executeStep(ctx, s, contextData, pool)
			}(step)
		}
		wg.Wait()

		// After each batch, handle llm_decide steps (one decision per batch).
		for _, step := range batch {
			if step.Action == "llm_decide" && step.ShouldRun(currentDecision) {
				decision := a.runLLMDecide(ctx, step, pool, contextData)
				if decision != "" {
					currentDecision = decision
					a.emit(fmt.Sprintf("[strategy] LLM decided: %s", currentDecision))
				}
				break
			}
		}
	}
}

func (a *Agent) executeStep(ctx context.Context, step strategy.Step, contextData map[string]any, pool *memory.EvidencePool) {
	if step.Action == "fingerprint_match" {
		component := strOr(contextData["component"], "")
		hit := a.fingerprintHit(component, contextData)
		if hit == nil {
			a.emit(fmt.Sprintf("[strategy] fingerprint_match: no known case for %s", component))
			return
		}
		rootCause := strOr(hit["root_cause"], "")
		if rootCause == "" {
			return
		}
		pool.Add(memory.NewEvidence("strategy", "fingerprint_match",
			"命中历史指纹: "+trunc(rootCause, 160), rootCause, 0.95))
		a.emit(fmt.Sprintf("[strategy] fingerprint_match hit: %s", trunc(rootCause, 120)))
		return
	}
	if step.Action == "llm_decide" {
		return
	}
	if step.Action != "tool_call" {
		return
	}
	tool, ok := a.tools[step.Tool]
	if !ok {
		pool.Add(memory.NewEvidence("strategy", "error",
			fmt.Sprintf("unknown tool: %s", step.Tool),
			fmt.Sprintf("Strategy referenced non-existent tool %s", step.Tool), 0.0))
		return
	}
	params := step.RenderParams(contextData)
	a.emit(fmt.Sprintf("[strategy] %s(%v)", step.Tool, params))
	toolCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	result, err := tool.Handler(toolCtx, params)
	if err != nil {
		toolCallsTotal.Inc(step.Tool, "error")
		pool.Add(memory.NewEvidence("strategy", "error",
			fmt.Sprintf("%s failed: %v", step.Tool, err), err.Error(), 0.0))
		return
	}
	toolCallsTotal.Inc(step.Tool, statusLabel(result.Success))
	if result.Success {
		pool.Add(memory.NewEvidence("strategy", "tool:"+step.Tool, trunc(result.Data, 200), result.Data, 0.8))
	} else {
		pool.Add(memory.NewEvidence("strategy", "tool:"+step.Tool, trunc(result.Error, 200), result.Error, 0.2))
	}
}

func (a *Agent) runLLMDecide(ctx context.Context, step strategy.Step, pool *memory.EvidencePool, contextData map[string]any) string {
	var choicesDesc strings.Builder
	var values []string
	for _, c := range step.DecideChoices {
		choicesDesc.WriteString(fmt.Sprintf("- %s: %s\n", c.Value, c.Description))
		if c.Value != "" {
			values = append(values, c.Value)
		}
	}
	if len(values) == 0 {
		return ""
	}

	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	decisionTool := &anthropic.ToolParam{
		Name:        "branch_decision",
		Description: anthropic.String("Pick the diagnostic branch."),
		InputSchema: schemaFromMap(tools.SchemaForObject(map[string]any{
			"choice": map[string]any{"type": "string", "enum": enum},
			"reason": map[string]any{"type": "string"},
		}, []string{"choice"})),
	}

	prompt := fmt.Sprintf("%s\n\nEvidence collected so far:\n%s\n\nCluster context:\n%s\n\nChoices:\n%s",
		step.DecidePrompt, pool.Summary(), jsonString(stripTopology(contextData)), choicesDesc.String())
	prompt += languageDirective(a.language)

	resp, err := a.callLLM(ctx, anthropic.MessageNewParams{
		Model:       anthropic.Model(a.model),
		MaxTokens:   128,
		Temperature: anthropic.Float(0.1),
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Tools:       []anthropic.ToolUnionParam{{OfTool: decisionTool}},
		ToolChoice:  anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "branch_decision"}},
	})
	if err != nil {
		return ""
	}
	for _, b := range resp.Content {
		if b.Type == "tool_use" && b.Name == "branch_decision" {
			var args struct {
				Choice string `json:"choice"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(b.Input, &args) == nil {
				a.emit(fmt.Sprintf("[strategy] LLM branch: %s (%s)", args.Choice, args.Reason))
				return args.Choice
			}
		}
	}
	return ""
}

// ---- Helpers ----

func (a *Agent) schemas() []anthropic.ToolUnionParam {
	names := make([]string, 0, len(a.tools))
	for name := range a.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]anthropic.ToolUnionParam, 0, len(names))
	for _, name := range names {
		d := a.tools[name]
		out = append(out, anthropic.ToolUnionParam{OfTool: toolParamFromSchema(d.Schema())})
	}
	return out
}

func toolParamFromSchema(schema map[string]any) *anthropic.ToolParam {
	name, _ := schema["name"].(string)
	desc, _ := schema["description"].(string)
	inputSchema, _ := schema["input_schema"].(map[string]any)
	return &anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(desc),
		InputSchema: schemaToInputSchema(inputSchema),
	}
}

func schemaToInputSchema(s map[string]any) anthropic.ToolInputSchemaParam {
	var inp anthropic.ToolInputSchemaParam
	if props, ok := s["properties"].(map[string]any); ok {
		inp.Properties = props
	}
	if req, ok := s["required"].([]string); ok {
		inp.Required = req
	}
	// Type defaults to "object" via the constant.Object zero value.
	return inp
}

func (a *Agent) reportDiagnosisTool() *anthropic.ToolParam {
	return &anthropic.ToolParam{
		Name: "report_diagnosis",
		Description: anthropic.String("Submit the final diagnosis report. " +
			"root_cause and suggestions are shown to a human operator and must be " +
			"written in the required output language (" + languageName(a.language) + ")."),
		InputSchema: schemaFromMap(tools.SchemaForObject(map[string]any{
			"root_cause":         map[string]any{"type": "string", "description": "Root cause in " + languageName(a.language)},
			"confidence":         map[string]any{"type": "string", "enum": []any{"high", "medium", "low"}},
			"suggestions":        map[string]any{"type": "array", "items": map[string]any{"type": "string", "description": "Actionable fix in " + languageName(a.language)}},
			"missing_evidence":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"evidence_citations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, []string{"root_cause", "confidence", "suggestions"})),
	}
}

// schemaFromMap converts a JSON-schema map to an anthropic.ToolInputSchemaParam.
func schemaFromMap(s map[string]any) anthropic.ToolInputSchemaParam {
	var out anthropic.ToolInputSchemaParam
	if props, ok := s["properties"].(map[string]any); ok {
		out.Properties = props
	}
	if req, ok := s["required"].([]string); ok {
		out.Required = req
	}
	return out
}

func (a *Agent) emit(msg string) {
	if a.onEvent != nil {
		a.onEvent(msg)
	}
}

// logLLMError surfaces the provider's own error payload. A generic "%v" hides
// the reason ("Thinking mode does not support this tool_choice"), which is
// exactly the detail needed to fix a rejected request.
func (a *Agent) logLLMError(err error, params anthropic.MessageNewParams) {
	attrs := []any{
		"model", a.model,
		"error", err.Error(),
		"max_tokens", params.MaxTokens,
		"tools", len(params.Tools),
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		attrs = append(attrs,
			"status", apiErr.StatusCode,
			"request_id", apiErr.RequestID,
			"body", trunc(apiErr.RawJSON(), 800),
		)
	}
	slog.Error("llm call failed", attrs...)
}

// callLLM wraps a single LLM request with token accounting and error metrics.
func (a *Agent) callLLM(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	a.llmCalls++
	if a.callLLMHook != nil {
		return a.callLLMHook(ctx, params)
	}
	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		llmErrors.Inc()
		// A failed LLM call used to vanish into a degrade path, so a model that
		// rejects the request shape looked like "the model found nothing".
		// Log it at ERROR with the API's own status/body.
		a.logLLMError(err, params)
		return nil, err
	}
	llmCallsTotal.Inc()
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		a.llmTokens += int64(resp.Usage.InputTokens + resp.Usage.OutputTokens)
		llmTokensTotal.Add(int64(resp.Usage.InputTokens + resp.Usage.OutputTokens))
	}
	return resp, nil
}

func blockToParam(b anthropic.ContentBlockUnion) anthropic.ContentBlockParamUnion {
	switch b.Type {
	case "text":
		return anthropic.NewTextBlock(b.Text)
	case "tool_use":
		// Re-emit as a tool_use block param so the assistant message round-trips.
		return anthropic.NewToolUseBlock(b.ID, json.RawMessage(b.Input), b.Name)
	default:
		return anthropic.NewTextBlock("")
	}
}

func toolUseAsBlock(u anthropic.ContentBlockUnion) struct {
	ID    string
	Name  string
	Input json.RawMessage
} {
	return struct {
		ID    string
		Name  string
		Input json.RawMessage
	}{ID: u.ID, Name: u.Name, Input: u.Input}
}

func firstText(resp *anthropic.Message) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (a *Agent) buildSystemPrompt(evidence *memory.EvidencePool, contextData map[string]any) string {
	return fmt.Sprintf(
		"You are a senior SRE diagnosing a big data platform issue.\n\n"+
			"## Evidence Already Collected (Phase 2 — do NOT re-query)\n%s\n\n"+
			"## Playbook\n1. **YARN first**: If query_yarn shows apps, call app_nodes to find EXACT nodes.\n"+
			"2. **Standalone**: If no YARN apps, ssh_exec 'find /data -name *flink*.log'.\n"+
			"3. **DeepWiki**: Verify specific error classes against component repo.\n"+
			"4. **3-turn rule**: If 3 tool calls find NO new evidence, STOP.\n\n"+
			"ALWAYS use Keywords='ERROR,Exception,FATAL' on large logs."+
			languageDirective(a.language),
		evidence.Summary(),
	)
}

func jsonInputToMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	return map[string]any{}
}

func stripTopology(m map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range m {
		if k == "topology" {
			continue
		}
		out[k] = v
	}
	return out
}

func stringMap(m map[string]any) map[string]string {
	out := make(map[string]string)
	for k, v := range m {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

func mapEvidence(evs []memory.Evidence) []map[string]any {
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ToDict())
	}
	return out
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func toStrSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return strings.Split(t, ",")
	default:
		return nil
	}
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func shortID() string {
	// Deterministic-enough unique tail without crypto/rand import at call site.
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
}
