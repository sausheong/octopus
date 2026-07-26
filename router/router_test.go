package router

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/registry"
)

func testCfg() *config.Config {
	os.Setenv("RT_ANTHROPIC", "sk-test")
	return &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "anthropic/haiku", MaxTokens: 256, Timeout: time.Second},
		Weights:      config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		Routing:      config.RoutingCfg{SessionSticky: true, SessionTTL: time.Hour, CacheAware: true},
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

func TestRouteKeepsStickySessionOnSuccessfulModel(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	chat := llm.ChatRequest{SessionID: "session-a", MaxTokens: 1, Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	r.Observe(chat, "anthropic/opus", &llm.Usage{})
	d := r.Route(context.Background(), chat)
	if d.Chosen != "anthropic/opus" || d.Reason != "sticky session affinity" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestRoutePricesKnownCacheReadBelowNewCacheWrite(t *testing.T) {
	cfg := testCfg()
	cfg.Routing.SessionSticky = false
	cfg.Weights = config.Weights{Cost: 1}
	cfg.Catalog = []config.CatalogEntry{
		{ID: "anthropic/cached", Quality: 0.5, CostPerMTokIn: 10, Speed: 0.5, Caps: config.Caps{MaxContext: 100000}},
		{ID: "anthropic/uncached", Quality: 0.5, CostPerMTokIn: 2, Speed: 0.5, Caps: config.Caps{MaxContext: 100000}},
	}
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	chat := llm.ChatRequest{
		SessionID:    "session-cache",
		MaxTokens:    1,
		CacheControl: &llm.CacheControl{Type: "ephemeral"},
		Messages:     []llm.Message{{Role: "user", Content: strings.Repeat("x", 300)}},
	}
	r.Observe(chat, "anthropic/cached", &llm.Usage{CacheCreationInputTokens: 100})
	d := r.Route(context.Background(), chat)
	if d.Chosen != "anthropic/cached" {
		t.Fatalf("chosen = %q, scores = %+v", d.Chosen, d.Scores)
	}
}

func TestSessionIDStableAcrossGrowingConversation(t *testing.T) {
	base := llm.ChatRequest{SystemPrompt: "system", Messages: []llm.Message{{Role: "user", Content: "first"}}}
	grown := base
	grown.Messages = append(append([]llm.Message(nil), base.Messages...),
		llm.Message{Role: "assistant", Content: "answer"}, llm.Message{Role: "user", Content: "next"})
	if SessionID(base) != SessionID(grown) {
		t.Fatalf("derived session changed as conversation grew")
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

func TestRouteClassifierReceivesRecentConversation(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	var classified string
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		classified = turn.Content
		return TaskProfile{Difficulty: "medium", EstTokensIn: 100, EstTokensOut: 100}
	}
	chat := llm.ChatRequest{Messages: []llm.Message{
		{Role: "user", Content: "Refactor the complex parser without changing its API."},
		{Role: "assistant", Content: "I have a plan."},
		{Role: "user", Content: "continue"},
	}}
	r.Route(context.Background(), chat)
	for _, want := range []string{"complex parser", "I have a plan", "continue"} {
		if !strings.Contains(classified, want) {
			t.Errorf("classifier context missing %q: %q", want, classified)
		}
	}
}

func TestRouteClassifierContextKeepsBeginningAndLatestAfterLongTurn(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	var classified string
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		classified = turn.Content
		return TaskProfile{Difficulty: "medium", EstTokensIn: 100, EstTokensOut: 100}
	}
	chat := llm.ChatRequest{
		SystemPrompt: "Preserve the parser API.",
		Messages: []llm.Message{
			{Role: "user", Content: "Refactor the original parser task."},
			{Role: "assistant", Content: strings.Repeat("long implementation detail ", 1000)},
			{Role: "user", Content: "continue with the original task"},
		},
	}
	r.Route(context.Background(), chat)
	for _, want := range []string{"Preserve the parser API", "original parser task", "continue with the original task"} {
		if !strings.Contains(classified, want) {
			t.Errorf("bounded classifier context missing %q", want)
		}
	}
	if len(classified) > maxClassifierContextBytes {
		t.Errorf("classifier context has %d bytes, limit is %d", len(classified), maxClassifierContextBytes)
	}
}

func TestRouteClassifierBoundsLongSingleTurn(t *testing.T) {
	cfg := testCfg()
	reg, _ := registry.New(context.Background(), cfg)
	r := NewRouter(cfg, reg)
	var classified llm.Message
	r.classifyFn = func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) TaskProfile {
		classified = turn
		return TaskProfile{Difficulty: "medium", EstTokensIn: 100, EstTokensOut: 100}
	}
	content := "important instruction at start\n" + strings.Repeat("payload ", 3000) + "\nlatest constraint at end"
	image := llm.ImageContent{MimeType: "image/png", Data: []byte("x")}
	r.Route(context.Background(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: content, Images: []llm.ImageContent{image}}}})
	if len(classified.Content) > maxClassifierContextBytes || !strings.Contains(classified.Content, "important instruction") || !strings.Contains(classified.Content, "latest constraint") {
		t.Fatalf("bounded classifier content is incorrect: len=%d", len(classified.Content))
	}
	if len(classified.Images) != 1 {
		t.Fatal("latest-turn images were not preserved for classification")
	}
}

