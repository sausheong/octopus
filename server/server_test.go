package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sausheong/harness/llm"
	"github.com/sausheong/octopus/anthropicio"
	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/insights"
	"github.com/sausheong/octopus/registry"
	"github.com/sausheong/octopus/router"
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

// transportAwareProv exposes both Harness transports so tests can verify that
// Octopus honours the client's stream preference instead of forcing every
// upstream request through SSE.
type transportAwareProv struct {
	streamCalls       int
	nonStreamingCalls int
	streamText        string
	nonStreamingText  string
	nonStreamingErr   error
}

func (p *transportAwareProv) Models() []llm.ModelInfo { return nil }
func (p *transportAwareProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *transportAwareProv) ChatStream(context.Context, llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.streamCalls++
	return completedEvents(p.streamText), nil
}
func (p *transportAwareProv) ChatNonStreaming(context.Context, llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.nonStreamingCalls++
	if p.nonStreamingErr != nil {
		return nil, p.nonStreamingErr
	}
	return completedEvents(p.nonStreamingText), nil
}

func completedEvents(text string) <-chan llm.ChatEvent {
	ch := make(chan llm.ChatEvent, 2)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: text}
	ch <- llm.ChatEvent{Type: llm.EventDone, StopReason: "end_turn", Usage: &llm.Usage{InputTokens: 3, OutputTokens: 1}}
	close(ch)
	return ch
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

// partialProv emits content but violates the stream contract by closing before
// EventDone. Buffered handlers must reject it and try the next candidate.
type partialProv struct{}

func (p *partialProv) Models() []llm.ModelInfo { return nil }
func (p *partialProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *partialProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent, 1)
	ch <- llm.ChatEvent{Type: llm.EventTextDelta, Text: "truncated"}
	close(ch)
	return ch, nil
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

func TestCollectWithFallbackPrefersNativeNonStreamingTransport(t *testing.T) {
	prov := &transportAwareProv{streamText: "streamed", nonStreamingText: "buffered"}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"native": prov})
	s := New(nil, reg, nil)
	dec := router.Decision{Chosen: "native/model", Eligible: []string{"native/model"}}

	out, chosen, err := s.collectWithFallback(context.Background(), dec, llm.ChatRequest{}, anthropicio.CollectMessage)
	if err != nil {
		t.Fatalf("collectWithFallback: %v", err)
	}
	if chosen != "native/model" || !strings.Contains(string(out), "buffered") {
		t.Fatalf("chosen=%q body=%s", chosen, out)
	}
	if prov.nonStreamingCalls != 1 || prov.streamCalls != 0 {
		t.Fatalf("non-streaming calls=%d stream calls=%d, want 1 and 0", prov.nonStreamingCalls, prov.streamCalls)
	}
}

func TestCollectWithFallbackRetainsFallbackAfterNativeNonStreamingFailure(t *testing.T) {
	primary := &transportAwareProv{nonStreamingErr: anthErr(529)}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"primary":  primary,
		"fallback": &fakeProv{text: "fallback response"},
	})
	s := New(nil, reg, nil)
	dec := router.Decision{
		Chosen:   "primary/model",
		Eligible: []string{"primary/model", "fallback/model"},
	}

	out, chosen, err := s.collectWithFallback(context.Background(), dec, llm.ChatRequest{}, anthropicio.CollectMessage)
	if err != nil {
		t.Fatalf("collectWithFallback: %v", err)
	}
	if chosen != "fallback/model" || !strings.Contains(string(out), "fallback response") {
		t.Fatalf("chosen=%q body=%s", chosen, out)
	}
	if primary.nonStreamingCalls != 1 || primary.streamCalls != 0 {
		t.Fatalf("primary non-streaming calls=%d stream calls=%d, want 1 and 0", primary.nonStreamingCalls, primary.streamCalls)
	}
}

func TestTryProvidersStreamContinuesUsingStreamingTransport(t *testing.T) {
	prov := &transportAwareProv{streamText: "streamed", nonStreamingText: "buffered"}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"native": prov})
	s := New(nil, reg, nil)
	dec := router.Decision{Chosen: "native/model", Eligible: []string{"native/model"}}

	ch, chosen, err := s.tryProvidersStream(context.Background(), dec, llm.ChatRequest{})
	if err != nil {
		t.Fatalf("tryProvidersStream: %v", err)
	}
	for range ch {
	}
	if chosen != "native/model" {
		t.Fatalf("chosen=%q", chosen)
	}
	if prov.streamCalls != 1 || prov.nonStreamingCalls != 0 {
		t.Fatalf("stream calls=%d non-streaming calls=%d, want 1 and 0", prov.streamCalls, prov.nonStreamingCalls)
	}
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

