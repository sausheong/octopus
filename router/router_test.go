package router

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
)

func testCfg() *config.Config {
	os.Setenv("RT_ANTHROPIC", "sk-test")
	return &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "anthropic/haiku", MaxTokens: 256, Timeout: time.Second},
		Weights:      config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		DefaultModel: "anthropic/haiku",
		Providers:    map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "RT_ANTHROPIC"}},
		Catalog: []config.CatalogEntry{
			{ID: "anthropic/opus", Quality: 0.98, CostPerMTokIn: 15, CostPerMTokOut: 75, Speed: 0.4,
				Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 1000000}},
			{ID: "anthropic/haiku", Quality: 0.70, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.95,
				Caps: config.Caps{Tools: true, Vision: true, Reasoning: false, MaxContext: 200000}},
		},
	}
}

// stubRouter swaps the classifier provider with a fake by constructing the
// Router with a registry whose anthropic provider is the fake. We do this by
// injecting via the exported classifyFn seam.
func TestRouteCrossChecksVision(t *testing.T) {
	cfg := testCfg()
	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r := NewRouter(cfg, reg)
	// Force classifier to a trivial profile via the seam; request has an image
	// so cross-check must set NeedsVision=true (both catalog models have vision,
	// so this mainly asserts the profile is mutated).
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		return TaskProfile{Difficulty: "low", EstTokensIn: 100, EstTokensOut: 100, Domain: "qa"}
	}
	chat := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "look", Images: []llm.ImageContent{{MimeType: "image/png", Data: []byte("x")}}}},
	}
	d := r.Route(context.Background(), chat)
	if !d.Profile.NeedsVision {
		t.Error("expected NeedsVision forced true by cross-check")
	}
	if d.Chosen == "" {
		t.Error("expected a chosen model")
	}
}

func TestRouteCrossChecksTools(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		return TaskProfile{Difficulty: "low", EstTokensIn: 100, EstTokensOut: 100}
	}
	chat := llm.ChatRequest{
		Tools:    []llm.ToolDef{{Name: "get"}},
		Messages: []llm.Message{{Role: "user", Content: "use a tool"}},
	}
	d := r.Route(context.Background(), chat)
	if !d.Profile.NeedsTools {
		t.Error("expected NeedsTools forced true by cross-check")
	}
}

func TestRouteShortCircuitSkipsClassifier(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{}
	}
	chat := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}}, // short, no images, no tools
	}
	d := r.Route(context.Background(), chat)
	if called {
		t.Error("classifier should not be called for a trivial request")
	}
	if d.Profile.Difficulty != "trivial" {
		t.Errorf("Difficulty = %q, want trivial", d.Profile.Difficulty)
	}
}

func TestRouteShortCircuitNotFiredWithTools(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{Difficulty: "low"}
	}
	chat := llm.ChatRequest{
		Tools:    []llm.ToolDef{{Name: "search"}},
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}
	r.Route(context.Background(), chat)
	if !called {
		t.Error("classifier should be called when request has tools")
	}
}

func TestRouteShortCircuitNotFiredWithImages(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{Difficulty: "low"}
	}
	chat := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "look", Images: []llm.ImageContent{{MimeType: "image/png", Data: []byte("x")}}}},
	}
	r.Route(context.Background(), chat)
	if !called {
		t.Error("classifier should be called when turn has images")
	}
}

func TestRouteShortCircuitNotFiredForLongContent(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{Difficulty: "medium"}
	}
	longContent := string(make([]byte, shortCircuitBytes+1))
	chat := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: longContent}},
	}
	r.Route(context.Background(), chat)
	if !called {
		t.Error("classifier should be called for long content")
	}
}

func TestRouteShortCircuitNotFiredForMultiTurn(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{Difficulty: "low"}
	}
	chat := llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "user", Content: "do you want me to proceed?"},
			{Role: "assistant", Content: "Yes, please go ahead."},
			{Role: "user", Content: "yes"},
		},
	}
	r.Route(context.Background(), chat)
	if !called {
		t.Error("classifier should be called for multi-turn conversation")
	}
}

func TestRouteDeterministicFloorFloorsClassifierEstimate(t *testing.T) {
	// If the classifier underestimates tokens, the deterministic floor should
	// raise EstTokensIn to at least what EstimateRequestTokens returns.
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	// Classifier returns a tiny estimate regardless of actual content.
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		return TaskProfile{Difficulty: "low", EstTokensIn: 1, EstTokensOut: 1}
	}
	longSystem := strings.Repeat("x", 3000) // ~1000 tokens at 3 bytes/token
	chat := llm.ChatRequest{
		SystemPrompt: longSystem,
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}
	d := r.Route(context.Background(), chat)
	det := EstimateRequestTokens(chat)
	if d.Profile.EstTokensIn < det {
		t.Errorf("EstTokensIn = %d, want >= deterministic estimate %d", d.Profile.EstTokensIn, det)
	}
}

func TestRouteDeterministicFloorDoesNotLower(t *testing.T) {
	// If the classifier produces a higher estimate than the deterministic floor,
	// the classifier's estimate should be preserved. Use long content to bypass
	// the trivial short-circuit so the classifier actually runs.
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		return TaskProfile{Difficulty: "low", EstTokensIn: 999999, EstTokensOut: 999999}
	}
	chat := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", shortCircuitBytes+1)}},
	}
	d := r.Route(context.Background(), chat)
	if d.Profile.EstTokensIn < 999999 {
		t.Errorf("EstTokensIn = %d, classifier's higher estimate should be preserved", d.Profile.EstTokensIn)
	}
}

func TestRouteReasoningSetOnDecision(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		return TaskProfile{Difficulty: "high", NeedsReasoning: true, EstTokensIn: 100, EstTokensOut: 100}
	}
	// Use content long enough to bypass the trivial short-circuit.
	chat := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", shortCircuitBytes+1)}}}
	d := r.Route(context.Background(), chat)
	if d.Reasoning == llm.ReasoningOff {
		t.Error("Decision.Reasoning should be set when NeedsReasoning=true")
	}
}

func TestRouteNoUserTurnUsesDefaultProfile(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	called := false
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		called = true
		return TaskProfile{}
	}
	chat := llm.ChatRequest{Messages: []llm.Message{{Role: "assistant", Content: "hi"}}}
	d := r.Route(context.Background(), chat)
	if called {
		t.Error("classifier should not be called when there is no user turn")
	}
	// DefaultProfile is high-difficulty + reasoning → opus.
	if d.Chosen != "anthropic/opus" {
		t.Errorf("Chosen = %q, want anthropic/opus", d.Chosen)
	}
}
