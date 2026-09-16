package diagagent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxReActToolCalls bounds how many tool calls one ReAct loop may make.
//
// maxTurns alone is not a real budget: a single turn can carry a batch of
// parallel calls, so a live run spent 89 tool calls on one question while its
// turn count stayed inside the limit. Counting calls as well means a sprawling
// investigation ends in a forced conclusion rather than only when the wall
// clock runs out.
const maxReActToolCalls = 20

// maxReActResultChars bounds one tool result as fed back into the ReAct
// transcript. Payloads are capped at the MCP boundary already, but replaying a
// 60KB log tail into every later turn is what makes the late turns slow.
const maxReActResultChars = 8000

// duplicateCallNotice replaces the body of a repeated identical call. The
// original result already sits earlier in the conversation, so repeating it
// would only spend context.
const duplicateCallNotice = "[duplicate] This exact call was already made in this run. " +
	"Its result is printed earlier in the conversation and has not changed. " +
	"Do not repeat it — use that result, or probe something different."

// callFingerprint identifies one tool invocation so an identical repeat can be
// recognised before it reaches the cluster.
func callFingerprint(name string, input json.RawMessage) string {
	return name + "\x00" + canonicalArgs(input)
}

// canonicalArgs renders arguments so that the same object always produces the
// same string, whatever order the model emitted its keys in.
func canonicalArgs(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}
	// encoding/json sorts map keys, so this is deterministic.
	b, err := json.Marshal(m)
	if err != nil {
		return string(input)
	}
	return string(b)
}

// contentFingerprint identifies a result body. Treating any non-empty result
// as new evidence is what stopped the stall detector from ever firing:
// re-reading the same log returns the same bytes, but kept resetting the
// dry-turn streak, so the 3-turn rule never triggered.
func contentFingerprint(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return fmt.Sprintf("%x", sum[:8])
}

// capToolResult bounds one result before it enters the transcript. The cut is
// rune-aligned so a truncated payload stays valid UTF-8.
func capToolResult(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] +
		fmt.Sprintf("\n…[truncated %d chars; narrow the query if you need the rest]", len(text)-cut)
}
