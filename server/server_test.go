package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/sausheong/harness/llm"
	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
	"github.com/sausheong/llmrouter/router"
)

// anthErr builds a fully-populated *anthropic.Error so its Error() method (which
// dereferences Request and Response) does not panic when invoked by the handler.
func anthErr(status int) *anthropic.Error {
	return &anthropic.Error{
		StatusCode: status,
		Request:    &http.Request{Method: "POST", URL: &url.URL{Scheme: "https", Host: "api", Path: "/v1/messages"}},
		Response:   &http.Response{StatusCode: status},
	}
}

// fakeProv is a provider returning a scripted text stream.
type fakeProv struct{ text string }

func (f *fakeProv) Models() []llm.ModelInfo { return nil }
func (f *fakeProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (f *fakeProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 3)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: f.text}
	ch <- llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn", Usage: &llm.Usage{InputTokens: 3, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

// errProv is a provider whose ChatStream fails with a scripted error, used to
// exercise backend status-code mapping.
type errProv struct{ err error }

func (p *errProv) Models() []llm.ModelInfo { return nil }
func (p *errProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *errProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, p.err
}

// buildServerWithProv wires a Server whose registry + router use the supplied
// provider for the "anthropic" backend.
func buildServerWithProv(t *testing.T, prov llm.LLMProvider) *Server {
	t.Helper()
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "anthropic/haiku", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "anthropic/haiku",
		Providers:    map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog: []config.CatalogEntry{
			{ID: "anthropic/haiku", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": prov})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	return New(rt, reg, cfg.Catalog)
}

// buildServer wires a Server whose registry + router use the fake provider.
func buildServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "anthropic/haiku", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "anthropic/haiku",
		Providers:    map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog: []config.CatalogEntry{
			{ID: "anthropic/haiku", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
		},
	}
	// Inject the fake provider directly into a registry via the test helper.
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": &fakeProv{text: "Hello"}})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	return New(rt, reg, cfg.Catalog)
}

func TestPrepareAttemptOnlyEnablesSupportedReasoning(t *testing.T) {
	reg := registry.NewForTest(map[string]llm.LLMProvider{"p": &fakeProv{}})
	s := New(nil, reg, []config.CatalogEntry{
		{ID: "p/reasoning", Caps: config.Caps{Reasoning: true, MaxContext: 1000}},
		{ID: "p/ordinary", Caps: config.Caps{MaxContext: 1000}},
	})
	dec := router.Decision{Reasoning: llm.ReasoningMedium}
	_, capable, err := s.prepareAttempt("p/reasoning", llm.ChatRequest{}, dec)
	if err != nil || capable.Reasoning != llm.ReasoningMedium {
		t.Fatalf("capable attempt reasoning=%q err=%v", capable.Reasoning, err)
	}
	_, ordinary, err := s.prepareAttempt("p/ordinary", llm.ChatRequest{}, dec)
	if err != nil || ordinary.Reasoning != llm.ReasoningOff {
		t.Fatalf("ordinary attempt reasoning=%q err=%v", ordinary.Reasoning, err)
	}
}

func TestHandlerNonStreaming(t *testing.T) {
	s := buildServer(t)
	body := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"Hello"`) {
		t.Errorf("body missing text: %s", rec.Body.String())
	}
}

func TestHandlerStreaming(t *testing.T) {
	s := buildServer(t)
	body := `{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Errorf("malformed SSE: %s", out)
	}
}

func TestHandlerBadJSON(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{bad"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("missing error body: %s", rec.Body.String())
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerBackendRateLimit(t *testing.T) {
	s := buildServerWithProv(t, &errProv{err: anthErr(429)})
	body := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("missing error body: %s", rec.Body.String())
	}
}

func TestHandlerBackendOverloaded(t *testing.T) {
	s := buildServerWithProv(t, &errProv{err: anthErr(529)})
	body := `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("missing error body: %s", rec.Body.String())
	}
}

func TestHandlerStreamingUsage(t *testing.T) {
	// Reuse the streaming fake which sets Usage{InputTokens:3, OutputTokens:1}.
	s := buildServer(t)
	body := `{"model":"claude","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"input_tokens":3`) {
		t.Errorf("message_delta usage missing input_tokens: %s", out)
	}
}

func TestHandlerModels(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"list"`) {
		t.Errorf("missing list object: %s", body)
	}
	if !strings.Contains(body, `"anthropic/haiku"`) {
		t.Errorf("missing catalog model in response: %s", body)
	}
}

func TestHandlerModelsMethodNotAllowed(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerFallbackOnProviderError(t *testing.T) {
	// Two providers: "bad" always errors, "good" succeeds. Scorer will pick
	// "bad/model" first (higher quality score), then fall back to "good/model".
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "good/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "good/model",
		Providers: map[string]config.ProviderCreds{
			"bad":  {APIKeyEnv: "X"},
			"good": {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"bad":  &errProv{err: anthErr(529)},
		"good": &fakeProv{text: "fallback response"},
	})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback response") {
		t.Errorf("expected fallback response in body: %s", rec.Body.String())
	}
}

func TestHandlerAllProvidersFail(t *testing.T) {
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "bad/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "bad/model",
		Providers:    map[string]config.ProviderCreds{"bad": {APIKeyEnv: "X"}},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"bad": &errProv{err: anthErr(529)}})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected error status when all providers fail, got 200")
	}
}

// ---- /v1/chat/completions handler tests ------------------------------------

func TestOAIHandlerNonStreaming(t *testing.T) {
	s := buildServer(t)
	body := `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"object":"chat.completion"`) {
		t.Errorf("missing object field: %s", out)
	}
	if !strings.Contains(out, `"Hello"`) {
		t.Errorf("missing text content: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason: %s", out)
	}
}

func TestOAIHandlerStreaming(t *testing.T) {
	s := buildServer(t)
	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"chat.completion.chunk"`) {
		t.Errorf("missing chunk object: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", out)
	}
}

func TestOAIHandlerSystemMessage(t *testing.T) {
	s := buildServer(t)
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestOAIHandlerBadJSON(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{bad"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("missing error body: %s", rec.Body.String())
	}
}

func TestOAIHandlerMethodNotAllowed(t *testing.T) {
	s := buildServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestOAIHandlerBackendError(t *testing.T) {
	s := buildServerWithProv(t, &errProv{err: anthErr(429)})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected error status, got 200")
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("missing error body: %s", rec.Body.String())
	}
}

func TestOAIHandlerFallback(t *testing.T) {
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "good/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "good/model",
		Providers: map[string]config.ProviderCreds{
			"bad":  {APIKeyEnv: "X"},
			"good": {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"bad":  &errProv{err: anthErr(529)},
		"good": &fakeProv{text: "fallback ok"},
	})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback ok") {
		t.Errorf("expected fallback content: %s", rec.Body.String())
	}
}

// asyncErrProv returns a channel that immediately emits EventError, simulating
// a provider that signals failure via the stream rather than the error return.
type asyncErrProv struct{ err error }

func (p *asyncErrProv) Models() []llm.ModelInfo { return nil }
func (p *asyncErrProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *asyncErrProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 1)
	ch <- llm.ChatEvent{Type: llm.EventError, Error: p.err}
	close(ch)
	return ch, nil
}

