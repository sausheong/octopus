# Fallback and Routing Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop Octopus from fanning out across the whole model catalog on failures where another backend cannot help, and fix three routing-correctness defects that feed that fan-out.

**Architecture:** Add a single `Retryable(error) bool` predicate to `anthropicio` (which already owns cross-provider backend-error classification) and consult it inline in the two existing fallback loops in `server/server.go`, alongside a configurable attempt cap. Three independent routing fixes ride along: counting `SystemPromptParts` when estimating tokens, an optional `caps.max_output_tokens` eligibility filter, and prefixing client-supplied user identifiers so they stop collapsing separate conversations into one sticky session.

**Tech Stack:** Go 1.25.1, `gopkg.in/yaml.v3`, `github.com/sausheong/harness` (llm types), standard `testing` package. No new dependencies.

## Global Constraints

- Module is `github.com/sausheong/octopus`; Go 1.25.1.
- **Always run Go commands with `GOWORK=off`** — the repo has a `go.work` pointing at a sibling `../harness` checkout that is gitignored. Example: `GOWORK=off go test ./...`.
- All tests must be hermetic. No network, no real provider calls. Use the existing `registry.NewForTest(map[string]llm.LLMProvider{...})` seam.
- `config.Parse` uses `dec.KnownFields(true)`, so any new YAML key must be added to the `yamlConfig`/`yamlRoutingCfg` structs or existing configs with that key will fail to parse.
- Existing `config.yaml` files must keep working unchanged. Every new config field is optional with a documented default.
- Preserve the existing comment style: explain *why*, not *what*. Match surrounding density.
- Every task ends with a passing `GOWORK=off go test ./...` and a commit.

---

### Task 1: Error classification — `KindCanceled` and `Retryable`

Adds the retry-policy predicate. Nothing consumes it yet; Task 2 wires it in.

**Files:**
- Modify: `anthropicio/errors.go`
- Test: `anthropicio/errors_test.go`

**Interfaces:**
- Consumes: existing `APIError{Kind, Message}`, `NewAPIError(kind, msg string) APIError`, `MapBackendError(err error) APIError`.
- Produces:
  - `const KindCanceled = "canceled"`
  - `func Retryable(err error) bool`
  - `MapBackendError` now returns `Kind == KindCanceled` for `context.Canceled` / `context.DeadlineExceeded`.

- [ ] **Step 1: Write the failing test**

Append to `anthropicio/errors_test.go`:

```go
// cancelWrapper mimics an SDK that wraps context cancellation inside its own
// error type. MapBackendError must detect the wrapped cancellation before it
// reaches the SDK type switches, or cancellation is misclassified as a
// retryable upstream failure and the fan-out defect persists.
type cancelWrapper struct{ inner error }

func (c cancelWrapper) Error() string { return "sdk: " + c.inner.Error() }
func (c cancelWrapper) Unwrap() error { return c.inner }

func TestMapBackendErrorClassifiesCancellation(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare canceled", context.Canceled},
		{"bare deadline", context.DeadlineExceeded},
		{"wrapped canceled", cancelWrapper{inner: context.Canceled}},
		{"wrapped in fmt.Errorf", fmt.Errorf("stream open: %w", context.Canceled)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapBackendError(c.err).Kind; got != KindCanceled {
				t.Errorf("Kind = %q, want %q", got, KindCanceled)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", NewAPIError("rate_limit", "slow down"), true},
		{"overloaded", NewAPIError("overloaded", "busy"), true},
		{"upstream", NewAPIError("upstream", "boom"), true},
		{"invalid request", NewAPIError("invalid_request", "bad max_tokens"), false},
		{"canceled", NewAPIError(KindCanceled, "gone"), false},
		{"anthropic 429", anthErr(429), true},
		{"anthropic 400", anthErr(400), false},
		{"anthropic 529", anthErr(529), true},
		{"raw context cancel", context.Canceled, false},
		{"unknown error", errors.New("mystery"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Retryable(c.err); got != c.want {
				t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
```

