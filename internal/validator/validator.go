// Package validator implements deterministic conclusion checks: layer 1
// (format) and layer 2 (root cause references evidence). The retry-with-feedback
// loop lives in diagagent. An independent LLM review (layer 3) is not implemented.
package validator

import (
	"strings"
)

// Validator performs the deterministic (LLM-free) conclusion checks.
type Validator struct {
	evidenceKeywords []string
}

// New builds a Validator.
func New() *Validator {
	return &Validator{
		evidenceKeywords: []string{
			"log", "metric", "config", "error", "oom",
			"timeout", "memory", "disk", "cpu", "checkpoint",
			"known", "issue", "bug", "deepwiki",
			// Reports default to zh-CN; an otherwise evidence-backed Chinese
			// conclusion must not be rejected merely for lacking an English
			// noun from the list above.
			"日志", "指标", "配置", "错误", "异常", "内存", "磁盘", "网络", "超时", "已知",
		},
	}
}

// ValidateLayer checks layers 1–2 deterministically. Returns (pass, feedback).
// There is no LLM review layer; the caller retries synthesis with feedback once.
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
