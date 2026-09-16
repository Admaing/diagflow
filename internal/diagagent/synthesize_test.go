package diagagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Admaing/diagflow/internal/memory"
	"github.com/anthropics/anthropic-sdk-go"
)

// synthesize must pick a tool_choice the model actually accepts. Thinking-mode
// models reject {"type":"tool"} with a 400, which silently produced a report
// with an empty root cause. "any" still guarantees a tool call.
func TestSynthesizeRequestsAnyToolChoice(t *testing.T) {
	a := New(WithModel("test-model"))
	var got anthropic.ToolChoiceUnionParam
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		got = params.ToolChoice
		m := &anthropic.Message{}
		m.Content = append(m.Content, anthropic.ContentBlockUnion{
			Type: "tool_use",
			Name: "report_diagnosis",
			Input: []byte(`{"root_cause":"DataNode on core1 unreachable per node logs",
				"confidence":"high","suggestions":["check core1 network"]}`),
		})
		return m, nil
	}

	rootCause, suggestions, confidence := a.synthesize(context.Background(),
		&memory.EvidencePool{}, map[string]any{}, "hdfs", "NameNode WARN", "")

	if got.OfTool != nil || got.OfAny == nil {
		t.Fatalf("expected OfAny tool_choice, got %+v", got)
	}
	if rootCause == "" || len(suggestions) == 0 || confidence != "high" {
		t.Fatalf("unexpected synthesis result: %q %v %q", rootCause, suggestions, confidence)
	}
}

// A failing synthesis call must not be swallowed: the caller falls back to the
// ReAct conclusion so the report still carries a root cause.
func TestSynthesizeReportsCallError(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		return nil, errors.New("invalid_request_error: Thinking mode does not support this tool_choice")
	}

	var events []string
	a.onEvent = func(msg string) { events = append(events, msg) }
	rootCause, _, _ := a.synthesize(context.Background(), &memory.EvidencePool{}, map[string]any{}, "hdfs", "warn", "")

	if rootCause != "" {
		t.Fatalf("expected empty root cause on LLM failure, got %q", rootCause)
	}
	if !strings.Contains(strings.Join(events, "\n"), "synthesis LLM call failed") {
		t.Fatalf("expected the failure to be surfaced, events: %v", events)
	}
}

// Thinking-mode models spend max_tokens on reasoning first. A synthesiser lit
// with a small budget emits a tool_use whose input is empty, which reads as
// "no findings" and silently produced a report with no root cause.
func TestSynthesizeGivesThinkingEnoughBudget(t *testing.T) {
	a := New(WithModel("test-model"))
	var got int64
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		got = params.MaxTokens
		m := &anthropic.Message{}
		m.Content = append(m.Content, anthropic.ContentBlockUnion{
			Type: "tool_use", Name: "report_diagnosis",
			Input: []byte(`{"root_cause":"r","confidence":"low","suggestions":["s"]}`),
		})
		return m, nil
	}
	a.synthesize(context.Background(), &memory.EvidencePool{}, map[string]any{}, "hdfs", "warn", "")
	if got < 4096 {
		t.Fatalf("synthesize budget %d is too small for a thinking model", got)
	}
}

// A truncated tool_use input must be surfaced, not silently swallowed.
func TestSynthesizeReportsTruncatedToolInput(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		m := &anthropic.Message{}
		m.StopReason = "max_tokens"
		m.Content = append(m.Content, anthropic.ContentBlockUnion{
			Type: "tool_use", Name: "report_diagnosis", Input: []byte(`{}`),
		})
		return m, nil
	}
	var events []string
	a.onEvent = func(msg string) { events = append(events, msg) }
	if rc, _, _ := a.synthesize(context.Background(), &memory.EvidencePool{}, map[string]any{}, "hdfs", "warn", ""); rc != "" {
		t.Fatalf("expected empty root cause, got %q", rc)
	}
	if !strings.Contains(strings.Join(events, "\n"), "unusable report_diagnosis input") {
		t.Fatalf("expected a truncation warning, events: %v", events)
	}
}