Add `"context"` and `"fmt"` to that file's import block (it already imports `errors`, `net/http`, `net/url`, `testing`, and the anthropic SDK).

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test ./anthropicio/ -run 'TestRetryable|TestMapBackendErrorClassifiesCancellation' -v`
Expected: compile failure — `undefined: KindCanceled`, `undefined: Retryable`.

- [ ] **Step 3: Write minimal implementation**

In `anthropicio/errors.go`, add `"context"` to the import block, then add the constant next to the `APIError` type:

```go
// KindCanceled marks client-side cancellation: the caller went away, so there
// is no point trying another backend and no client left to write a response to.
const KindCanceled = "canceled"
```

At the very top of `MapBackendError`, **before** the existing `var anthErr *anthropic.Error` block:

```go
	// Checked first, before the SDK type switches: some SDKs wrap cancellation
	// inside their own error types, so an errors.As match on a provider error
	// would otherwise shadow it and misreport a hang-up as a retryable 502.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewAPIError(KindCanceled, err.Error())
	}
```

Then append to the file:

```go
// Retryable reports whether trying a different backend could plausibly help.
// A malformed request fails identically everywhere, and a cancelled request
// has no one waiting for it; everything else is worth another candidate.
func Retryable(err error) bool {
	switch MapBackendError(err).Kind {
	case "invalid_request", KindCanceled:
		return false
	default:
		return true
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./anthropicio/ -v`
Expected: PASS, including the pre-existing `TestMapErrorKinds` and backend-mapping tests.

- [ ] **Step 5: Commit**

```bash
git add anthropicio/errors.go anthropicio/errors_test.go
git commit -m "feat: classify cancellation and add Retryable predicate"
```

---

### Task 2: Configurable attempt cap

Adds `routing.max_attempts` to config and carries it on the routing decision. Task 3 consumes it.

**Files:**
- Modify: `config/config.go`
- Modify: `router/scorer.go` (add field to `Decision`)
- Modify: `router/router.go` (populate the field in `Route`)
- Modify: `settings/document.go` (round-trip the new field)
- Test: `config/config_test.go`, `router/router_test.go`, `settings/store_test.go`

**Interfaces:**
- Consumes: `config.RoutingCfg`, `yamlRoutingCfg`, `router.Decision`, `Router.Route`, `settings.RoutingDocument`.
- Produces:
  - `config.RoutingCfg.MaxAttempts int` — YAML key `max_attempts`. Zero means unset and is defaulted to 3 by both `Parse` and `Validate`; negative is rejected.
  - `router.Decision.MaxAttempts int` — populated by `Route` from config.
  - `settings.RoutingDocument.MaxAttempts int` — JSON key `max_attempts`.

- [ ] **Step 1: Write the failing tests**

Append to `config/config_test.go`:

```go
func TestParseDefaultsMaxAttempts(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
weights:
  quality: 1
catalog:
  - id: "p/m"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
providers:
  p:
    api_key_env: "K"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want default 3", cfg.Routing.MaxAttempts)
	}
}

func TestParseHonoursExplicitMaxAttempts(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
weights:
  quality: 1
routing:
  max_attempts: 1
catalog:
  - id: "p/m"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
providers:
  p:
    api_key_env: "K"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.MaxAttempts != 1 {
		t.Errorf("MaxAttempts = %d, want 1", cfg.Routing.MaxAttempts)
	}
}

// Zero means "unset, use the default" because settings builds Config values by
// hand and would otherwise trip on the zero value. Negative is the error case.
func TestValidateRejectsNegativeMaxAttempts(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Routing:    RoutingCfg{MaxAttempts: -1, SessionTTL: time.Hour},
		Providers:  map[string]ProviderCreds{"p": {APIKeyEnv: "K"}},
		Catalog:    []CatalogEntry{{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000}}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want error for max_attempts = -1, got nil")
	}
}

