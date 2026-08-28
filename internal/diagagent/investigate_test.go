package diagagent

import (
	"strings"
	"testing"

	"github.com/Admaing/diagflow/internal/rag/vectorstore"
)

// fakeKB implements KnowledgeBase in-memory for tests.
type fakeKB struct {
	realEmbeddings bool
	intents        map[string]string // intent → root cause
	added          []string
}

func (f *fakeKB) Search(query string, n int) []vectorstore.SearchResult {
	// Minimal: return a pseudo-match if query approximates a stored intent.
	root, ok := f.intents[query]
	if !ok {
		return nil
	}
	score := 0.5
	return []vectorstore.SearchResult{{
		ID:          query,
		Document:    query,
		Metadata:    map[string]any{"root_cause": root},
		FusionScore: &score,
	}}
}

func (f *fakeKB) FingerprintMatch(component, errorPattern, version string) map[string]any {
	return nil
}

func (f *fakeKB) AddCase(component, errorPattern, version, rootCause string, suggestions []string) string {
	return ""
}

func (f *fakeKB) AddInvestigationIntent(intentDesc, rootCause, component, version string) {
	f.added = append(f.added, intentDesc)
	f.intents[intentDesc] = rootCause
}

func (f *fakeKB) HasRealEmbeddings() bool { return f.realEmbeddings }

func newTestAgent() *Agent {
	a := New()
	a.kb = &fakeKB{intents: map[string]string{}}
	return a
}

func TestTraceRenderMarkdown(t *testing.T) {
	trace := &InvestigationTrace{Component: "flink", ProblemType: "job_failure"}
	step := trace.AddStep("定位失败任务并取容器日志")
	step.Judgment = "TaskManager OOM，heap 2G 配 4 slots"
	step.LogSnippets = []string{"java.lang.OutOfMemoryError: Java heap space"}
	step.DeepwikiPrompt = "repo: apache/flink\nquestion: 这是已知 bug？"

	md := trace.RenderMarkdown()
	if !strings.Contains(md, "定位失败任务并取容器日志") {
		t.Fatalf("markdown missing intent:\n%s", md)
	}
	if !strings.Contains(md, "OutOfMemoryError") {
		t.Fatalf("markdown missing log snippet:\n%s", md)
	}
	if !strings.Contains(md, "apache/flink") {
		t.Fatalf("markdown missing deepwiki prompt:\n%s", md)
	}
}

func TestTraceRenderHistoricalMatch(t *testing.T) {
	trace := &InvestigationTrace{}
	step := trace.AddStep("查 TaskManager OOM")
	step.MatchedHistorical = true
	step.HistoricalRootCause = "heap 太小"
	md := trace.RenderMarkdown()
	if !strings.Contains(md, "命中历史排查思路") {
		t.Fatalf("markdown missing historical-match block:\n%s", md)
	}
}

func TestSearchSimilarIntentMatch(t *testing.T) {
	a := newTestAgent()
	kb := a.kb.(*fakeKB)
	kb.realEmbeddings = true
	kb.intents["查 TaskManager OOM"] = "heap 太小"

	root, matched := a.searchSimilarIntent("查 TaskManager OOM")
	if !matched || root != "heap 太小" {
		t.Fatalf("expected match, got matched=%v root=%q", matched, root)
	}
}

func TestSearchSimilarIntentNoEmbeddings(t *testing.T) {
	a := newTestAgent()
	kb := a.kb.(*fakeKB)
	kb.realEmbeddings = false
	kb.intents["查 TaskManager OOM"] = "heap 太小"

	_, matched := a.searchSimilarIntent("查 TaskManager OOM")
	if matched {
		t.Fatal("without real embeddings, semantic match must be skipped")
	}
}

func TestBuildDeepwikiPrompt(t *testing.T) {
	a := newTestAgent()
	a.repositoryMap = map[string]string{"flink": "apache/flink"}
	step := &InvestigationStep{LogSnippets: []string{"java.lang.OutOfMemoryError: Java heap space"}}

	prompt := a.buildDeepwikiPrompt("flink", "1.14.3", step)
	if !strings.Contains(prompt, "apache/flink") {
		t.Fatalf("prompt missing repo:\n%s", prompt)
	}
	if !strings.Contains(prompt, "OutOfMemoryError") {
		t.Fatalf("prompt missing error class:\n%s", prompt)
	}
}

func TestConfirmCorrectSkipsHistorical(t *testing.T) {
	a := newTestAgent()
	kb := a.kb.(*fakeKB)
	kb.realEmbeddings = true

	trace := &InvestigationTrace{}
	s1 := trace.AddStep("查 A")
	s1.MatchedHistorical = true
	s1.HistoricalRootCause = "已知"
	trace.AddStep("查 B") // executed, should be written back

	report := &Report{Component: "flink", RootCause: "新根因", Trace: trace}
	a.ConfirmCorrect(report)

	if len(kb.added) != 1 || kb.added[0] != "查 B" {
		t.Fatalf("expected only the executed intent written back, got %v", kb.added)
	}
}
