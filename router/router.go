package router

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
)

// Router turns an inbound chat request into a routing Decision.
type Router struct {
	cfg *config.Config
	reg *registry.Registry
	// classifyFn is the classification seam; defaults to Classify. Tests
	// override it to avoid real provider calls.
	classifyFn func(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile
}

// NewRouter builds a Router. The classifier provider is resolved per request
// from the registry using cfg.Classifier.Model.
func NewRouter(cfg *config.Config, reg *registry.Registry) *Router {
	return &Router{cfg: cfg, reg: reg, classifyFn: Classify}
}

// shortCircuitTokens is the content-length threshold (in bytes, used as a
// proxy for tokens) below which the classifier is skipped. Requests this
// small are assumed trivial/low-difficulty with no special capability needs.
const shortCircuitBytes = 500

// Route classifies the last genuine user turn, applies request-derived
// capability cross-checks, scores the catalog, and returns the Decision.
func (r *Router) Route(ctx context.Context, chat llm.ChatRequest) Decision {
	turn, ok := LastUserTurn(chat.Messages)
	var prof TaskProfile
	if !ok {
		prof = DefaultProfile()
	} else if isTrivial(chat, turn) {
		prof = TrivialProfile()
		slog.Debug("classifier skipped (trivial request)")
	} else {
		prov, model, err := r.reg.Resolve(r.cfg.Classifier.Model)
		if err != nil {
			slog.Warn("classifier provider unresolved; using default profile", "err", err)
			prof = DefaultProfile()
		} else {
			cctx := ctx
			if r.cfg.Classifier.Timeout > 0 {
				var cancel context.CancelFunc
				cctx, cancel = context.WithTimeout(ctx, r.cfg.Classifier.Timeout)
				defer cancel()
			}
			prof = r.classifyFn(cctx, prov, model, r.cfg.Classifier.MaxTokens, classificationTurn(chat, turn))
		}
	}

	// Request-derived cross-checks override the classifier's guesses: if the
	// actual request carries images or tools, those capabilities are required.
	if requestHasImages(chat) {
		prof.NeedsVision = true
	}
	if len(chat.Tools) > 0 {
		prof.NeedsTools = true
	}

	// Apply deterministic token floor: classifier context is deliberately
	// bounded, so it can underestimate large system prompts, histories, and
	// tool schemas. EstimateRequestTokens counts the full request; we take the max
	// so the classifier's semantic estimate is never overridden downward.
	detIn := EstimateRequestTokens(chat)
	if detIn > prof.EstTokensIn {
		prof.EstTokensIn = detIn
	}
	// Reserve capacity for the requested output (MaxTokens = 0 means "model
	// default"; treat it as a modest 1024 for context-filter purposes).
	detOut := chat.MaxTokens
	if detOut <= 0 {
		detOut = 1024
	}
	if detOut > prof.EstTokensOut {
		prof.EstTokensOut = detOut
	}

	d := Score(prof, r.cfg.Catalog, r.cfg.Weights)
	// If the profile benefits from reasoning, recommend medium effort. The
	// server applies it only to candidates that advertise reasoning support.
	if prof.NeedsReasoning {
		d.Reasoning = llm.ReasoningMedium
	}
	slog.Info("routing decision",
		"chosen", d.Chosen,
		"reason", d.Reason,
		"difficulty", d.Profile.Difficulty,
		"needs_reasoning", d.Profile.NeedsReasoning,
		"needs_vision", d.Profile.NeedsVision,
		"needs_tools", d.Profile.NeedsTools,
		"eligible", d.Eligible,
	)
	return d
}

const maxClassifierContextBytes = 12 << 10

// classificationTurn gives the classifier enough recent conversation to
// understand replies such as "continue" while bounding classifier cost. A
// genuinely single-turn request is passed through unchanged.
func classificationTurn(chat llm.ChatRequest, turn llm.Message) llm.Message {
	if chat.SystemPrompt == "" && len(chat.Messages) == 1 {
		return turn
	}

	chunks := make([]string, 0, len(chat.Messages)+1)
	if chat.SystemPrompt != "" {
		chunks = append(chunks, "system: "+chat.SystemPrompt)
	}
	for _, m := range chat.Messages {
		var b strings.Builder
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		if len(m.Images) > 0 {
			b.WriteString(" [image attached]")
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(" [tool call ")
			b.WriteString(tc.Name)
			b.WriteString(": ")
			b.Write(tc.Input)
			b.WriteByte(']')
		}
		if m.ToolCallID != "" {
			b.WriteString(" [tool result for ")
			b.WriteString(m.ToolCallID)
			b.WriteByte(']')
		}
		chunks = append(chunks, b.String())
	}

	// Retain whole recent turns where possible; only an individually oversized
	// latest turn is truncated, at a UTF-8 boundary.
	selected := make([]string, 0, len(chunks))
	used := 0
	for i := len(chunks) - 1; i >= 0; i-- {
		need := len(chunks[i])
		if len(selected) > 0 {
			need++
		}
		if used+need > maxClassifierContextBytes {
			if len(selected) == 0 {
				s := chunks[i]
				if len(s) > maxClassifierContextBytes {
					s = s[len(s)-maxClassifierContextBytes:]
					for !utf8.ValidString(s) {
						s = s[1:]
					}
				}
				selected = append(selected, s)
			}
			break
		}
		selected = append(selected, chunks[i])
		used += need
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return llm.Message{Role: "user", Content: strings.Join(selected, "\n")}
}

// requestHasImages reports whether any message carries image content.
func requestHasImages(chat llm.ChatRequest) bool {
	for _, m := range chat.Messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// SetClassifier overrides the classification function (test seam).
func (r *Router) SetClassifier(fn func(ctx context.Context, prov llm.LLMProvider, model string, maxTokens int, turn llm.Message) TaskProfile) {
	r.classifyFn = fn
}