func TestValidateDefaultsZeroMaxAttempts(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Routing:    RoutingCfg{MaxAttempts: 0, SessionTTL: time.Hour},
		Providers:  map[string]ProviderCreds{"p": {APIKeyEnv: "K"}},
		Catalog:    []CatalogEntry{{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero max_attempts should default, got error: %v", err)
	}
	if cfg.Routing.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want defaulted to 3", cfg.Routing.MaxAttempts)
	}
}
```

Append to `settings/store_test.go` — the Settings UI rebuilds `RoutingCfg` field
by field, so a field missing from `RoutingDocument` is silently reset on save:

```go
func TestDocumentRoundTripPreservesMaxAttempts(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{SessionSticky: true, SessionTTL: time.Hour, CacheAware: true, MaxAttempts: 5},
		Providers:  map[string]config.ProviderCreds{"p": {APIKeyEnv: "K"}},
		Catalog: []config.CatalogEntry{
			{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: config.Caps{MaxContext: 1000}},
		},
	}
	back, err := documentFromConfig(cfg).config()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.Routing.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d after round trip, want 5", back.Routing.MaxAttempts)
	}
}
```

Ensure `time` and the `config` package are imported in that test file.

Append to `router/router_test.go`:

```go
func TestRouteCarriesMaxAttempts(t *testing.T) {
	cfg := &config.Config{
		ServerAddr: "x",
		Weights:    config.Weights{Quality: 1},
		Routing:    config.RoutingCfg{MaxAttempts: 2},
		Catalog: []config.CatalogEntry{
			{ID: "p/m", Quality: 0.9, Speed: 0.5, Caps: config.Caps{MaxContext: 100000}},
		},
	}
	reg := registry.NewForTest(map[string]llm.LLMProvider{"p": &stubProv{}})
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
```

Before writing this test, inspect `router/router_test.go` for the existing stub provider type and reuse it rather than adding `stubProv`. If no suitable stub exists, add one matching the `llm.LLMProvider` interface (`ChatStream`, `Models`, `NormalizeToolSchema`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOWORK=off go test ./config/ ./router/ -run 'MaxAttempts' -v`
Expected: compile failure — `unknown field MaxAttempts`.

- [ ] **Step 3: Write minimal implementation**

In `config/config.go`, add to `RoutingCfg`:

```go
	// MaxAttempts bounds how many backends one request may try. Without it,
	// worst-case latency and spend grow with catalog size.
	MaxAttempts int `yaml:"max_attempts"`
```

Add to `yamlRoutingCfg` (pointer, matching the existing `session_sticky` pattern so unset is distinguishable from an explicit value):

```go
	MaxAttempts *int `yaml:"max_attempts"`
```

In `Parse`, alongside the existing sticky/cacheAware defaulting:

```go
	maxAttempts := 3
	if yc.Routing.MaxAttempts != nil {
		maxAttempts = *yc.Routing.MaxAttempts
	}
```

and set `MaxAttempts: maxAttempts` in the `RoutingCfg` literal.

In `Marshal`, add `MaxAttempts: &copyCfg.Routing.MaxAttempts` to the `yamlRoutingCfg` literal.

In `Validate`, next to the existing `SessionTTL` checks:

```go
	if c.Routing.MaxAttempts < 0 {
		return fmt.Errorf("routing.max_attempts must not be negative")
	}
	if c.Routing.MaxAttempts == 0 {
		c.Routing.MaxAttempts = 3
	}
```

In `router/scorer.go`, add to the `Decision` struct:

```go
	// MaxAttempts bounds provider fallback for this request.
	MaxAttempts int
```

In `router/router.go` `Route`, after `d.ClassifierUsage = classifierUsage`:

```go
	d.MaxAttempts = r.cfg.Routing.MaxAttempts
```

In `settings/document.go`, add to `RoutingDocument`:

```go
	MaxAttempts int `json:"max_attempts"`
```

Add `MaxAttempts: 3` to the `RoutingDocument` literal in `defaultDocument()`.

In `documentFromConfig`, add to the `RoutingDocument` literal:

```go
			MaxAttempts:   cfg.Routing.MaxAttempts,
```

In `Document.config()`, add to the `config.RoutingCfg` literal:

```go
			MaxAttempts:   d.Routing.MaxAttempts,
```

This round-trip is not optional: `Document.config()` rebuilds `RoutingCfg` from
scratch, so a field it does not copy is silently zeroed every time the macOS
Settings UI saves.

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./config/ ./router/ ./settings/ -v`
Expected: PASS, including `TestDocumentRoundTripPreservesMaxAttempts`.

- [ ] **Step 5: Commit**

```bash
git add config/config.go config/config_test.go router/scorer.go router/router.go router/router_test.go settings/document.go settings/store_test.go
git commit -m "feat: add routing.max_attempts with default of 3"
```

---

### Task 3: Guard both fallback loops

The core fix. Consumes Tasks 1 and 2.

**Files:**
- Modify: `server/server.go` (`tryProvidersStream`, `collectWithFallback`, `handleMessages`, `handleChatCompletions`)
- Test: `server/server_test.go`

**Interfaces:**
- Consumes: `anthropicio.Retryable`, `anthropicio.KindCanceled`, `anthropicio.MapBackendError`, `router.Decision.MaxAttempts`.
- Produces: no new exported symbols. Behavior changes only.

- [ ] **Step 1: Write the failing tests**

Append to `server/server_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOWORK=off go test ./server/ -run 'Attempt|Cancelled|Permanent400|RateLimit|Fanout|Unresolvable' -v`
Expected: FAIL. `TestPermanent400StopsAfterOneAttempt` reports 8 calls and status 502; the cap tests report 8 calls.

- [ ] **Step 3: Write minimal implementation**

In `server/server.go`, add a small helper above `tryProvidersStream`:

```go
// attemptCap returns the per-request fallback bound, defaulting to 3 when a
// Decision predates the config field (hand-built Decisions in tests).
func attemptCap(dec router.Decision) int {
	if dec.MaxAttempts > 0 {
		return dec.MaxAttempts
	}
	return 3
}
```

In `tryProvidersStream`, declare before the loop (`maxTries`, not `cap` — the
latter shadows the builtin):

```go
	attempts := 0
	maxTries := attemptCap(dec)
```

At **each** of the three points inside the loop that currently set `lastErr` and `continue` for a *backend* failure — the `prov.ChatStream` error, the empty-stream case, and the `first.Type == llm.EventError` case — replace the bare `continue` with:

```go
		attempts++
		if !anthropicio.Retryable(lastErr) || attempts >= maxTries {
			break
		}
		continue
```

Leave the `prepareAttempt` resolve-failure branch as a bare `continue` — it does not increment `attempts`.

Apply the identical change in `collectWithFallback` at its three backend-failure points: the `prov.ChatStream` error, the `peekForContent` error, and the `collect` error.

In `handleMessages`, replace the two `writeError(w, anthropicio.MapBackendError(err))` calls with:

```go
		if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
			// The client hung up; there is no one to receive a status or body.
			slog.Debug("request cancelled by client", "requested_model", dr.RequestedModel)
			return
		}
		writeError(w, anthropicio.MapBackendError(err))
		return
