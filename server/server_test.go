package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
	"github.com/sausheong/llmrouter/router"
)

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
	return New(rt, reg)
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

var _ = io.Discard // keep io imported if unused above
