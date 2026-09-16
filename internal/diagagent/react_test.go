package diagagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Admaing/diagflow/internal/memory"
	"github.com/Admaing/diagflow/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

// toolUseMessage builds an assistant turn that calls one tool.
func toolUseMessage(name, id string) *anthropic.Message {
	m := &anthropic.Message{}
	m.Content = append(m.Content, anthropic.ContentBlockUnion{Type: "tool_use", ID: id, Name: name})
	return m
}

// toolUseMessageArgs builds a tool call with explicit arguments.
func toolUseMessageArgs(name, id, args string) *anthropic.Message {
	m := &anthropic.Message{}
	m.Content = append(m.Content, anthropic.ContentBlockUnion{
		Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(args),
	})
	return m
}

// probeTool builds a tool whose handler records how often it actually ran and
// returns a body derived from that count.
func probeTool(invocations *int, body func(int) string) tools.Def {
	return tools.Def{
		Name:        "probe",
		Description: "records invocations",
		InputSchema: tools.Object(map[string]any{"x": tools.Prop("x")}, nil),
		Handler: func(context.Context, map[string]any) (tools.ToolResult, error) {
			*invocations++
			return tools.OK(body(*invocations)), nil
		},
	}
}

func textMessage(text string) *anthropic.Message {
	m := &anthropic.Message{}
	m.Content = append(m.Content, anthropic.ContentBlockUnion{Type: "text", Text: text})
	return m
}

// An agent that only ever calls tools must still end with a diagnosis once the
// turn budget runs out, instead of discarding every tool result it gathered.
func TestRunReactFinalizesWhenTurnBudgetExhausted(t *testing.T) {
	a := New(WithModel("test-model"))
	a.RegisterTools([]tools.Def{{
		Name:        "noop",
		Description: "returns its argument",
		InputSchema: tools.Object(map[string]any{"x": tools.Prop("x")}, nil),
		Handler: func(context.Context, map[string]any) (tools.ToolResult, error) {
			return tools.OK("evidence-1"), nil
		},
	}})

	calls := 0
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		calls++
		// Every call asks for a tool, until the finalize call which must not
		// offer tools at all — that is what forces a text answer.
		if len(params.Tools) > 0 {
			return toolUseMessage("noop", "t"), nil
		}
		return textMessage("ROOT CAUSE: disk full"), nil
	}

	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 3)
	if out != "ROOT CAUSE: disk full" {
		t.Fatalf("expected a forced conclusion, got %q", out)
	}
	if calls != 4 { // 3 tool turns + 1 tool-free finalize
		t.Fatalf("expected 4 LLM calls, got %d", calls)
	}
}

// The stall path must also return a conclusion, and stop offering tools.
func TestRunReactFinalizesOnStall(t *testing.T) {
	a := New(WithModel("test-model"))
	a.RegisterTools([]tools.Def{{
		Name:        "empty",
		Description: "returns nothing",
		InputSchema: tools.Object(nil, nil),
		Handler: func(context.Context, map[string]any) (tools.ToolResult, error) {
			return tools.OK(""), nil
		},
	}})

	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			return toolUseMessage("empty", "t"), nil
		}
		return textMessage("conclusion from stall"), nil
	}

	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 10)
	if out != "conclusion from stall" {
		t.Fatalf("expected a stall conclusion, got %q", out)
	}
}

// A plain text answer ends the loop immediately.
func TestRunReactReturnsTextWithoutTools(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		return textMessage("direct answer"), nil
	}
	if out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 5); out != "direct answer" {
		t.Fatalf("got %q", out)
	}
}

// A failing LLM must degrade gracefully, not panic.
func TestRunReactDegradesOnLLMError(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		return nil, context.DeadlineExceeded
	}
	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 5)
	if out == "" {
		t.Fatal("expected a degraded message, got empty output")
	}
}

// Repeating the same call with the same arguments cannot produce new
// information, so it must not reach the cluster a second time — and key order
// must not make two identical calls look different.
func TestRunReactSkipsIdenticalRepeatedCalls(t *testing.T) {
	a := New(WithModel("test-model"))
	invocations := 0
	a.RegisterTools([]tools.Def{probeTool(&invocations, func(n int) string {
		return fmt.Sprintf("body-%d", n)
	})})

	turns := 0
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			turns++
			if turns%2 == 0 {
				return toolUseMessageArgs("probe", "t", `{"x":1,"y":2}`), nil
			}
			return toolUseMessageArgs("probe", "t", `{"y":2,"x":1}`), nil
		}
		return textMessage("concluded"), nil
	}

	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 6)
	if out != "concluded" {
		t.Fatalf("got %q", out)
	}
	if invocations != 1 {
		t.Fatalf("tool ran %d times, want 1: identical calls must be deduplicated", invocations)
	}
}

