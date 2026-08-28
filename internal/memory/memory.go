// Package memory mirrors diagflow/core/memory.py — evidence pooling and
// session memory shared across the diagnostic engine.
package memory

import (
	"fmt"
	"sync"
	"time"
)

// Evidence is a single finding from one diagnostic step.
type Evidence struct {
	SourceAgent string         `json:"source_agent"`
	Category    string         `json:"category"`
	Summary     string         `json:"summary"`
	Detail      string         `json:"detail"`
	Confidence  float64        `json:"confidence"`
	Timestamp   string         `json:"timestamp"`
	RawData     map[string]any `json:"-"`
}

// NewEvidence builds an Evidence with a zero-time UTC timestamp default.
func NewEvidence(sourceAgent, category, summary, detail string, confidence float64) Evidence {
	return Evidence{
		SourceAgent: sourceAgent,
		Category:    category,
		Summary:     summary,
		Detail:      detail,
		Confidence:  confidence,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
}

// ToDict returns a map representation (excluding RawData), matching
// Evidence.to_dict in Python.
func (e Evidence) ToDict() map[string]any {
	return map[string]any{
		"source_agent": e.SourceAgent,
		"category":     e.Category,
		"summary":      e.Summary,
		"detail":       e.Detail,
		"confidence":   e.Confidence,
		"timestamp":    e.Timestamp,
	}
}

// ShortStr renders "[source_agent] summary (conf=0.9)".
func (e Evidence) ShortStr() string {
	return fmt.Sprintf("[%s] %s (conf=%.1f)", e.SourceAgent, e.Summary, e.Confidence)
}

// EvidencePool is a thread-safe collection of evidence gathered during diagnosis.
type EvidencePool struct {
	mu    sync.Mutex
	items []Evidence
}

// Add appends one evidence item.
func (p *EvidencePool) Add(e Evidence) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, e)
}

// AddMany appends several items.
func (p *EvidencePool) AddMany(es []Evidence) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, es...)
}

// All returns a snapshot copy of the pool.
func (p *EvidencePool) All() []Evidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Evidence, len(p.items))
	copy(out, p.items)
	return out
}

// ByAgent filters by source agent.
func (p *EvidencePool) ByAgent(agent string) []Evidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Evidence
	for _, e := range p.items {
		if e.SourceAgent == agent {
			out = append(out, e)
		}
	}
	return out
}

// ByCategory filters by category.
func (p *EvidencePool) ByCategory(category string) []Evidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Evidence
	for _, e := range p.items {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out
}

// HighConfidence filters by confidence >= threshold.
func (p *EvidencePool) HighConfidence(threshold float64) []Evidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Evidence
	for _, e := range p.items {
		if e.Confidence >= threshold {
			out = append(out, e)
		}
	}
	return out
}

// Summary renders a compact multi-line summary for LLM prompts.
func (p *EvidencePool) Summary() string {
	items := p.All()
	lines := []string{"=== Evidence Pool ==="}
	for _, e := range items {
		lines = append(lines, "  "+e.ShortStr())
	}
	if len(items) == 0 {
		lines = append(lines, "  (no evidence collected yet)")
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// Clear empties the pool.
func (p *EvidencePool) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = p.items[:0]
}

// Len returns the number of items.
func (p *EvidencePool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// Message is a single session message (role + content).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SessionMemory manages the conversation window for a diagnosis session.
type SessionMemory struct {
	MaxTokens int
	Messages  []Message
}

// NewSessionMemory returns a SessionMemory with the default 24k token window.
func NewSessionMemory() *SessionMemory {
	return &SessionMemory{MaxTokens: 24000}
}

// AddUser appends a user message.
func (s *SessionMemory) AddUser(content string) {
	s.Messages = append(s.Messages, Message{Role: "user", Content: content})
}

// AddAssistant appends an assistant message.
func (s *SessionMemory) AddAssistant(content string) {
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: content})
}

// GetMessages returns the message slice.
func (s *SessionMemory) GetMessages() []Message {
	return s.Messages
}
