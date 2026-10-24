package diagagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func diagnosisToolMessage(rootCause string, suggestions ...string) *anthropic.Message {
	input, _ := json.Marshal(map[string]any{
		"root_cause": rootCause, "confidence": "high", "suggestions": suggestions,
	})
	m := &anthropic.Message{}
	m.Content = append(m.Content, anthropic.ContentBlockUnion{
		Type: "tool_use", Name: "report_diagnosis", Input: input,
	})
	return m
}

// A failed synthesis is an infrastructure failure, not a diagnosis. This is
// the exact regression seen in production: an upstream 403 was returned as a
// medium-confidence root cause.
func TestDiagnoseReturnsErrorWhenSynthesisFails(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			return nil, errors.New("403 Forbidden: client ip not allowed")
		}
		return nil, errors.New("ReAct request failed")
	}

	rpt, err := a.Diagnose(t.Context(), "flink", "job_failure", map[string]any{
		"problem": "Job FAILED", "error_pattern": "CheckpointExpired",
	})
	if rpt != nil {
		t.Fatalf("a failed dependency must not produce a report, got %+v", rpt)
	}
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("expected ErrLLMUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("the underlying provider error should be retained for operators: %v", err)
	}
}

// If ReAct fails but the deterministic evidence is still synthesised into a
// valid conclusion, the result is useful but must be explicitly degraded.
func TestDiagnoseMarksReActFailureDegraded(t *testing.T) {
	a := New(WithModel("test-model"))
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Tools) > 0 {
			return diagnosisToolMessage("日志显示 DataNode 磁盘已满，写入失败", "清理 /data 旧日志"), nil
		}
		return nil, errors.New("ReAct unavailable")
	}

	rpt, err := a.Diagnose(t.Context(), "hdfs", "disk_full", map[string]any{
		"problem": "No space left on device", "error_pattern": "NoSpaceLeft",
	})
	if err != nil {
		t.Fatalf("synthesis succeeded from evidence; unexpected error: %v", err)
	}
	if rpt.Status != ReportStatusDegraded {
		t.Fatalf("status = %q, want %q", rpt.Status, ReportStatusDegraded)
	}
	if len(rpt.Warnings) == 0 || !strings.Contains(strings.Join(rpt.Warnings, "\n"), "ReAct") {
		t.Fatalf("expected a ReAct warning, got %v", rpt.Warnings)
	}
	if strings.Contains(strings.ToLower(rpt.RootCause), "llm error") {
		t.Fatalf("technical error leaked into root cause: %q", rpt.RootCause)
	}
}

// A deterministic fingerprint match is a complete answer and must not spend an
// LLM request, even when semantic embeddings are unavailable.
func TestDiagnoseFingerprintFastPath(t *testing.T) {
	kb := &fakeKB{intents: map[string]string{}, fingerprintHit: map[string]any{
		"root_cause": "已知 CheckpointExpired 问题", "suggestions": []string{"调大 checkpoint timeout"},
	}}
	a := New(WithModel("test-model"), WithKB(kb))
	a.callLLMHook = func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
		t.Fatal("fingerprint fast-path must not call the LLM")
		return nil, nil
	}

	rpt, err := a.Diagnose(t.Context(), "flink", "job_failure", map[string]any{
		"problem": "CheckpointExpired timeout", "error_pattern": "CheckpointExpired",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpt.Status != ReportStatusOK || !rpt.MatchedKnowledge {
		t.Fatalf("unexpected report: status=%q matched=%v", rpt.Status, rpt.MatchedKnowledge)
	}
	if a.llmCalls != 0 {
		t.Fatalf("fingerprint hit made %d LLM calls, want 0", a.llmCalls)
	}
}
