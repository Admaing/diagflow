// Package validator mirrors diagflow/core/validator.py — four-layer
// hallucination control. Layers 1–2 are deterministic (no LLM); layer 3 is an
// independent LLM review; layer 4 is retry-with-feedback in the caller.
package validator

import (
	"strings"
)

// Validator performs multi-layer conclusion validation.
type Validator struct {
	// Layer 3+ uses the Anthropic client directly (wrapped by the caller via
	// Verify). For deterministic layers 1–2 no client is needed.
	evidenceKeywords []string
}

// New builds a Validator.
func New() *Validator {
	return &Validator{
		evidenceKeywords: []string{
			"log", "metric", "config", "error", "oom",
			"timeout", "memory", "disk", "cpu", "checkpoint",
			"known", "issue", "bug", "deepwiki",
		},
	}
}

// ValidateLayer checks layers 1–2 deterministically. Returns (pass, feedback).
// The LLM review (layer 3) is invoked by the caller through the DiagAgent when
// a validation client is available.
func (v *Validator) ValidateLayer(rootCause string, suggestions []string, evidenceCount int) (bool, string) {
	// Layer 1: format
	if rootCause == "" || len(rootCause) < 10 {
		return false, "root cause too short"
	}
	if len(suggestions) == 0 {
		return false, "no suggestions provided"
	}
	if evidenceCount == 0 {
		return false, "no evidence — may be speculative"
	}

	// Layer 2: cross-source — root cause must reference evidence keywords.
	lower := strings.ToLower(rootCause)
	hasEv := false
	for _, kw := range v.evidenceKeywords {
		if strings.Contains(lower, kw) {
			hasEv = true
			break
		}
	}
	if !hasEv && evidenceCount > 0 {
		return false, "root cause doesn't reference evidence — may be hallucinated"
	}

	return true, ""
}
