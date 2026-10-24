package diagagent

import (
	"context"
	"strings"
	"testing"

	"github.com/Admaing/diagflow/internal/memory"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestLanguageDirective(t *testing.T) {
	if d := languageDirective("zh-CN"); !strings.Contains(d, "简体中文") {
		t.Fatalf("zh-CN directive should demand Simplified Chinese, got %q", d)
	}
	if d := languageDirective(""); !strings.Contains(d, "简体中文") {
		t.Fatalf("empty language should default to Simplified Chinese, got %q", d)
	}
	if d := languageDirective("en"); !strings.Contains(d, "English") {
		t.Fatalf("en directive should demand English, got %q", d)
	}
}

// The synthesis prompt and the report tool must both carry the configured
// output language, otherwise the model defaults to English prose in a report
// whose labels are Chinese.
func TestSynthesizeCarriesOutputLanguage(t *testing.T) {
	a := New(WithModel("test-model"))
	a.language = "zh-CN"

	var prompt string
	var toolDesc string
	a.callLLMHook = func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
		if len(params.Messages) > 0 {
			for _, b := range params.Messages[0].Content {
				if b.OfText != nil {
					prompt += b.OfText.Text
				}
			}
		}
		if len(params.Tools) > 0 && params.Tools[0].OfTool != nil {
			toolDesc = params.Tools[0].OfTool.Description.Value
		}
		m := &anthropic.Message{}
		m.Content = append(m.Content, anthropic.ContentBlockUnion{
			Type: "tool_use", Name: "report_diagnosis",
			Input: []byte(`{"root_cause":"x","confidence":"high","suggestions":["y"]}`),
		})
		return m, nil
	}

	_, _, _, _ = a.synthesize(context.Background(), &memory.EvidencePool{}, map[string]any{}, "hdfs", "warn", "")

	if !strings.Contains(prompt, "简体中文") {
		t.Fatalf("synthesis prompt missing language directive: %q", prompt)
	}
	if !strings.Contains(toolDesc, "简体中文") {
		t.Fatalf("report tool description missing output language: %q", toolDesc)
	}
}

func TestReactSystemPromptCarriesOutputLanguage(t *testing.T) {
	a := New(WithModel("test-model"))
	a.language = "zh-CN"
	system := a.buildSystemPrompt(&memory.EvidencePool{}, map[string]any{})
	if !strings.Contains(system, "简体中文") {
		t.Fatalf("ReAct system prompt missing language directive: %q", system)
	}
}