func TestObserveEventsReportsCompletedUsageToInsights(t *testing.T) {
	var observed insights.Observation
	s := New(nil, nil, []config.CatalogEntry{{ID: "local/model"}}, func(value insights.Observation) {
		observed = value
	})
	usage := &llm.Usage{InputTokens: 12, OutputTokens: 4}
	in := make(chan llm.ChatEvent, 1)
	in <- llm.ChatEvent{Type: llm.EventDone, Usage: usage}
	close(in)
	for range s.observeEvents(llm.ChatRequest{}, "local/model", router.Decision{Chosen: "local/model"}, in) {
	}
	if observed.Model != "local/model" || observed.Usage != usage || len(observed.Catalog) != 1 {
		t.Fatalf("observation = %+v", observed)
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

func TestHealthAndReadinessProbes(t *testing.T) {
	s := buildServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("GET %s Cache-Control=%q", path, got)
		}
	}
}

func TestReadinessDoesNotExposeDetailsAndDoesNotRequireRouterAuth(t *testing.T) {
	s := buildServer(t)
	s.SetAuthToken("secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "anthropic") {
		t.Fatalf("readiness status=%d body=%s", rec.Code, rec.Body.String())
	}

	notReady := New(nil, nil, nil)
	rec = httptest.NewRecorder()
	notReady.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoutingOverridesRequireConfiguredAuthentication(t *testing.T) {
	open := buildServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set(minQualityHeader, "0.95")
	req.Header.Set(fixedModelHeader, "anthropic/opus")
	meta, err := open.requestMetadata(req, "/v1/messages", true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.MinQuality != 0 || meta.FixedModel != "" || meta.HighestQuality {
		t.Fatalf("open endpoint trusted policy overrides: %+v", meta)
	}

	authed := buildServer(t)
	authed.SetAuthToken("secret")
	req.Header.Set("x-api-key", "secret")
	meta, err = authed.requestMetadata(req, "/v1/messages", true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.MinQuality != 0.95 || meta.FixedModel != "anthropic/opus" {
		t.Fatalf("authenticated overrides not parsed: %+v", meta)
	}
}

func TestRoutingOverridesRejectInvalidValues(t *testing.T) {
	s := buildServer(t)
	s.SetAuthToken("secret")
	for _, tc := range []struct {
		header string
		value  string
	}{
		{minQualityHeader, "1.1"},
		{minQualityHeader, "NaN"},
		{highestQualityHeader, "sometimes"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set(tc.header, tc.value)
		if _, err := s.requestMetadata(req, "/v1/messages", false); err == nil {
			t.Errorf("%s=%q accepted", tc.header, tc.value)
		}
	}
}

func TestInvalidAuthenticatedOverrideReturnsClientErrorOnBothAPIs(t *testing.T) {
	s := buildServer(t)
	s.SetAuthToken("secret")
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/v1/messages", `{"model":"x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("x-api-key", "secret")
		req.Header.Set(minQualityHeader, "expensive")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
	}
}

type stalledProv struct{}

func (*stalledProv) Models() []llm.ModelInfo { return nil }
func (*stalledProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (*stalledProv) ChatStream(context.Context, llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return make(chan llm.ChatEvent), nil
}

type cancelAwareStalledProv struct{ canceled chan struct{} }

func (*cancelAwareStalledProv) Models() []llm.ModelInfo { return nil }
func (*cancelAwareStalledProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *cancelAwareStalledProv) ChatStream(ctx context.Context, _ llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	ch := make(chan llm.ChatEvent)
	go func() {
		<-ctx.Done()
		close(p.canceled)
		close(ch)
	}()
	return ch, nil
}

func TestFirstEventTimeoutFallsBackBeforeWritingClientBytes(t *testing.T) {
	stalled := &cancelAwareStalledProv{canceled: make(chan struct{})}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"stalled": stalled,
		"good":    &fakeProv{text: "bounded fallback"},
	})
	s := New(nil, reg, nil)
	s.SetFirstEventTimeout(20 * time.Millisecond)
	dec := router.Decision{Chosen: "stalled/model", Eligible: []string{"stalled/model", "good/model"}}
	started := time.Now()
	ch, chosen, err := s.tryProvidersStream(context.Background(), dec, llm.ChatRequest{})
	if err != nil {
		t.Fatalf("tryProvidersStream: %v", err)
	}
	if chosen != "good/model" {
		t.Fatalf("chosen=%q", chosen)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("fallback took %s", elapsed)
	}
	for range ch {
	}
	select {
	case <-stalled.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed-out provider context was not cancelled")
	}
}

func TestFixedModelOverrideDisablesFallbackCandidates(t *testing.T) {
	dec := router.Decision{
		Chosen: "provider/fixed", Eligible: []string{"provider/fixed", "provider/other"}, PolicyOverride: "fixed_model",
	}
	got := candidates(dec)
	if len(got) != 1 || got[0] != "provider/fixed" {
		t.Fatalf("fixed-model candidates = %v", got)
	}
}

func TestNoEligibleMessageExplainsStrictQualityFloor(t *testing.T) {
	got := noEligibleMessage(router.Decision{Reason: "no eligible model meets quality floor", QualityPolicy: "strict", AppliedQualityFloor: .95})
	if !strings.Contains(got, "quality floor 0.95") {
		t.Fatalf("message = %q", got)
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

func TestHandlerFallbackOnPrematureProviderClosure(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 1},
		Providers: map[string]config.ProviderCreds{
			"partial": {APIKeyEnv: "X"},
			"good":    {APIKeyEnv: "X"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "partial/model", Quality: 0.9, Caps: config.Caps{MaxContext: 200000}},
			{ID: "good/model", Quality: 0.86, Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{
		"partial": &partialProv{},
		"good":    &fakeProv{text: "complete fallback"},
	})
	s := New(router.NewRouter(cfg, reg), reg, cfg.Catalog)
	body := `{"model":"any","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "complete fallback") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
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

func TestLocalOnlyProviderFailureNeverFallsBackToCloud(t *testing.T) {
	local := &countingErrProv{err: anthErr(429)}
	cloud := &countingErrProv{err: anthErr(429)}
	cfg := &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Classifier: config.ClassifierCfg{Model: "cloud/classifier", MaxTokens: 16},
		Weights:    config.Weights{Quality: 1},
		Routing: config.RoutingCfg{
			Strategy: config.RoutingStrategyPerTurn, DataPolicy: config.DataPolicyLocalOnly, MaxAttempts: 3,
		},
		Providers: map[string]config.ProviderCreds{
			"local": {Kind: "openai", Location: config.ProviderLocationLocal, BaseURL: "http://127.0.0.1:11434/v1"},
			"cloud": {Kind: "anthropic", Location: config.ProviderLocationRemote, APIKey: "test"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "local/model", Quality: 0.9, Speed: 0.5, Caps: config.Caps{MaxContext: 200000}},
			{ID: "cloud/model", Quality: 0.99, Speed: 0.9, Caps: config.Caps{MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"local": local, "cloud": cloud})
	s := New(router.NewRouter(cfg, reg), reg, cfg.Catalog)

	body := `{"model":"any","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if local.calls != 1 {
		t.Fatalf("local calls = %d, want 1", local.calls)
	}
	if cloud.calls != 0 {
		t.Fatalf("cloud calls = %d, want 0 under local_only", cloud.calls)
	}
}

func TestLocalOnlyNoEligibleErrorExplainsFailClosedPolicy(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 1},
		Routing: config.RoutingCfg{
			Strategy: config.RoutingStrategyPerTurn, DataPolicy: config.DataPolicyLocalOnly,
		},
		Providers: map[string]config.ProviderCreds{
			"local": {Kind: "openai", Location: config.ProviderLocationLocal, BaseURL: "http://127.0.0.1:11434/v1"},
		},
		Catalog: []config.CatalogEntry{
			{ID: "local/model", Quality: 0.8, Speed: 0.5, Caps: config.Caps{Tools: false, MaxContext: 200000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"local": &fakeProv{text: "unused"}})
	s := New(router.NewRouter(cfg, reg), reg, cfg.Catalog)
	body := `{"model":"any","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"use lookup"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "remote routing is disabled") {
		t.Fatalf("fail-closed explanation missing: %s", rec.Body.String())
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

func TestSessionIDHeaderUsesOctopusNameAndSupportsLegacyAlias(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-LLMRouter-Session-ID", "legacy")
	if got := sessionIDHeader(req); got != "legacy" {
		t.Fatalf("legacy session header = %q", got)
	}

	req.Header.Set("X-Octopus-Session-ID", "octopus")
	if got := sessionIDHeader(req); got != "octopus" {
		t.Fatalf("Octopus session header = %q, want preferred new name", got)
	}
}

var _ = io.Discard // keep io imported if unused above

// countingErrProv records how many times ChatStream was called and always
// fails with a scripted error.
type countingErrProv struct {
	calls int
	err   error
}

func (p *countingErrProv) Models() []llm.ModelInfo { return nil }
func (p *countingErrProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *countingErrProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.calls++
	return nil, p.err
}

// buildFanoutServer wires a Server over an eight-model catalog served by one
// counting provider, so a test can assert exactly how many backends were tried.
func buildFanoutServer(t *testing.T, prov llm.LLMProvider, maxAttempts int) *Server {
	t.Helper()
	var catalog []config.CatalogEntry
	for _, name := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8"} {
		catalog = append(catalog, config.CatalogEntry{
			ID: "anthropic/" + name, Quality: 0.9, CostPerMTokIn: 1, CostPerMTokOut: 1, Speed: 0.5,
			Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000},
		})
	}
	cfg := &config.Config{
		ServerAddr: "x",
		Classifier: config.ClassifierCfg{Model: "anthropic/m1", MaxTokens: 16},
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{MaxAttempts: maxAttempts},
		Providers:  map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog:    catalog,
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": prov})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	return New(rt, reg, cfg.Catalog)
}

const fanoutBody = `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
const fanoutStreamBody = `{"model":"x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestPermanent400StopsAfterOneAttempt(t *testing.T) {
	prov := &countingErrProv{err: anthErr(400)}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if prov.calls != 1 {
		t.Errorf("ChatStream calls = %d, want 1 (400 is not retryable)", prov.calls)
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 (real backend error, not a masked 502)", rec.Code)
	}
}

func TestRateLimitStopsAtAttemptCap(t *testing.T) {
	prov := &countingErrProv{err: anthErr(429)}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if prov.calls != 3 {
		t.Errorf("ChatStream calls = %d, want 3 (cap), catalog has 8", prov.calls)
	}
	if rec.Code != 429 {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

func TestMaxAttemptsOneDisablesFallback(t *testing.T) {
	prov := &countingErrProv{err: anthErr(429)}
	s := buildFanoutServer(t, prov, 1)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if prov.calls != 1 {
		t.Errorf("ChatStream calls = %d, want 1", prov.calls)
	}
}

func TestCancelledRequestStopsAndWritesNothing(t *testing.T) {
	prov := &countingErrProv{err: context.Canceled}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if prov.calls != 1 {
		t.Errorf("ChatStream calls = %d, want 1 (client is gone)", prov.calls)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes to a disconnected client, want 0", rec.Body.Len())
	}
}

func TestCancelledStreamingRequestStopsAndWritesNothing(t *testing.T) {
	prov := &countingErrProv{err: context.Canceled}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutStreamBody)))
	if prov.calls != 1 {
		t.Errorf("ChatStream calls = %d, want 1 (client is gone)", prov.calls)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes to a disconnected client, want 0", rec.Body.Len())
	}
}

// The OpenAI endpoint must hang up as silently as the Anthropic one; before
// this fix its default branch turned a client disconnect into a 502.
func TestOpenAICancelledRequestStopsAndWritesNothing(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"buffered", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`},
		{"streaming", `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &countingErrProv{err: context.Canceled}
			s := buildFanoutServer(t, prov, 3)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body)))
			if prov.calls != 1 {
				t.Errorf("ChatStream calls = %d, want 1 (client is gone)", prov.calls)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("wrote %d bytes to a disconnected client, want 0", rec.Body.Len())
			}
		})
	}
}

// A hang-up is not a server fault, so it must not raise a warning: operators
// page on those, and the pre-fix loop emitted one per catalog entry.
func TestCancellationIsNotLoggedAsAWarning(t *testing.T) {
	var buf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := buildFanoutServer(t, &countingErrProv{err: context.Canceled}, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if got := buf.String(); got != "" {
		t.Errorf("cancellation emitted warn-level logs:\n%s", got)
	}

	// Guard against the test passing because nothing is logged at all: a real
	// backend failure must still warn.
	buf.Reset()
	s = buildFanoutServer(t, &countingErrProv{err: anthErr(429)}, 3)
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if !strings.Contains(buf.String(), "trying fallback") {
		t.Errorf("a genuine backend failure should still warn, got:\n%s", buf.String())
	}
}

// A Decision built before routing.max_attempts existed carries 0, which must
// mean "use the default" rather than "make no attempts at all".
func TestAttemptCapTreatsZeroAsDefault(t *testing.T) {
	if got := attemptCap(router.Decision{}); got != 3 {
		t.Errorf("attemptCap(zero Decision) = %d, want default 3", got)
	}
	if got := attemptCap(router.Decision{MaxAttempts: 5}); got != 5 {
		t.Errorf("attemptCap(5) = %d, want 5", got)
	}
}

func TestStreamingPathRespectsAttemptCap(t *testing.T) {
	prov := &countingErrProv{err: anthErr(429)}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutStreamBody)))
	if prov.calls != 3 {
		t.Errorf("streaming ChatStream calls = %d, want 3", prov.calls)
	}
}

func TestOpenAIPathRespectsAttemptCap(t *testing.T) {
	prov := &countingErrProv{err: anthErr(429)}
	s := buildFanoutServer(t, prov, 3)
	rec := httptest.NewRecorder()
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if prov.calls != 3 {
		t.Errorf("openai ChatStream calls = %d, want 3", prov.calls)
	}
	if rec.Code != 429 {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

// The 400 arm of oaiBackendError is the path a local mlx/Ollama/LM Studio
// rejection takes: the backend's own complaint has to reach the caller intact
// instead of being retried across the catalog and masked as a 502.
func TestOpenAIInvalidRequestStopsAfterOneAttempt(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"buffered", `{"model":"x","messages":[{"role":"user","content":"hi"}]}`},
		{"streaming", `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &countingErrProv{err: &openai.APIError{HTTPStatusCode: 400, Message: "bad max_tokens"}}
			s := buildFanoutServer(t, prov, 3)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body)))
			if prov.calls != 1 {
				t.Errorf("ChatStream calls = %d, want 1 (400 is not retryable)", prov.calls)
			}
			if rec.Code != 400 {
				t.Errorf("status = %d, want 400 (real backend error, not a masked 502)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "bad max_tokens") {
				t.Errorf("backend message lost from response body: %s", rec.Body.String())
			}
		})
	}
}

