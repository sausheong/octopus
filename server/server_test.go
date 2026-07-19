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

var _ = io.Discard // keep io imported if unused above