```

In `handleChatCompletions`, apply the same guard before each `oaiBackendError(w, err)` call, logging with `"requested_model", requestedModel`.

Finally, in both loops change the fallback log line from `slog.Warn` to a level chosen by class, so one hang-up no longer emits a warning per catalog entry:

```go
		if anthropicio.MapBackendError(err).Kind == anthropicio.KindCanceled {
			slog.Debug("request cancelled during provider attempt", "model", id)
		} else {
			slog.Warn("provider stream open failed, trying fallback", "model", id, "err", err)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./server/ -v`
Expected: PASS, including all pre-existing fallback tests. If a pre-existing test asserted full-catalog fan-out, read it carefully — it likely encoded the old buggy behavior and should be updated to the new expectation, not deleted.

- [ ] **Step 5: Full suite and race check**

Run: `GOWORK=off go test ./... && GOWORK=off go test -race ./... && GOWORK=off go vet ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add server/server.go server/server_test.go
git commit -m "fix: stop provider fallback on non-retryable errors and cap attempts"
```

---

### Task 4: Count `SystemPromptParts` when estimating tokens

Independent of Tasks 1–3.

**Files:**
- Modify: `router/profile.go` (`EstimateRequestTokens`)
- Test: `router/router_test.go`

**Interfaces:**
- Consumes: `llm.ChatRequest.SystemPromptParts []llm.SystemPromptPart` (each has a `Text string`).
- Produces: no signature change to `EstimateRequestTokens(chat llm.ChatRequest) int`.

- [ ] **Step 1: Write the failing test**

Append to `router/router_test.go`:

```go
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
```

Ensure `strings` is in that file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOWORK=off go test ./router/ -run TestEstimateRequestTokensCountsSystemPromptParts -v`
Expected: FAIL — `parts-only estimate = 5, want >= 15000`.

- [ ] **Step 3: Write minimal implementation**

In `router/profile.go`, replace `totalBytes += len(chat.SystemPrompt)` with:

```go
	// SystemPromptParts, when non-empty, replaces SystemPrompt in harness
	// semantics — and anthropicio.Decode populates both for a structured
	// prompt, so counting each unconditionally would double the estimate.
	if len(chat.SystemPromptParts) > 0 {
		for _, part := range chat.SystemPromptParts {
			totalBytes += len(part.Text)
		}
	} else {
		totalBytes += len(chat.SystemPrompt)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./router/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/profile.go router/router_test.go
git commit -m "fix: count SystemPromptParts when estimating request tokens"
```

---

### Task 5: Optional `caps.max_output_tokens` eligibility filter

Independent of Tasks 1–4.

**Files:**
- Modify: `config/config.go` (`Caps` struct, `Validate`)
- Modify: `router/scorer.go` (`eligible`)
- Test: `router/scorer_test.go`, `config/config_test.go`

**Interfaces:**
- Consumes: `config.Caps`, `router.TaskProfile.EstTokensOut`.
- Produces: `config.Caps.MaxOutputTokens int` — YAML key `max_output_tokens`, JSON key `max_output_tokens`. Zero means unconstrained.

- [ ] **Step 1: Write the failing tests**

Append to `router/scorer_test.go`:

```go
func TestEligibilityRespectsMaxOutputTokens(t *testing.T) {
	catalog := []config.CatalogEntry{
		{ID: "p/small", Quality: 0.9, Speed: 0.9,
			Caps: config.Caps{MaxContext: 200000, MaxOutputTokens: 4096}},
		{ID: "p/large", Quality: 0.8, Speed: 0.5,
			Caps: config.Caps{MaxContext: 200000, MaxOutputTokens: 64000}},
		{ID: "p/unset", Quality: 0.7, Speed: 0.5,
			Caps: config.Caps{MaxContext: 200000}},
	}
	w := config.Weights{Quality: 1}

	// Within every limit: all three eligible.
	small := Score(TaskProfile{EstTokensIn: 100, EstTokensOut: 1000}, catalog, w)
	if len(small.Eligible) != 3 {
		t.Errorf("small request eligible = %v, want all 3", small.Eligible)
	}

	// Exceeds p/small's output limit only.
	big := Score(TaskProfile{EstTokensIn: 100, EstTokensOut: 30000}, catalog, w)
	for _, id := range big.Eligible {
		if id == "p/small" {
			t.Errorf("p/small (4096 output cap) eligible for a 30000-token output request")
		}
	}
	if len(big.Eligible) != 2 {
		t.Errorf("big request eligible = %v, want p/large and p/unset", big.Eligible)
	}
}
```

Append to `config/config_test.go`:

```go
func TestValidateRejectsNegativeMaxOutputTokens(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Providers:  map[string]ProviderCreds{"p": {APIKeyEnv: "K"}},
		Catalog: []CatalogEntry{
			{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000, MaxOutputTokens: -1}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want error for negative max_output_tokens, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOWORK=off go test ./router/ ./config/ -run 'MaxOutputTokens' -v`
Expected: compile failure — `unknown field MaxOutputTokens in struct literal`.

- [ ] **Step 3: Write minimal implementation**

In `config/config.go`, add to `Caps`:

```go
	// MaxOutputTokens is the model's output limit. Zero means unconstrained,
	// so configs written before this field keep working unchanged.
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
```

In `Validate`, inside the per-catalog-entry loop next to the `MaxContext` check:

```go
		if e.Caps.MaxOutputTokens < 0 {
			return fmt.Errorf("catalog id %q: caps.max_output_tokens must not be negative", e.ID)
		}
```

In `router/scorer.go` `eligible`, before the final `return true`:

```go
	if e.Caps.MaxOutputTokens > 0 && p.EstTokensOut > e.Caps.MaxOutputTokens {
		return false
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./router/ ./config/ ./settings/ -v`
Expected: PASS. `settings` is included because `Caps` round-trips through `settings.CatalogDocument`.

- [ ] **Step 5: Commit**

```bash
git add config/config.go config/config_test.go router/scorer.go router/scorer_test.go
git commit -m "feat: add optional caps.max_output_tokens eligibility filter"
```

---

### Task 6: Stop user identifiers collapsing separate conversations

Independent of Tasks 1–5. Touches both decoders.

**Files:**
- Modify: `anthropicio/decode.go:62`
- Modify: `openaiio/decode.go:29`
- Modify: `router/router.go` (`SessionID`)
- Test: `router/router_test.go`, `anthropicio/decode_test.go`, `openaiio/openaiio_test.go`

**Interfaces:**
- Consumes: `llm.ChatRequest.SessionID` (documented in harness as routing metadata never sent to providers).
- Produces: a prefix convention on `SessionID`:
  - `"user:" + id` — a client-supplied user identifier. Folded into the derived hash, **not** treated as an explicit session.
  - any other non-empty value — an explicit session (the `X-Octopus-Session-ID` header path, which assigns unprefixed).
  - `""` — derived hash only.

- [ ] **Step 1: Write the failing tests**

Append to `router/router_test.go`:

```go
func TestSessionIDDoesNotCollapseConversationsPerUser(t *testing.T) {
	poem := llm.ChatRequest{
		SessionID: "user:alice", SystemPrompt: "sysA",
		Messages: []llm.Message{{Role: "user", Content: "write a poem"}},
	}
	debug := llm.ChatRequest{
		SessionID: "user:alice", SystemPrompt: "sysB",
		Messages: []llm.Message{{Role: "user", Content: "debug this kernel panic"}},
	}
	if SessionID(poem) == SessionID(debug) {
		t.Error("one user's two conversations still collapse to a single session")
	}
}

func TestSessionIDSeparatesUsersWithIdenticalPrompts(t *testing.T) {
	base := llm.ChatRequest{
		SystemPrompt: "sys",
		Messages:     []llm.Message{{Role: "user", Content: "hello"}},
	}
	alice, bob := base, base
	alice.SessionID = "user:alice"
	bob.SessionID = "user:bob"
	if SessionID(alice) == SessionID(bob) {
		t.Error("two users sending identical prompts share a session")
	}
}

func TestSessionIDExplicitHeaderStillWins(t *testing.T) {
	a := llm.ChatRequest{
		SessionID: "hdr-1", SystemPrompt: "sysA",
		Messages: []llm.Message{{Role: "user", Content: "totally different"}},
	}
	b := llm.ChatRequest{
		SessionID: "hdr-1", SystemPrompt: "sysB",
		Messages: []llm.Message{{Role: "user", Content: "also different"}},
	}
	if SessionID(a) != SessionID(b) {
		t.Error("an explicit session header must pin both requests to one session")
	}
	if !strings.HasPrefix(SessionID(a), "explicit:") {
		t.Errorf("SessionID = %q, want explicit: prefix", SessionID(a))
	}
}
```

Append to `anthropicio/decode_test.go`:

```go
func TestDecodeTagsUserIDAsNonExplicitSession(t *testing.T) {
	dr, err := Decode([]byte(`{"model":"m","max_tokens":10,
		"metadata":{"user_id":"alice"},
		"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dr.Chat.SessionID != "user:alice" {
		t.Errorf("SessionID = %q, want %q", dr.Chat.SessionID, "user:alice")
	}
}

