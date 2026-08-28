// Package strategy mirrors diagflow/core/strategy.py — YAML-driven diagnostic
// plans. Steps reference tool_call / fingerprint_match / llm_decide actions.
package strategy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Step is a single step in a diagnostic strategy.
type Step struct {
	Action        string         `yaml:"action"` // "fingerprint_match" | "tool_call" | "llm_decide"
	Description   string         `yaml:"description"`
	Tool          string         `yaml:"tool"` // only for action=tool_call
	Params        map[string]any `yaml:"params"`
	priority      *int           // nil = omitted (defaults to 10 on access)
	DecidePrompt  string         `yaml:"decide_prompt"`
	DecideChoices []DecideChoice `yaml:"decide_choices"`
	IfDecision    string         `yaml:"if_decision"`
}

// UnmarshalYAML captures priority presence to distinguish omitted (default 10)
// from explicit 0 (fingerprint_match).
func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	type plain Step
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*s = Step(p)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "priority" {
			var n int
			if err := node.Content[i+1].Decode(&n); err == nil {
				s.priority = &n
			}
		}
	}
	return nil
}

// Priority returns the step priority (0 if explicitly set, else default 10).
func (s Step) Priority() int {
	if s.priority == nil {
		return 10
	}
	return *s.priority
}

// DecideChoice is an item in an llm_decide step's choices.
type DecideChoice struct {
	Value       string `yaml:"value"`
	Description string `yaml:"description"`
}

// ShouldRun reports whether this step should execute given the current decision.
func (s Step) ShouldRun(currentDecision string) bool {
	if s.IfDecision == "" {
		return true
	}
	return currentDecision == s.IfDecision
}

// RenderParams resolves {{ context.xxx }} template variables from context.
func (s Step) RenderParams(context map[string]any) map[string]any {
	rendered := make(map[string]any, len(s.Params))
	for k, v := range s.Params {
		rendered[k] = renderValue(v, context)
	}
	return rendered
}

var tmplRe = regexp.MustCompile(`\{\{\s*(context\.\w+)\s*\}\}`)

func renderValue(v any, context map[string]any) any {
	switch t := v.(type) {
	case string:
		return tmplRe.ReplaceAllStringFunc(t, func(m string) string {
			sub := tmplRe.FindStringSubmatch(m)
			if len(sub) < 2 {
				return m
			}
			path := sub[1]
			if strings.HasPrefix(path, "context.") {
				key := path[len("context."):]
				if val, ok := context[key]; ok {
					return fmt.Sprintf("%v", val)
				}
				return ""
			}
			return m
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = renderValue(vv, context)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = renderValue(vv, context)
		}
		return out
	default:
		return v
	}
}

// Strategy is a complete diagnostic strategy for one (component, problem_type).
type Strategy struct {
	Component     string         `yaml:"component"`
	ProblemType   string         `yaml:"problem_type"`
	Version       string         `yaml:"version"`
	Description   string         `yaml:"description"`
	Steps         []Step         `yaml:"steps"`
	KnowledgeBase map[string]any `yaml:"knowledge_base"`
	Validation    map[string]any `yaml:"validation"`
	Output        map[string]any `yaml:"output"`
}

// GroupByPriority groups steps into batches (same priority runs in parallel).
func (s Strategy) GroupByPriority() [][]Step {
	if len(s.Steps) == 0 {
		return nil
	}
	sorted := make([]Step, len(s.Steps))
	copy(sorted, s.Steps)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority() < sorted[j].Priority() })

	var batches [][]Step
	var current []Step
	currentP := -1
	for _, step := range sorted {
		p := step.Priority()
		if currentP == -1 || p == currentP {
			current = append(current, step)
			currentP = p
		} else {
			batches = append(batches, current)
			current = []Step{step}
			currentP = p
		}
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// BuildTaskPrompt builds the Phase 3 task prompt.
func (s Strategy) BuildTaskPrompt(context map[string]string) string {
	return fmt.Sprintf(
		"Diagnose a %s issue.\n"+
			"Problem: %s\n"+
			"Cluster: %s\n"+
			"Region: %s\n"+
			"Version: %s\n"+
			"Detail: %s\n\n"+
			"Strategy-driven evidence collection has already run. Analyze the "+
			"evidence in the pool, form a root cause hypothesis, and use "+
			"deepwiki_query to verify if this is a known bug in the component's "+
			"version. Then produce a structured conclusion.",
		s.Component,
		getStr(context, "problem", "unknown"),
		getStr(context, "cluster_id", "unknown"),
		getStr(context, "region", "unknown"),
		getStr(context, "version", "unknown"),
		getStr(context, "detail", ""),
	)
}

func getStr(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

// Load resolves and parses the strategy for (component, problem_type).
func Load(component, problemType, strategiesDir string) Strategy {
	dir := strategiesDir
	if dir == "" {
		// Default: data/strategies relative to the repo root (three levels up
		// from internal/strategy).
		dir = filepath.Join(repoRoot(), "data", "strategies")
	}

	candidates := []string{
		filepath.Join(dir, fmt.Sprintf("%s_%s.yaml", component, problemType)),
		filepath.Join(dir, fmt.Sprintf("%s_default.yaml", component)),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return parseFile(candidate, component, problemType)
		}
	}
	return defaultStrategy(component, problemType)
}

// repoRoot returns the repository root directory.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	// Walk up to find the directory containing go.mod.
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func parseFile(path, component, problemType string) Strategy {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultStrategy(component, problemType)
	}
	var raw struct {
		Component     string         `yaml:"component"`
		ProblemType   string         `yaml:"problem_type"`
		Version       string         `yaml:"version"`
		Description   string         `yaml:"description"`
		Steps         []Step         `yaml:"steps"`
		KnowledgeBase map[string]any `yaml:"knowledge_base"`
		Validation    map[string]any `yaml:"validation"`
		Output        map[string]any `yaml:"output"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return defaultStrategy(component, problemType)
	}

	steps := raw.Steps
	if steps == nil {
		steps = []Step{}
	}
	// Normalize step defaults (action defaults to tool_call; priority handled
	// by the custom UnmarshalYAML).
	for i := range steps {
		if steps[i].Action == "" {
			steps[i].Action = "tool_call"
		}
	}

	return Strategy{
		Component:     firstNonEmpty(raw.Component, component),
		ProblemType:   firstNonEmpty(raw.ProblemType, problemType),
		Version:       raw.Version,
		Description:   raw.Description,
		Steps:         steps,
		KnowledgeBase: raw.KnowledgeBase,
		Validation:    raw.Validation,
		Output:        raw.Output,
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func defaultStrategy(component, problemType string) Strategy {
	p0, p1, p2 := 0, 1, 2
	return Strategy{
		Component:   component,
		ProblemType: problemType,
		Steps: []Step{
			{Action: "fingerprint_match", Description: "Check known issues first", priority: &p0},
			{Action: "tool_call", Tool: "query_node_log", Description: "Scan for errors in main log",
				Params: map[string]any{"log_path": "jobmanager.log", "keywords": "ERROR,FATAL"}, priority: &p1},
			{Action: "tool_call", Tool: "query_metrics", Description: "Check resource metrics",
				Params: map[string]any{}, priority: &p2},
		},
		KnowledgeBase: map[string]any{"fingerprint": true, "semantic_search": true, "bm25_search": true},
		Validation:    map[string]any{"min_evidence_count": 1},
		Output:        map[string]any{"suggestions_min": 2, "suggestions_max": 5},
	}
}