// countingScriptProv counts calls and returns a scripted event sequence, so a
// test can bound the fallback sites that fail via the channel rather than via
// the ChatStream error return.
type countingScriptProv struct {
	calls  int
	events []llm.ChatEvent
}

func (p *countingScriptProv) Models() []llm.ModelInfo { return nil }
func (p *countingScriptProv) NormalizeToolSchema(t []llm.ToolDef) ([]llm.ToolDef, []llm.Diagnostic) {
	return t, nil
}
func (p *countingScriptProv) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	p.calls++
	ch := make(chan llm.ChatEvent, len(p.events))
	for _, ev := range p.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// Each fallback loop has three backend-failure exits, and only the ChatStream
// error return is covered above. These pin the channel-side exits: a stream
// that closes empty, one whose first event is an error, and one that fails
// only part-way through collection.
func TestChannelSideFailuresRespectAttemptBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		events    []llm.ChatEvent
		wantCalls int
	}{
		{"streaming empty stream stops at cap", fanoutStreamBody, nil, 3},
		{"streaming first-event error is retryable to the cap", fanoutStreamBody,
			[]llm.ChatEvent{{Type: llm.EventError, Error: anthErr(429)}}, 3},
		{"streaming first-event 400 stops immediately", fanoutStreamBody,
			[]llm.ChatEvent{{Type: llm.EventError, Error: anthErr(400)}}, 1},
		{"buffered peek failure stops at cap", fanoutBody, nil, 3},
		// First event is the error, so this stops at the peekForContent break
		// site rather than the collect one the two cases below reach.
		{"buffered peek 400 stops immediately", fanoutBody,
			[]llm.ChatEvent{{Type: llm.EventError, Error: anthErr(400)}}, 1},
		{"buffered collection error stops at cap", fanoutBody,
			[]llm.ChatEvent{{Type: llm.EventTextDelta, Text: "partial"}, {Type: llm.EventError, Error: anthErr(429)}}, 3},
		{"buffered collection 400 stops immediately", fanoutBody,
			[]llm.ChatEvent{{Type: llm.EventTextDelta, Text: "partial"}, {Type: llm.EventError, Error: anthErr(400)}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &countingScriptProv{events: tc.events}
			s := buildFanoutServer(t, prov, 3)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(tc.body)))
			if prov.calls != tc.wantCalls {
				t.Errorf("ChatStream calls = %d, want %d (catalog has 8)", prov.calls, tc.wantCalls)
			}
		})
	}
}