func TestDecodeLeavesSessionEmptyWithoutUserID(t *testing.T) {
	dr, err := Decode([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dr.Chat.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", dr.Chat.SessionID)
	}
}
```

Append to `openaiio/openaiio_test.go`:

```go
func TestDecodeTagsUserAsNonExplicitSession(t *testing.T) {
	chat, _, _, err := Decode([]byte(`{"model":"m","user":"bob",
		"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chat.SessionID != "user:bob" {
		t.Errorf("SessionID = %q, want %q", chat.SessionID, "user:bob")
	}
}

func TestDecodeLeavesSessionEmptyWithoutUser(t *testing.T) {
	chat, _, _, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chat.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", chat.SessionID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `GOWORK=off go test ./router/ ./anthropicio/ ./openaiio/ -run 'SessionID|UserID|AsNonExplicit|WithoutUser' -v`
Expected: FAIL — decoders emit a bare `"alice"`/`"bob"`, and `SessionID` treats it as explicit so the two conversations collide.

- [ ] **Step 3: Write minimal implementation**

In `anthropicio/decode.go`, replace `SessionID: wr.Metadata.UserID,` in the `chat` literal with nothing, and after the literal add:

```go
	// metadata.user_id identifies a user, not a conversation. Tagging it keeps
	// it out of the explicit-session path, where it would otherwise collapse
	// all of one user's conversations onto a single sticky model and cache.
	if wr.Metadata.UserID != "" {
		chat.SessionID = "user:" + wr.Metadata.UserID
	}
```

In `openaiio/decode.go`, change `chat := llm.ChatRequest{Model: wr.Model, SessionID: wr.User}` to:

```go
	chat := llm.ChatRequest{Model: wr.Model}
	// Same reasoning as the Anthropic endpoint: "user" names a user, not a
	// conversation.
	if wr.User != "" {
		chat.SessionID = "user:" + wr.User
	}
```

In `router/router.go`, replace the explicit branch of `SessionID` with:

```go
	// A "user:" prefix marks a client-supplied user identifier rather than a
	// conversation. It disambiguates two users who send the same opening
	// prompt, but must not pin all of one user's conversations together — so
	// it feeds the derived hash instead of short-circuiting as an explicit ID.
	userTag := ""
	if strings.HasPrefix(chat.SessionID, "user:") {
		userTag = chat.SessionID
	} else if chat.SessionID != "" {
		sum := sha256.Sum256([]byte(chat.SessionID))
		return "explicit:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	h.Write([]byte(userTag))
	h.Write([]byte{0})
	h.Write([]byte(chat.SystemPrompt))
```

leaving the rest of the derived-hash body unchanged. `strings` is already imported in that file.

- [ ] **Step 4: Run tests to verify they pass**

Run: `GOWORK=off go test ./router/ ./anthropicio/ ./openaiio/ ./server/ -v`
Expected: PASS. `server` is included because its session-header tests exercise the explicit path.

- [ ] **Step 5: Commit**

```bash
git add anthropicio/decode.go anthropicio/decode_test.go openaiio/decode.go openaiio/openaiio_test.go router/router.go router/router_test.go
git commit -m "fix: stop user identifiers collapsing separate conversations"
```

---

### Task 7: Documentation

**Files:**
- Modify: `config.example.yaml`
- Modify: `README.md`

**Interfaces:**
- Consumes: `routing.max_attempts` (Task 2), `caps.max_output_tokens` (Task 5), the behavior changes from Tasks 3 and 6.
- Produces: nothing consumed by code.

- [ ] **Step 1: Update `config.example.yaml`**

In the `routing:` block, after `cache_aware: true`:

```yaml
  # Maximum backends one request may try before giving up. Only retryable
  # failures (rate limits, overloads, transport errors) consume an attempt;
  # a malformed request stops immediately. Defaults to 3 when omitted.
  max_attempts: 3
```

In the first catalog entry's `caps`, add `max_output_tokens` and a comment above the entry noting it is optional:

```yaml
    # max_output_tokens is optional; omit it (or use 0) for no output limit.
    caps: { tools: true, vision: true, reasoning: true, max_context: 1000000, max_output_tokens: 32000 }
```

- [ ] **Step 2: Update the README `routing` config-reference section**

Find the `### \`routing\`` section (near line 589) and add a `max_attempts` row/entry matching the surrounding format, describing the default of 3 and that only retryable failures consume an attempt.

- [ ] **Step 3: Update the README `catalog` config-reference section**

Find the `### \`catalog\`` section (near line 643) and document `caps.max_output_tokens` as optional, zero/omitted meaning unconstrained, enforced as a hard eligibility filter like `max_context`.

- [ ] **Step 4: Update the README "Provider fallback" section**

Find `### Provider fallback` (near line 488). Replace the bullet list so it states the new behavior:

- Eligible models are attempted in score order, starting with the chosen model.
- Only retryable failures — rate limits, overloads, and transport errors — advance to the next model.
- A malformed request (HTTP 400) stops immediately and the backend's own error is returned, rather than being retried across the catalog and masked as a 502.
- A cancelled request stops immediately; no further backends are tried.
- `routing.max_attempts` (default 3) bounds the total number of backends tried.
- A catalog entry naming an unconfigured provider is skipped without consuming an attempt.
- Once streaming response bytes have been sent, Octopus cannot transparently switch providers.

- [ ] **Step 5: Update the README session-affinity section**

Find `### Session affinity` (near line 397) and document that `X-Octopus-Session-ID` is the explicit session key, while Anthropic `metadata.user_id` and OpenAI `user` contribute to the derived session hash — separating users without merging one user's distinct conversations.

- [ ] **Step 6: Verify the example config still parses**

Run:

```bash
GOWORK=off go test ./config/ -v
```

Then confirm the example file is valid by parsing it directly:

```bash
cat > /tmp/parse_example_test.go <<'EOF'
package config

import (
	"os"
	"testing"
)

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("config.example.yaml does not parse: %v", err)
	}
}
EOF
cp /tmp/parse_example_test.go config/zz_example_test.go
GOWORK=off go test ./config/ -run TestExampleConfigParses -v
rm config/zz_example_test.go
```

Expected: PASS. This catches a `KnownFields(true)` rejection of a mistyped new key. Remove the temporary file before committing — `git status` must show only `config.example.yaml` and `README.md`.

- [ ] **Step 7: Commit**

```bash
git status --porcelain
git add config.example.yaml README.md
git commit -m "docs: document max_attempts, max_output_tokens, and fallback changes"
```

---

### Task 8: Final verification

**Files:** none modified.

- [ ] **Step 1: Full build, vet, test, race**

```bash
GOWORK=off go build ./... && \
GOWORK=off go vet ./... && \
GOWORK=off go test ./... && \
GOWORK=off go test -race ./...
```

Expected: all PASS, no output from build or vet.

- [ ] **Step 2: Confirm a clean tree**

```bash
git status --porcelain
```

Expected: empty. Any stray `zz_*_test.go` probe file is a mistake — delete it.

- [ ] **Step 3: Review the combined diff against the spec**

```bash
git log --oneline 11bc902..HEAD
git diff 11bc902..HEAD --stat
```

Confirm every file listed in the spec's "Files affected" section appears, and no unrelated file does.
