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
// fails, times out, or returns unparseable output. It routes toward capable
// models so a classifier hiccup never silently downgrades quality.
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
		Difficulty:  "trivial",
		EstTokensIn: 100,
		EstTokensOut: 200,
		Domain:      "other",
	}
}

// tokensPerByte is a conservative character-to-token ratio used for
// deterministic sizing. Real tokenizers average ~4 bytes/token for English;
// we use 3 to err on the side of over-estimating (safer for context filtering).
const tokensPerByte = 3

// bytesPerImage is a rough token cost for an image. Most vision APIs charge
// 1000-2000 tokens per image depending on resolution; 1500 is a safe midpoint.
const tokensPerImage = 1500

// EstimateRequestTokens returns a deterministic lower-bound token estimate for
// the full inbound request: system prompt, all message content, all tool
// definitions, and image overhead. This estimate is used to floor the
// classifier's LLM-generated guess so that large system prompts or long
// conversation histories cannot cause a model to be selected whose context
// window is actually too small.
func EstimateRequestTokens(chat llm.ChatRequest) int {
	n := len(chat.SystemPrompt) / tokensPerByte
	for _, m := range chat.Messages {
		n += len(m.Content) / tokensPerByte
		n += len(m.Images) * tokensPerImage
	}
	for _, t := range chat.Tools {
		n += len(t.Name)/tokensPerByte + len(t.Description)/tokensPerByte + len(t.Parameters)/tokensPerByte
	}
	return n
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