// The stall detector must judge novelty by content. Re-reading a log returns
// the same bytes; treating that as progress is what stopped the 3-turn rule
// from ever firing on a live run.
func TestRunReactStallDetectorIgnoresRepeatedContent(t *testing.T) {
	a := New(WithModel("test-model"))
	invocations := 0
	a.RegisterTools([]tools.Def{probeTool(&invocations, func(int) string {
		return "the same log tail every time"
	})})

	turns := 0
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			turns++
			// Fresh arguments every turn, so deduplication cannot be what
			// stops this — only the unchanged content can.
			return toolUseMessageArgs("probe", "t", fmt.Sprintf(`{"x":%d}`, turns)), nil
		}
		return textMessage("stalled conclusion"), nil
	}

	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 20)
	if out != "stalled conclusion" {
		t.Fatalf("got %q", out)
	}
	// One novel turn, then three dry turns before the stall rule fires.
	if invocations != 4 {
		t.Fatalf("tool ran %d times, want 4: repeated content must count as dry", invocations)
	}
}

// maxTurns counts LLM turns, and one turn can carry a batch of calls, so the
// loop needs a call budget of its own.
func TestRunReactStopsAtToolCallBudget(t *testing.T) {
	a := New(WithModel("test-model"))
	invocations := 0
	a.RegisterTools([]tools.Def{probeTool(&invocations, func(n int) string {
		return fmt.Sprintf("unique-body-%d", n)
	})})

	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			return toolUseMessageArgs("probe", "t", fmt.Sprintf(`{"x":%d}`, invocations)), nil
		}
		return textMessage("budget conclusion"), nil
	}

	// maxTurns is far above the budget so the call budget is what stops it.
	out := a.runReact(context.Background(), "diagnose", &memory.EvidencePool{}, map[string]any{}, 100)
	if out != "budget conclusion" {
		t.Fatalf("got %q", out)
	}
	if invocations != maxReActToolCalls {
		t.Fatalf("tool ran %d times, want %d", invocations, maxReActToolCalls)
	}
}

func TestCanonicalArgsIsKeyOrderIndependent(t *testing.T) {
	a := canonicalArgs(json.RawMessage(`{"b":2,"a":1}`))
	b := canonicalArgs(json.RawMessage(`{"a":1,"b":2}`))
	if a != b {
		t.Fatalf("key order changed the fingerprint: %q vs %q", a, b)
	}
	if callFingerprint("t", json.RawMessage(`{"a":1}`)) == callFingerprint("t", json.RawMessage(`{"a":2}`)) {
		t.Fatal("different arguments must not share a fingerprint")
	}
	if callFingerprint("t1", json.RawMessage(`{"a":1}`)) == callFingerprint("t2", json.RawMessage(`{"a":1}`)) {
		t.Fatal("different tools must not share a fingerprint")
	}
	// Unparseable input must not panic; it falls back to the raw string.
	if canonicalArgs(json.RawMessage(`not json`)) != "not json" {
		t.Fatal("unparseable arguments should pass through")
	}
}

func TestContentFingerprintIgnoresSurroundingWhitespace(t *testing.T) {
	if contentFingerprint("abc") != contentFingerprint("  abc\n") {
		t.Fatal("whitespace-only differences must not look like new evidence")
	}
	if contentFingerprint("abc") == contentFingerprint("abd") {
		t.Fatal("different content must not collide")
	}
}

func TestCapToolResult(t *testing.T) {
	short := "small"
	if got := capToolResult(short, 100); got != short {
		t.Fatalf("short text was modified: %q", got)
	}
	long := strings.Repeat("x", 100)
	got := capToolResult(long, 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) {
		t.Fatalf("prefix not preserved: %q", got)
	}
	if !strings.Contains(got, "truncated 90 chars") {
		t.Fatalf("truncation is not reported: %q", got)
	}
	// A multi-byte rune straddling the cut must not leave invalid UTF-8.
	multibyte := strings.Repeat("日", 20)
	cut := capToolResult(multibyte, 9)
	if !utf8.ValidString(cut) {
		t.Fatalf("truncation produced invalid UTF-8: %q", cut)
	}
}
