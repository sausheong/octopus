# Fallback and Routing Correctness

**Date:** 2026-07-26
**Status:** Approved
**Scope:** Sub-project C of the post-review remediation work

## Problem

A code review of Octopus surfaced nineteen findings across five independent
areas. This spec covers one of them: correctness and cost defects in the
routing and provider-fallback path. Each defect below was confirmed by a
probe test run against the current code.

1. **Unconditional fallback.** `candidates()` returns every eligible model and
   both fallback loops try each one regardless of why the previous attempt
   failed. With an eight-model catalog, a permanent `400 invalid_request`
   produced eight provider calls; a client disconnect also produced eight. The
   400 case bills the user eight times for a request that could never succeed,
   and the final response is a generic 502 that hides the real 400.

2. **No attempt cap.** Worst-case latency and spend grow linearly with catalog
   size, with no bound.

3. **`SystemPromptParts` is not counted when estimating tokens.**
   `EstimateRequestTokens` reads `chat.SystemPrompt` but never
   `chat.SystemPromptParts`. A request carrying 60 KB in parts with an empty
   `SystemPrompt` estimated **5 tokens**. That underestimate defeats the
   context-window eligibility filter, which is the estimator's only purpose.

4. **`metadata.user_id` collapses unrelated conversations.** Both endpoints
   treat a client-supplied user identifier as an explicit session key. Two
   entirely different conversations from one `user_id` hash to a single
   session, so sticky affinity pins them to one model and cache-aware scoring
   predicts a cache hit against a prefix that does not exist.

5. **No output-token eligibility check.** `caps.max_context` is enforced, but
   nothing compares the request's `max_tokens` against a model's output limit.
   The router can select a model that will reject the request, which then
   triggers defect 1.

## Goals

- Stop fanning out on failures where another backend cannot help.
- Bound worst-case attempts regardless of catalog size.
- Surface the backend's real error instead of masking it as 502.
- Make token estimation account for structured system prompts.
- Make session identity reflect conversations, not users.
- Allow the catalog to declare an output-token limit.

## Non-goals

Deferred to other sub-projects: dropped API parameters (`top_p`, `top_k`,
`stop_sequences`, `tool_choice`, `thinking`), the temperature pointer, the
OpenAI `created` field, `message_start` usage, `count_tokens`, settings DNS
rebinding, the stream goroutine leak, Insights response-path latency, and
LICENSE/CI. Those are independent and get their own specs.

## Design

### 1. Error classification

`anthropicio` already owns cross-provider backend-error classification via
`MapBackendError`, which returns an `APIError` carrying a `Kind`. Extend it in
place.

```go
// KindCanceled marks client-side cancellation: the caller went away, so
// there is no point trying another backend and no client to write to.
const KindCanceled = "canceled"

// Retryable reports whether trying a different backend could plausibly help.
func Retryable(err error) bool
```

`MapBackendError` gains a `context.Canceled` / `context.DeadlineExceeded`
check that runs **before** the existing SDK type switches. Ordering is
load-bearing: some SDKs wrap cancellation inside their own error types, so the
`errors.Is` check must precede the `errors.As` checks or cancellation will be
misclassified as an upstream 502.

| Kind | Retryable | Rationale |
|---|---|---|
| `rate_limit` (429) | yes | Another backend may have capacity |
| `overloaded` (503/529/5xx) | yes | Transient upstream failure |
| `upstream` (transport, empty stream, unparseable) | yes | Network flake or one bad backend |
| `invalid_request` (400) | no | Request is malformed; stop and surface it |
| `canceled` | no | Client is gone |

The table has exactly one definition. Both loops call `Retryable`.

### 2. The two fallback loops

`tryProvidersStream` and `collectWithFallback` keep their existing structure.
Each gains an attempt counter and the same guard at each point where it
currently sets `lastErr` and continues:

```go
attempts++
if !anthropicio.Retryable(err) || attempts >= maxAttempts {
    break   // lastErr is already set
}
```

A resolve failure is a configuration error, not a backend error: it means a
catalog entry names a provider that is not configured. It does not increment
the counter and always advances to the next candidate.

Error surfacing changes:

- **Non-retryable stop** returns `lastErr` unchanged, so a backend 400 reaches
  the client as a 400 carrying the backend's own message. Today it becomes a
  generic 502.
- **Cancellation** returns from the handler without writing a status or body,
  because there is no client left to receive one. It is logged at `Debug`, not
  `Warn`, so a single hang-up no longer emits one warning per catalog entry.

### 3. Configuration

```yaml
routing:
  max_attempts: 3   # optional; default 3
```

`config.RoutingCfg` gains `MaxAttempts int`. `yamlRoutingCfg` uses `*int`,
matching the existing pointer pattern for `session_sticky` and `cache_aware`,
so an unset value is distinguishable from an explicit one; `Parse` defaults nil
to 3. `Validate` requires `>= 1`.