func TestHandlerFallbackOnAsyncStreamError(t *testing.T) {
	// Primary provider signals failure via EventError on the channel (not via
	// the error return of ChatStream). tryProviders must detect this and fall
	// back to the next eligible provider.
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "good/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "good/model",
		Providers: map[string]config.ProviderCreds{
			"bad":  {APIKeyEnv: "X"},
			"good": {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"bad":  &asyncErrProv{err: anthErr(529)},
		"good": &fakeProv{text: "async fallback"},
	})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "async fallback") {
		t.Errorf("expected fallback content: %s", rec.Body.String())
	}
}

func TestHandlerOversizedBody(t *testing.T) {
	s := buildServer(t)
	// Send a body larger than maxRequestBytes (32 MiB).
	huge := strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 33<<20) + `"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", huge)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("expected error for oversized body, got 200")
	}
}

func TestOAIHandlerRateLimitMaps429(t *testing.T) {
	s := buildServerWithProv(t, &errProv{err: anthErr(429)})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("missing rate_limit_error: %s", rec.Body.String())
	}
}

func TestOAIHandlerOverloadedMaps503(t *testing.T) {
	s := buildServerWithProv(t, &errProv{err: anthErr(529)})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"overloaded_error"`) {
		t.Errorf("missing overloaded_error: %s", rec.Body.String())
	}
}

