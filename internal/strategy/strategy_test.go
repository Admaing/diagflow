package strategy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderParamsSimple(t *testing.T) {
	step := Step{Action: "tool_call", Tool: "ssh_exec", Params: map[string]any{"node_name": "{{ context.cluster_id }}-master1"}}
	rendered := step.RenderParams(map[string]any{"cluster_id": "uhadoop-test"})
	if rendered["node_name"] != "uhadoop-test-master1" {
		t.Fatalf("got %v", rendered["node_name"])
	}
}

func TestRenderParamsMissingKey(t *testing.T) {
	step := Step{Action: "tool_call", Tool: "t", Params: map[string]any{"key": "{{ context.missing }}"}}
	rendered := step.RenderParams(map[string]any{})
	if rendered["key"] != "" {
		t.Fatalf("got %v", rendered["key"])
	}
}

func TestRenderParamsNested(t *testing.T) {
	step := Step{
		Action: "tool_call", Tool: "t",
		Params: map[string]any{
			"outer": map[string]any{"inner": "{{ context.version }}"},
			"list":  []any{"{{ context.component }}", "static"},
		},
	}
	rendered := step.RenderParams(map[string]any{"version": "1.17", "component": "flink"})
	outer := rendered["outer"].(map[string]any)
	if outer["inner"] != "1.17" {
		t.Fatalf("got %v", outer["inner"])
	}
	list := rendered["list"].([]any)
	if list[0] != "flink" {
		t.Fatalf("got %v", list[0])
	}
}

func TestGroupByPriority(t *testing.T) {
	s := Strategy{Component: "flink", ProblemType: "t", Steps: []Step{
		{Action: "tool_call", Tool: "a", priority: intPtr(0)},
		{Action: "tool_call", Tool: "b", priority: intPtr(0)},
		{Action: "tool_call", Tool: "c", priority: intPtr(1)},
		{Action: "tool_call", Tool: "d", priority: intPtr(2)},
	}}
	batches := s.GroupByPriority()
	if len(batches) != 3 {
		t.Fatalf("got %d batches", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Fatalf("batch 0 len %d", len(batches[0]))
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flink_job_failure.yaml")
	content := "component: flink\nproblem_type: job_failure\nsteps:\n  - action: fingerprint_match\n    priority: 0\n  - action: tool_call\n    tool: query_yarn\n    params:\n      action: list_apps\n    priority: 1\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	s := Load("flink", "job_failure", dir)
	if s.Component != "flink" {
		t.Fatalf("component %s", s.Component)
	}
	if len(s.Steps) != 2 {
		t.Fatalf("steps %d", len(s.Steps))
	}
	if s.Steps[0].Action != "fingerprint_match" {
		t.Fatalf("step0 action %s", s.Steps[0].Action)
	}
	if s.Steps[0].Priority() != 0 {
		t.Fatalf("step0 priority %d", s.Steps[0].Priority())
	}
	if s.Steps[1].Priority() != 1 {
		t.Fatalf("step1 priority %d", s.Steps[1].Priority())
	}
}

func TestLoadPriorityDefaultsTo10(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flink_test.yaml")
	if err := writeFile(path, "steps:\n  - action: tool_call\n    tool: query_yarn\n"); err != nil {
		t.Fatal(err)
	}
	s := Load("flink", "test", dir)
	if len(s.Steps) != 1 {
		t.Fatalf("steps %d", len(s.Steps))
	}
	if s.Steps[0].Priority() != 10 {
		t.Fatalf("default priority %d", s.Steps[0].Priority())
	}
}

func TestFallbackToDefault(t *testing.T) {
	dir := t.TempDir()
	s := Load("unknown", "unknown", dir)
	if s.Component != "unknown" {
		t.Fatalf("component %s", s.Component)
	}
	if len(s.Steps) == 0 {
		t.Fatal("expected default steps")
	}
}

func intPtr(n int) *int { return &n }

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