func TestRouteClassifierFreeLocalModelHandlesNonTrivialRequest(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "x",
		Weights:    config.Weights{Quality: 0.5, Cost: 0.4, Speed: 0.1},
		Providers: map[string]config.ProviderCreds{
			"local": {Kind: "openai", BaseURL: "http://127.0.0.1:8080/v1"},
		},
		Catalog: []config.CatalogEntry{{
			ID: "local/model", Quality: 0.6, Speed: 0.8,
			Caps: config.Caps{MaxContext: 32768},
		}},
	}
	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	d := NewRouter(cfg, reg).Route(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: strings.Repeat("build this feature ", 40)}},
	})
	if d.NoEligible || d.Chosen != "local/model" {
		t.Fatalf("decision = %+v, want local/model", d)
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

func TestRouteCarriesMaxAttempts(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "x",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{MaxAttempts: 2},
		Catalog: []config.CatalogEntry{
			{ID: "p/m", Quality: 0.9, Speed: 0.5, Caps: config.Caps{MaxContext: 100000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"p": &fakeProvider{}})
	rt := NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, _ llm.LLMProvider, _ string, _ int, _ llm.Message) TaskProfile {
		return TrivialProfile()
	})
	dec := rt.Route(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if dec.MaxAttempts != 2 {
		t.Errorf("Decision.MaxAttempts = %d, want 2", dec.MaxAttempts)
	}
}

func TestEstimateRequestTokensCountsSystemPromptParts(t *testing.T) {
	big := strings.Repeat("x", 60000)

	partsOnly := EstimateRequestTokens(llm.ChatRequest{
		SystemPromptParts: []llm.SystemPromptPart{{Text: big}},
		Messages:          []llm.Message{{Role: "user", Content: "hi"}},
	})
	if partsOnly < 15000 {
		t.Errorf("parts-only estimate = %d, want >= 15000 (60KB / 3 bytes per token)", partsOnly)
	}

	// Decode sets both fields for a structured prompt. Parts replace
	// SystemPrompt in harness semantics, so the bytes must be counted once.
	both := EstimateRequestTokens(llm.ChatRequest{
		SystemPrompt:      big,
		SystemPromptParts: []llm.SystemPromptPart{{Text: big}},
		Messages:          []llm.Message{{Role: "user", Content: "hi"}},
	})
	if both != partsOnly {
		t.Errorf("double counted: both = %d, parts-only = %d", both, partsOnly)
	}

	promptOnly := EstimateRequestTokens(llm.ChatRequest{
		SystemPrompt: big,
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	})
	if promptOnly != partsOnly {
		t.Errorf("prompt-only = %d, parts-only = %d; equivalent input should estimate equally", promptOnly, partsOnly)
	}
}
