package router

import (
	"context"
	"log/slog"

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
			prof = r.classifyFn(cctx, prov, model, r.cfg.Classifier.MaxTokens, turn)
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

	d := Score(prof, r.cfg.Catalog, r.cfg.Weights, r.cfg.DefaultModel)
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