The value reaches the loops on `router.Decision`, which already carries
per-request policy such as `Reasoning`. No new parameter threading through
`Server` is required.

### 4. Token estimation for structured system prompts

`EstimateRequestTokens` gains a `SystemPromptParts` branch.

Double-counting is the hazard: `anthropicio.Decode` sets **both**
`SystemPrompt` and `SystemPromptParts` for a structured prompt. The rule
follows harness's own documented semantics — `SystemPromptParts`, when
non-empty, *replaces* `SystemPrompt`:

```go
if len(chat.SystemPromptParts) > 0 {
    for _, part := range chat.SystemPromptParts {
        totalBytes += len(part.Text)
    }
} else {
    totalBytes += len(chat.SystemPrompt)
}
```

### 5. Output-token eligibility

`config.Caps` gains an optional `MaxOutputTokens int` (`max_output_tokens`).
In `eligible()`:

```go
if e.Caps.MaxOutputTokens > 0 && p.EstTokensOut > e.Caps.MaxOutputTokens {
    return false
}
```

Zero means unconstrained, so every existing `config.yaml` keeps working with no
migration. `Validate` rejects negative values.

### 6. Session identity

`llm.ChatRequest.SessionID` is documented in harness as routing metadata that
is never sent to providers, so it can carry a tagged value rather than
requiring a parallel field.

Both decoders stop treating a client-supplied user identifier as an explicit
session key:

- `anthropicio.Decode` currently sets `SessionID: wr.Metadata.UserID`.
- `openaiio.Decode` currently sets `SessionID: wr.User` — the same defect.

Both instead emit the value prefixed, as `"user:" + id`. `router.SessionID`
branches on the prefix:

- No prefix and non-empty → explicit session (the `X-Octopus-Session-ID`
  header path). Unchanged behavior.
- `user:` prefix → **not** an explicit session. The identifier is written into
  the derived hash alongside the system prompt, tool definitions, and first
  user message.
- Empty → derived hash as today.

The header path in `server.sessionIDHeader` overwrites `chat.SessionID`
unprefixed, so an explicit header continues to win over `user_id`.

Result: one user's two conversations get distinct sessions, and two users
sending an identical opening prompt also get distinct sessions.

## Testing

All tests are hermetic and use the existing `registry.NewForTest` seam. The
probe tests written during review are the templates for the loop assertions.

**Classification.** Table-driven over every kind, including a
`context.Canceled` wrapped in an SDK error type to lock in the ordering
requirement from Section 1.

**Fallback loops.** Assert exact provider call counts against an
eight-model catalog, for both streaming and buffered paths:

| Scenario | Expected calls | Expected response |
|---|---|---|
| Backend returns 400 | 1 | 400, backend's message |
| Backend returns 429 | 3 (default cap) | 429 |
| `max_attempts: 1`, backend 429 | 1 | 429 |
| Client context cancelled | 1 | no status, no body |
| First model unresolvable, second succeeds | 1 `ChatStream` call, 2 candidates walked | 200 |

The unresolvable-model row is the assertion that a resolve failure does not
consume an attempt: it never reaches `ChatStream`, so the counter stays at
zero and the loop advances even when `max_attempts: 1`.

**Estimator.** Parts-only, parts-plus-prompt (asserting no double count), and
prompt-only.

**Eligibility.** `max_output_tokens` unset, set-and-satisfied, and
set-and-exceeded.

**Session identity.** Two different conversations under one `user_id` produce
different session IDs; two users with identical prompts produce different
session IDs; an explicit header still overrides `user_id`; the OpenAI `user`
field behaves identically to Anthropic's `metadata.user_id`.

## Risks

- **Ordering bug in `MapBackendError`.** If the cancellation check lands after
  the SDK type switches, cancellation is silently misclassified as retryable
  and the fan-out defect persists. The wrapped-cancellation test guards this.
- **Behavior change for 400s.** Callers that today receive a 502 after a full
  fan-out will now receive a 400 quickly. This is the intended fix, but it is
  an observable change worth noting in the README.
- **`user_id` prefix collision.** A client sending a literal
  `X-Octopus-Session-ID: user:foo` header would be treated as derived rather
  than explicit. The header path assigns unprefixed, so this only occurs if a
  caller deliberately crafts that value; acceptable.

## Files affected

- `anthropicio/errors.go` — `KindCanceled`, `Retryable`, cancellation check
- `anthropicio/decode.go` — prefix `user_id`
- `openaiio/decode.go` — prefix `user`
- `server/server.go` — attempt guards in both loops, cancellation response path
- `router/router.go` — `SessionID` prefix handling, `MaxAttempts` on `Decision`
- `router/profile.go` — `SystemPromptParts` in the estimator
- `router/scorer.go` — `MaxOutputTokens` in `eligible()`
- `config/config.go` — `MaxAttempts`, `MaxOutputTokens`, validation
- `config.example.yaml`, `README.md` — document both new fields
