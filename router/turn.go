package router

import "github.com/sausheong/harness/llm"

// LastUserTurn returns the most recent genuine user message — a message with
// Role=="user" and ToolCallID=="" (i.e. not a tool_result). Walking backward
// and skipping tool_result blocks is what makes routing deterministic across
// a tool-use continuation: the continuation re-derives from the same user
// turn that produced the original tool_use, so it routes to the same model.
// Returns (zero, false) if the history contains no genuine user turn.
func LastUserTurn(msgs []llm.Message) (llm.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" && m.ToolCallID == "" {
			return m, true
		}
	}
	return llm.Message{}, false
}