// A resolve failure must not merely fall through — it must leave the whole
// attempt budget available to the backends that follow it.
func TestUnresolvableModelLeavesFullAttemptBudget(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "missing/m1", Quality: 0.99, Speed: 0.5, Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
		{ID: "anthropic/m2", Quality: 0.9, Speed: 0.5, Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
		{ID: "anthropic/m3", Quality: 0.8, Speed: 0.5, Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
	}
	prov := &countingErrProv{err: anthErr(429)}
	cfg := &config.Config{
		ServerAddr: "x",
		Classifier: config.ClassifierCfg{Model: "anthropic/m2", MaxTokens: 16},
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{MaxAttempts: 2},
		Providers:  map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog:    catalog,
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": prov})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, catalog)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if prov.calls != 2 {
		t.Errorf("ChatStream calls = %d, want 2 (the unresolvable entry must not eat an attempt)", prov.calls)
	}
}

// A resolve failure means a catalog entry names an unconfigured provider. That
// is a config error, not a backend failure, so it must not consume an attempt.
func TestUnresolvableModelDoesNotConsumeAttempt(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "missing/m1", Quality: 0.95, Speed: 0.5, Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
		{ID: "anthropic/m2", Quality: 0.9, Speed: 0.5, Caps: config.Caps{Tools: true, Vision: true, MaxContext: 200000}},
	}
	cfg := &config.Config{
		ServerAddr: "x",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{MaxAttempts: 1},
		Providers:  map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "X"}},
		Catalog:    catalog,
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"anthropic": &fakeProv{text: "ok"}})
	rt := router.NewRouter(cfg, reg)
	rt.SetClassifier(func(ctx context.Context, p llm.LLMProvider, model string, mt int, turn llm.Message) router.TaskProfile {
		return router.TaskProfile{Difficulty: "low", EstTokensIn: 10, EstTokensOut: 10}
	})
	s := New(rt, reg, catalog)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fanoutBody)))
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 (unresolvable first entry must not burn the single attempt)", rec.Code)
	}
}

