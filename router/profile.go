// Package router contains the routing core: task classification, model
// scoring, and the decision record. It is pure logic over harness types
// plus the config catalog; its only I/O is the classifier LLM call.
package router

import "github.com/sausheong/harness/llm"

// TaskProfile is the abstract description of a request the classifier
// produces and the scorer consumes. Difficulty is one of
// "trivial"|"low"|"medium"|"high"; Domain is "code"|"writing"|"qa"|"math"|"other".
type TaskProfile struct {
	Difficulty     string `json:"difficulty"`
	NeedsReasoning bool   `json:"needs_reasoning"`
	NeedsVision    bool   `json:"needs_vision"`
	NeedsTools     bool   `json:"needs_tools"`
	EstTokensIn    int    `json:"est_tokens_in"`
	EstTokensOut   int    `json:"est_tokens_out"`
	Domain         string `json:"domain"`
}

// DefaultProfile is the conservative fallback used when the classifier call
// fails, times out, or returns unparseable output. It prefers capable models
// without making optional reasoning support a hard availability requirement.
func DefaultProfile() TaskProfile {
	return TaskProfile{
		Difficulty:     "high",
		NeedsReasoning: true,
		EstTokensIn:    4000,
		EstTokensOut:   2000,
		Domain:         "other",
	}
}

// TrivialProfile is the optimistic profile used when the short-circuit fires.
// Routes toward cheap/fast models; no special capabilities assumed.
func TrivialProfile() TaskProfile {
	return TaskProfile{
		Difficulty:   "trivial",
		EstTokensIn:  100,
		EstTokensOut: 200,
		Domain:       "other",
	}
}

// tokensPerByte is a conservative byte-to-token ratio. Real tokenizers average
// ~4 bytes/token for English; we use 3 to over-estimate (safer for context
// filtering). We sum all bytes before dividing to avoid losing short fields
// to per-field integer truncation.
const tokensPerByte = 3

// tokensPerImage is a conservative token cost per image. Vision APIs typically
// charge 1000–2000 tokens depending on resolution; we use 2000 (the high end)
// to avoid under-estimating.
const tokensPerImage = 2000

// msgOverheadBytes is the approximate structural overhead per message
// (role tag, formatting). Conservative estimate.
const msgOverheadBytes = 12

// EstimateRequestTokens returns a conservative token estimate for the full
// inbound request: system prompt, all message content (including tool-call
// arguments and tool-result text), tool definitions, and image overhead.
// It sums all byte lengths before applying ceiling division so that no field
// is silently rounded to zero.
//
// This is an approximation — accurate only within a factor of ~2 without
// provider tokenizers — but it is intentionally conservative (over-estimates)
// to serve as a reliable lower bound for context-window filtering.
func EstimateRequestTokens(chat llm.ChatRequest) int {
	var totalBytes int

	// SystemPromptParts, when non-empty, replaces SystemPrompt in harness
	// semantics — and anthropicio.Decode populates both for a structured
	// prompt, so counting each unconditionally would double the estimate.
	if len(chat.SystemPromptParts) > 0 {
		for _, part := range chat.SystemPromptParts {
			totalBytes += len(part.Text)
		}
	} else {
		totalBytes += len(chat.SystemPrompt)
	}

	for _, m := range chat.Messages {
		totalBytes += msgOverheadBytes
		totalBytes += len(m.Content)
		// Tool-call arguments from assistant messages.
		for _, tc := range m.ToolCalls {
			totalBytes += len(tc.ID) + len(tc.Name) + len(tc.Input)
		}
		// Tool result metadata.
		if m.ToolCallID != "" {
			totalBytes += len(m.ToolCallID)
		}
	}

	for _, t := range chat.Tools {
		totalBytes += len(t.Name) + len(t.Description) + len(t.Parameters)
	}

	// Images are billed separately in tokens, not bytes.
	imageTokens := 0
	for _, m := range chat.Messages {
		imageTokens += len(m.Images) * tokensPerImage
	}

	return (totalBytes+tokensPerByte-1)/tokensPerByte + imageTokens
}

// isTrivial reports whether a request is simple enough to skip the classifier.
// Conditions: single-turn (no prior assistant messages), last user turn is
// short, no images, and no tools. Multi-turn conversations are never trivial —
// a short reply like "yes" may be answering a complex prior question and needs
// the full context to route correctly.
func isTrivial(chat llm.ChatRequest, turn llm.Message) bool {
	if len(chat.Tools) > 0 {
		return false
	}
	if len(turn.Images) > 0 {
		return false
	}
	for _, m := range chat.Messages {
		if m.Role == "assistant" {
			return false
		}
	}
	return len(turn.Content) <= shortCircuitBytes
}