// midErrProv emits one text delta then an EventError, simulating a provider
// that fails mid-stream. For non-streaming requests this should trigger fallback
// (collection fails); for streaming the error arrives after headers are written.
type midErrProv struct {
	text string
	err  error
}

func (p *midErrProv) Models() []llm.ModelInfo { return nil }
func (p *midErrProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *midErrProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 2)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: p.text}
	ch <- llm.ChatEvent{Type: llm.EventError, Error: p.err}
	close(ch)
	return ch, nil
}

func TestHandlerNonStreamingFallbackOnCollectionError(t *testing.T) {
	// For buffered (non-streaming) requests, a mid-stream EventError must
	// trigger fallback to the next provider — collectWithFallback iterates
	// all candidates before giving up.
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "good/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "good/model",
		Providers: map[string]config.ProviderCreds{
			"bad":  {APIKeyEnv: "X"},
			"good": {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"bad":  &midErrProv{text: "partial", err: anthErr(529)},
		"good": &fakeProv{text: "collection fallback"},
	})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "collection fallback") {
		t.Errorf("expected fallback content: %s", rec.Body.String())
	}
}

// emptyProv returns a channel that closes immediately with no events.
type emptyProv struct{}

func (p *emptyProv) Models() []llm.ModelInfo { return nil }
func (p *emptyProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *emptyProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent)
	close(ch)
	return ch, nil
}

func TestHandlerNoEligibleReturns422(t *testing.T) {
	// A request whose token footprint exceeds all catalog models should get 422,
	// not a silent route to default_model.
	cfg := &config.Config{
		ServerAddr:   "x",
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "anthropic/haiku",
		Providers:    map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog: []config.CatalogEntry{
			{ID: "anthropic/haiku", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 100}}, // tiny context
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": &fakeProv{text: "should not reach"}})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10000, EstTokensOut: 10000}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	// Should be 400 (Anthropic endpoint maps invalid_request to 400).
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestOAIHandlerNoEligibleReturns422(t *testing.T) {
	cfg := &config.Config{
		ServerAddr:   "x",
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "anthropic/haiku",
		Providers:    map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog: []config.CatalogEntry{
			{ID: "anthropic/haiku", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 100}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": &fakeProv{text: "should not reach"}})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10000, EstTokensOut: 10000}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerFallbackOnEmptyBufferedStream(t *testing.T) {
	// Primary provider returns an empty channel (no events). collectWithFallback
	// must detect this and try the next candidate.
	cfg := &config.Config{
		ServerAddr:   "x",
		Classifier:   config.ClassifierCfg{Model: "good/model", MaxTokens: 16},
		Weights:      config.Weights{Quality: 1, Cost: 1, Speed: 1},
		DefaultModel: "good/model",
		Providers: map[string]config.ProviderCreds{
			"bad":  {APIKeyEnv: "X"},
			"good": {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "bad/model", Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.7, CostPerMTokIn: 1, CostPerMTokOut: 5, Speed: 0.9,
				Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"bad":  &emptyProv{},
		"good": &fakeProv{text: "empty stream fallback"},
	})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, cfg.Catalog)

	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty stream fallback") {
		t.Errorf("expected fallback content: %s", rec.Body.String())
	}
}

var _ = io.Discard // keep io imported if unused above
