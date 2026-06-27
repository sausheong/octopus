// Package router contains the routing core: task classification, model
// scoring, and the decision record. It is pure logic over harness types
// plus the config catalog; its only I/O is the classifier LLM call.
package router

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