// buildServerWithAuth wires the standard fake-provider Server and configures a
// shared secret on it. An empty token leaves authentication disabled.
func buildServerWithAuth(t *testing.T, token string) *Server {
	t.Helper()
	s := buildServerWithProv(t, &fakeProv{text: "Hello"})
	s.SetAuthToken(token)
	return s
}

func TestRoutingAuth(t *testing.T) {
	const token = "s3cret-token"
	for _, c := range []struct {
		name       string
		configured string
		header     string
		value      string
		path       string
		want       int
	}{
		{"unconfigured allows anonymous", "", "", "", "/v1/messages", 200},
		{"correct x-api-key", token, "x-api-key", token, "/v1/messages", 200},
		{"correct bearer", token, "Authorization", "Bearer " + token, "/v1/messages", 200},
		{"wrong token anthropic", token, "x-api-key", "nope", "/v1/messages", 401},
		{"missing token anthropic", token, "", "", "/v1/messages", 401},
		{"wrong token openai", token, "Authorization", "Bearer nope", "/v1/chat/completions", 401},
		{"models requires auth", token, "", "", "/v1/models", 401},
		{"models with auth", token, "x-api-key", token, "/v1/models", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := buildServerWithAuth(t, c.configured)
			var req *http.Request
			if c.path == "/v1/models" {
				req = httptest.NewRequest(http.MethodGet, c.path, nil)
			} else {
				body := `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
				if c.path == "/v1/chat/completions" {
					body = `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
				}
				req = httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body))
			}
			if c.header != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// The error body must match the endpoint the caller used, not a single shape.
func TestAuthErrorShapePerEndpoint(t *testing.T) {
	s := buildServerWithAuth(t, "tok")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	var anth map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anth); err != nil {
		t.Fatalf("anthropic body not JSON: %v", err)
	}
	if anth["type"] != "error" {
		t.Errorf("anthropic error body = %v, want top-level type=error", anth)
	}

	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)))
	var oai map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &oai); err != nil {
		t.Fatalf("openai body not JSON: %v", err)
	}
	if _, ok := oai["error"]; !ok || oai["type"] != nil {
		t.Errorf("openai error body = %v, want an error object with no top-level type", oai)
	}
}
