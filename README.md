# Octopus

![Octopus logo](octopus.png)

Octopus is a local LLM routing gateway for coding agents and OpenAI-compatible applications. It exposes Anthropic and OpenAI APIs on one loopback-only server, classifies each request, filters models by capability and context size, and chooses a backend using configurable quality, cost, and speed weights.

It is designed to work particularly well with Claude Code: Anthropic prompt-cache markers are preserved end to end, conversations remain on the backend that owns their cache, and cache creation/read usage is included in responses and logs.

## Contents

- [Quick start](#quick-start)
- [Using Octopus](#using-octopus)
  - [Claude Code](#claude-code)
  - [Codex CLI](#codex-cli)
  - [Other clients](#other-clients)
- [macOS menu bar app](#macos-menu-bar-app)
- [Prompt caching](#prompt-caching)
- [Routing behavior](#routing-behavior)
- [API compatibility](#api-compatibility)
- [Configuration reference](#configuration-reference)
- [Deployment and security](#deployment-and-security)
- [Observability and troubleshooting](#observability-and-troubleshooting)
- [Local and mixed-provider recipes](#local-and-mixed-provider-recipes)
- [Benchmarking](#benchmarking)
- [Development](#development)
- [Releasing](#releasing)
- [License](#license)

## Highlights

- Anthropic-compatible `POST /v1/messages`, streaming and buffered.
- OpenAI-compatible `POST /v1/chat/completions`, streaming and buffered.
- OpenAI-compatible `GET /v1/models` generated from the configured catalog.
- Routing across Anthropic, OpenAI, Gemini, Qwen, local models, and compatible gateways.
- Optional low-cost classifier with a zero-call short circuit for trivial requests.
- Hard tool, vision, context-window, and output-limit eligibility checks.
- Quality/cost/speed scoring with deterministic fallback order.
- Sticky conversation routing for prompt-cache continuity.
- Full Anthropic `cache_control` preservation, including `5m` and `1h` TTLs.
- Extended-thinking/reasoning mapping and thinking-block round trips.
- Tool calls, parallel tool results, images, streaming usage, and refusal propagation.
- Loopback-only binding, bounded request bodies, transport timeouts, and graceful shutdown.
- Native macOS menu-bar app with structured settings, an Advanced YAML editor, and immediate validated reloads.

## Quick start

### Requirements

- Go 1.25 or newer.
- An API key for each configured cloud provider, or a running local inference server.

### Install and build

```bash
git clone https://github.com/sausheong/octopus.git
cd octopus
make app
```

On macOS this builds `dist/Octopus.app`. Launch it with:

```bash
open dist/Octopus.app
```

Octopus appears only in the menu bar; it does not add a Dock icon. See [macOS menu bar app](#macos-menu-bar-app) for setup details. On Linux and other supported non-macOS systems, use `make build` to build the headless `./octopus` command.

### Configure

On macOS, choose **Settings…** from the Octopus menu-bar menu. Saving creates or updates `~/.octopus/config.yaml` and reloads the router immediately. You can use the structured General, Providers, and Models forms or edit the full file in Advanced YAML.

For the headless command, copy the example file:

```bash
cp config.example.yaml config.yaml
```

`config.yaml` is ignored by Git. A minimal Anthropic configuration is:

```yaml
server:
  addr: "127.0.0.1:8787"

weights:
  quality: 0.5
  cost: 0.3
  speed: 0.2

routing:
  session_sticky: true
  session_ttl: "1h"
  cache_aware: true

providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"

catalog:
  - id: "anthropic/claude-sonnet"
    quality: 0.90
    cost_per_mtok_in: 3.0
    cost_per_mtok_out: 15.0
    speed: 0.75
    caps:
      tools: true
      vision: true
      reasoning: true
      max_context: 200000
```

Use a model ID accepted by your provider and current pricing for the catalog cost fields.

### Run the headless command

The commands in this section apply to non-macOS builds. The macOS app reads `~/.octopus/config.yaml` and manages its own lifecycle.

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
./octopus
```

Use another configuration file with:

```bash
./octopus -config /path/to/config.yaml
```

The process logs `octopus listening` when it is ready. It handles `SIGINT` and `SIGTERM` by draining in-flight requests for up to 30 seconds.

## Using Octopus

Octopus speaks two APIs on one port, so most clients need only a base URL. Point the client at `http://127.0.0.1:8787` and keep the real provider keys in the Octopus configuration, not in the client.

### Claude Code

Run the two in separate terminals so the upstream provider credential and Claude Code's gateway token stay distinct.

Terminal 1 — start Octopus with the real provider key:

```bash
cd /path/to/octopus
export ANTHROPIC_API_KEY="sk-ant-real-provider-key"
./octopus
```

Terminal 2 — point Claude Code at Octopus:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_AUTH_TOKEN="local-octopus"
claude
```

`ANTHROPIC_AUTH_TOKEN` can be any non-empty value unless you have set `server.auth_token_env`, in which case it must match that token. Octopus never forwards this client token: upstream requests are built with credentials from `config.yaml`. This follows Anthropic's [Claude Code LLM gateway guidance](https://docs.anthropic.com/en/docs/claude-code/llm-gateway).

Claude Code requests a specific model, but Octopus treats the inbound model as advisory and picks a catalog model itself. The label shown in Claude Code is therefore the client-side name, not necessarily the backend that served the turn; the routed provider and model appear in Octopus logs and Insights. Passing `--model octopus` makes that distinction visible.

#### Verify prompt caching

1. Enable `routing.session_sticky` and `routing.cache_aware`, or omit them to use their `true` defaults.
2. Start a session with a large, stable prompt prefix.
3. Send at least two turns within the cache TTL.
4. Look for `prompt cache usage` in the Octopus log.
5. Confirm a later response reports a positive `cache_read_input_tokens`.

A zero cache count does not always mean forwarding failed. Providers impose minimum cacheable-prefix sizes, and a new or changed prefix must be written before it can be read.

### Codex CLI

**Codex CLI does not work with Octopus yet.** Current Codex versions require the OpenAI Responses API, which Octopus does not implement.

Codex removed support for the Chat Completions wire protocol ([openai/codex#7782](https://github.com/openai/codex/discussions/7782)). Setting `wire_api = "chat"` now fails at startup:

```
Error loading config.toml: `wire_api = "chat"` is no longer supported.
How to fix: set `wire_api = "responses"` in your provider config.
```

Setting `wire_api = "responses"` reaches Octopus but finds no such endpoint:

```
ERROR: unexpected status 404 Not Found: 404 page not found,
       url: http://127.0.0.1:8787/v1/responses
```

Octopus serves `/v1/messages`, `/v1/chat/completions`, and `/v1/models`. Adding `/v1/responses` is tracked as future work; until then, use a client that speaks either the Anthropic Messages API or OpenAI Chat Completions.

### Other clients

Any client that targets OpenAI Chat Completions works by overriding the base URL:

```bash
export OPENAI_BASE_URL="http://127.0.0.1:8787/v1"
export OPENAI_API_KEY="local-octopus"
```

`GET /v1/models` reports the configured catalog, so clients that populate a model picker from the server will list your catalog entries. As with Claude Code, the requested model is advisory — Octopus selects the backend.

## macOS menu bar app

### Build and launch

Requirements are macOS 13 or newer, Xcode Command Line Tools, and Go 1.25 or newer.

```bash
make app
open dist/Octopus.app
```

For regular use, copy `dist/Octopus.app` to `/Applications`. The locally built app is ad-hoc signed rather than distributed through the App Store; macOS may ask you to confirm the first launch.

The menu contains exactly two items:

1. **Settings…** opens the local settings web app in your default browser.
2. **Quit Octopus** stops the router and settings server, then exits.

There is no Dock icon and no application-window menu. The settings server binds to a random loopback port and is available only while Octopus is running.
### Settings and live reload

The app always reads and writes `~/.octopus/config.yaml`. If the file does not exist, Settings opens with safe defaults; the file is created on the first successful save. The containing directory is created with mode `0700` and the file is written atomically with mode `0600`.

Settings has five sections:

- **General** controls the router address, classifier, scoring weights, session/cache behavior, and the fallback attempt limit.
- **Providers** configures provider kinds, endpoints, environment-variable names, and optional inline credentials. Inline credentials are stored locally in the YAML file; prefer environment variables when practical.
- **Models** edits the routing catalog, pricing, capabilities, and context and output limits.
- **Advanced YAML** edits the complete configuration directly.
- **Insights** shows request volume, token usage, estimated spend, savings over time, cache efficiency, and model usage.

Saving requires a CSRF token that is generated fresh each time Octopus starts and embedded in the page when it loads. A settings page left open across a restart therefore holds a token the new process will not accept, and must be reopened from the menu bar before it can save.

Settings shows the configuration in full, inline `api_key` values included, on both the Providers form and the Advanced YAML tab. An editor that hides part of the file cannot show you what you are about to change, and the file is yours on your own machine. The protection is on writes rather than reads: a save is accepted only from a request whose `Host` header is literally loopback and that carries the current CSRF token, so a web page that resolves its own hostname to `127.0.0.1` cannot alter the configuration or repoint a provider. Such a page can read what Settings serves, so treat the settings port as it is treated here — anything in `~/.octopus/config.yaml` is readable by anything running on, or reaching, your machine.

Every save is parsed and validated before replacing the existing file. A valid save reloads the router immediately without restarting the menu-bar app. If validation fails, the existing file and running router remain unchanged and the settings page shows the error. If a newly saved configuration cannot start—for example because a credential environment variable is absent—the file remains saved, the previous working router stays active when possible, and the status area reports the problem.

The settings interface follows the macOS light or dark appearance and supports keyboard navigation, visible focus states, reduced motion, and WCAG 2.2 AA contrast.

### Insights

Insights is the last item in the Settings sidebar. It records one aggregate observation each time a provider completes a request with final token usage, over the last 7, 30, 90, or 365 days. Tracking starts when an Insights-capable build first runs; history is not reconstructed from logs.

It reports net savings, actual and baseline cost, request and token counts, savings over time, cache hit rate, and a per-model usage breakdown. Savings are split three ways: **model routing** (choosing a cheaper model than the baseline), **prompt caching** (cache reads against the chosen model's uncached cost), and **classifier overhead**, which is subtracted. Negative values are kept rather than clamped — a cache write or a classifier call really can cost more than it saved in a given period.

#### Baseline selection

The baseline answers: what would this same request have cost on the best model Octopus could safely have picked? For each request, Octopus applies the usual tool, vision, context, and difficulty filters, takes the highest-`quality` model still eligible, and prices it at ordinary uncached rates using the measured token counts. If the chosen model was already the highest quality, it is its own baseline.

#### Cost formulas

With `Pᵢ`/`Pₒ` the configured per-million input and output prices, and `I`, `W`, `R`, `O` the provider-reported ordinary-input, cache-write, cache-read, and output tokens:

```text
uncached(model)  = (I + W + R)/1e6 × Pᵢ(model) + O/1e6 × Pₒ(model)
chosen measured  = (I + W×write_mult + R×0.10)/1e6 × Pᵢ(chosen) + O/1e6 × Pₒ(chosen)

routing savings  = uncached(baseline) - uncached(chosen)
cache savings    = uncached(chosen)  - chosen measured
net savings      = routing savings + cache savings - classifier overhead
actual cost      = chosen measured + classifier overhead
```

`write_mult` is `1.25` for a five-minute cache and `2.00` for one hour. Classifier tokens are priced at the classifier model's own catalog rates.

#### Privacy and accuracy

Daily totals and per-model aggregates live in `~/.octopus/insights.json`, written atomically with mode `0600` inside a `0700` directory. The ledger holds dates, model IDs, token totals, request counts, and USD amounts — no prompts, responses, tool definitions, session identifiers, or credentials. Quit Octopus before deleting it to reset history.

These are estimates, not invoices. They use your catalog prices and provider-reported usage, so requests that never report final usage are not counted, and price edits apply only to future observations. The counterfactual also reuses the chosen model's measured token counts for the baseline, which a different model would not have reproduced exactly. Taxes, volume and batch discounts, and tiered pricing are outside the calculation.

### Provider credentials at launch

Start Octopus from an environment containing the provider credentials named in the configuration. For a local development build:

```bash
ANTHROPIC_API_KEY="sk-ant-..." dist/Octopus.app/Contents/MacOS/octopus
```

Applications opened from Finder do not inherit shell-only exports. For routine Finder launches, provide credentials through your login-session environment, use the optional inline-key field in the local configuration, or use a local provider that does not require a key. Prefer environment-variable names such as `ANTHROPIC_API_KEY`; an inline key is stored in `~/.octopus/config.yaml`, which Octopus protects with mode `0600` but does not encrypt.

### Remove the app

Quit Octopus, remove `Octopus.app`, and optionally remove `~/.octopus` if you no longer want the configuration and Insights history. Removing `~/.octopus` deletes user data and is not performed by the build or app.

## Prompt caching

Octopus preserves cache metadata rather than regenerating it. For Anthropic-shaped requests it supports:

- Top-level `cache_control` for automatic caching.
- Individual system text block markers without flattening the system array.
- Tool-definition markers.
- User and assistant content-block markers.
- Tool-result markers.
- `ephemeral` cache type.
- Default/`5m` and `1h` TTL values.

Invalid cache types or TTLs are rejected as invalid requests. Tool-schema normalization includes cache metadata in its deterministic cache key, so changing a cache TTL cannot reuse a stale normalized tool definition.

Only providers implementing the harness prompt-caching capability are priced as cache-capable. Other provider kinds safely ignore the provider-neutral cache metadata.

### Cache-aware cost model

Anthropic cache pricing is represented with these input multipliers:

| Operation | Input-price multiplier |
|---|---:|
| Cache read | `0.10×` |
| Five-minute cache write | `1.25×` |
| One-hour cache write | `2.00×` |
| Uncached input | `1.00×` |

After a successful response, Octopus records the reported ratio of cached to uncached input. Subsequent scores blend the cache-read and ordinary-input prices instead of assuming the entire request is cached.

### Session affinity

Sticky routing records the successful model for a conversation and selects it again while it remains eligible. Session identity is resolved in this order:

1. `X-Octopus-Session-ID` HTTP header, or the legacy `X-LLMRouter-Session-ID` header. Either is the explicit session key: it alone determines the session.
2. A deterministic SHA-256 identifier derived from the system prompt, tools, and first genuine user turn.

Anthropic `metadata.user_id` and the OpenAI `user` field are not explicit keys. They contribute to the derived hash instead, so two users sending the same opening prompt stay separated, while one user's distinct conversations are not merged onto a single model and a false prompt-cache prediction.

Explicit identifiers are hashed before being stored. The in-memory session table is concurrency-safe, expires entries, and has a hard size bound. Session state is process-local and is reset when Octopus restarts.

If the sticky model is no longer eligible because of tools, vision, or context size, normal scoring selects another model. A successful fallback becomes the new sticky model.

### Restart behavior

Octopus does not cache prompts itself — caching happens on the provider (e.g. Anthropic), keyed to the exact byte prefix it received. Octopus's own job is to avoid routing a conversation away from the backend holding its warm cache, and to price a routing decision honestly when it might cost a cache write. Both of those depend entirely on the in-memory session table, which is not persisted, so a process restart resets it to empty while any conversation it was tracking keeps going.

The next request in each affected session after a restart is scored without that memory:

- **Sticky affinity is unknown.** `stickyModelForSession` finds no entry, so the request is scored fresh instead of pinned to whichever model it was already using.
- **Cache fraction is unknown.** `cacheInputMultipliersForSession` has no `CacheUntil`/`CacheFraction` to blend in, so every cache-capable model is scored as if this request needs a full cache write (`1.25×`/`2.00×`), even for the model that actually still holds the warm cache at the provider.

Two outcomes follow, both limited to that one request:

1. Scoring still lands on the same model the conversation was already using. The provider's cache is likely still warm (if within its TTL), so the real bill reflects a cache hit — but Octopus's internal cost/savings accounting for that request overstates the cost, because it assumed a cold write. This is a bookkeeping inaccuracy in Insights, not an extra charge.
2. Scoring lands on a different model. This is a genuine cache-losing switch — a real cold write at whatever provider is now serving the conversation, no different from any other mid-conversation switch.

Either way, the response from the provider carries real `cache_creation_input_tokens`/`cache_read_input_tokens` usage, and `Router.Observe` repopulates the session table from it immediately after. So the effect of a restart is confined to one request per active session: the process self-corrects from the next turn onward. There is currently no snapshot/restore for the session table (unlike the Insights ledger, which is persisted to disk on every write) — a conversation's sticky/cache state does not survive a restart, only its session identity does, since `SessionID` is a pure deterministic hash of the request and needs no stored state to recompute.

## Routing behavior

Each request passes through the following pipeline:

```text
Anthropic/OpenAI request
        │
        ▼
Decode and validate wire format
        │
        ▼
Classify task or apply deterministic shortcut
        │
        ▼
Enforce tools, vision, and context constraints
        │
        ▼
Estimate request cost, including expected cache state
        │
        ▼
Rank eligible catalog models
        │
        ▼
Apply valid session affinity
        │
        ▼
Normalize tools and call provider
        │
        ├── retryable failure before response → next eligible model
        ▼
Encode response and observe usage/cache state
```

### Classification

The classifier returns this task profile:

- `difficulty`: `trivial`, `low`, `medium`, or `high`.
- `needs_reasoning`.
- `needs_vision`.
- `needs_tools`.
- `est_tokens_in` and `est_tokens_out`.
- `domain`: `code`, `writing`, `qa`, `math`, or `other`.

The classifier is skipped when either of these conditions applies:

- The request is a short single-turn prompt of at most 500 bytes with no tools, images, or prior assistant turn.
- `classifier.model` is omitted, in which case a conservative default profile is used.

For nontrivial conversations, classification uses a bounded recent-context representation. Classification timeouts, malformed output, and unavailable classifier providers fall back to the conservative profile rather than failing the client request.

Actual request contents override classifier guesses: images require vision and tools require tool support. A deterministic estimate of the complete system prompt, history, tool schemas, tool arguments/results, requested output, and images provides a context-size floor.

### Eligibility and scoring

Models are first filtered by hard requirements:

- Vision support when images are present.
- Tool support when tools are declared.
- Enough context for estimated input plus output.
- An output limit, when `caps.max_output_tokens` declares one, at least as large as the estimated response.

For high-difficulty tasks, models below the `0.85` quality floor are removed when at least one otherwise-eligible model clears the floor. This floor never empties the eligible set by itself.

Remaining models receive a normalized weighted score:

```text
quality × normalized_quality
+ cost × normalized_cost_efficiency
+ speed × normalized_speed
+ optional reasoning preference
```

Weights need not sum to one; Octopus normalizes them. Free models receive the maximum cost-efficiency score. Catalog order is the deterministic tie-breaker.

### Reasoning

When a task benefits from reasoning, the router recommends medium reasoning effort. The server applies it only to catalog entries with `caps.reasoning: true`. Harness providers map this to their native mechanism, including Anthropic adaptive/extended thinking and provider-specific reasoning settings. Thinking blocks and signatures are preserved for later turns.

### Provider fallback

Eligible models are attempted in score order, starting with the chosen model.

- Every failure advances to the next model except a malformed request and a cancelled request; anything else, including an error the router cannot classify, is treated as retryable. Failure while opening a stream, a closed stream, or an `EventError` before the first meaningful event all qualify. Buffered responses may also fall back after a later collection failure, because no client bytes have been written yet.
- A malformed request (HTTP `400`) stops immediately and the backend's own error is returned, rather than being retried across the catalog and masked as a `502`.
- A cancelled request stops immediately; no further backends are tried, and no status or body is written because the client is gone.
- `routing.max_attempts` (default `3`) bounds the total number of backends tried.
- A catalog entry naming an unconfigured provider is skipped without consuming an attempt.
- Once streaming response bytes have been sent, Octopus cannot transparently switch providers.

If every attempted backend fails, Octopus maps rate-limit, overload, invalid-request, and generic backend failures into the appropriate endpoint-specific error shape.

If no catalog entry can satisfy the request, the Anthropic endpoint returns an invalid-request error and the OpenAI endpoint returns HTTP `422`.

## API compatibility

### Endpoints

| Method | Path | Shape |
|---|---|---|
| `POST` | `/v1/messages` | Anthropic Messages API |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions API |
| `GET` | `/v1/models` | OpenAI model list generated from the catalog |

Request bodies are limited to 32 MiB.

### Anthropic example

```bash
curl http://127.0.0.1:8787/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: local' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'X-Octopus-Session-ID: demo-anthropic' \
  -d '{
    "model": "router",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Explain prompt caching briefly."}]
  }'
```

Streaming uses the usual `"stream": true` request field and Anthropic SSE events.

### OpenAI example

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer local' \
  -H 'X-Octopus-Session-ID: demo-openai' \
  -d '{
    "model": "router",
    "messages": [{"role": "user", "content": "Hello from Octopus."}]
  }'
```

OpenAI buffered usage includes `prompt_tokens_details.cached_tokens` when the provider reports cache reads.

### Supported content

- Text system, user, and assistant content.
- Base64 images accepted by the Anthropic endpoint.
- OpenAI text and data-URL image parts.
- Tool definitions and assistant tool calls.
- Parallel Anthropic tool results, including text and images.
- Anthropic thinking blocks and signatures.
- Streaming text and tool-input deltas, plus Anthropic terminal usage, stop reasons, and refusals.

This is a compatibility gateway, not a claim of complete coverage of every field in either upstream API. Unsupported request content is rejected explicitly rather than silently reinterpreted.

## Configuration reference

Octopus parses YAML with unknown-field rejection, so misspelled settings fail fast.

### `server`

| Field | Required | Description |
|---|---:|---|
| `addr` | Yes | Loopback `host:port`, such as `127.0.0.1:8787`, `localhost:8787`, or `[::1]:8787`. Non-loopback addresses are rejected. |
| `auth_token_env` | No | Names the environment variable holding a shared secret for the routing endpoints. The token itself is never written to the config file. Omitted or empty means no authentication. |

When `auth_token_env` is set, requests must present the secret as either `x-api-key` or `Authorization: Bearer <token>`; both are accepted because Anthropic and OpenAI clients each send their own. An `Authorization` header carrying the bare token without the `Bearer ` prefix is also accepted. Requests without a valid secret receive `401` in the error shape of the endpoint they called.

Naming a variable that is not set in the environment resolves to an empty token, which disables authentication rather than rejecting every request. Octopus logs a warning at startup when this happens — `auth token variable is empty; routing endpoints are UNAUTHENTICATED` — because the alternative is a security control that silently is not there. Confirm the variable is exported in the environment the router actually runs in: for the menu bar app, that is the environment Launch Services gives the app, not your shell, so a token exported in `.zshrc` will not be visible to it.

Saving from Settings rewrites the file through the config marshaller, which always emits `auth_token_env` even when empty. A line reading `auth_token_env: ""` appearing after a save is expected and matches how the sibling `server` fields are written.

### `classifier`

The entire section is optional.

| Field | Required when section is used | Description |
|---|---:|---|
| `model` | Yes | Catalog-style `provider/model` ID used for classification. |
| `max_tokens` | Yes | Positive response-token limit; `256` is usually sufficient. |
| `timeout` | Yes | Positive Go duration such as `10s`. |

The classifier provider must resolve at runtime. Failure falls back to the default profile.

### `weights`

| Field | Range | Description |
|---|---:|---|
| `quality` | `>= 0` | Relative influence of catalog quality. |
| `cost` | `>= 0` | Relative influence of request cost. |
| `speed` | `>= 0` | Relative influence of catalog speed. |

Values must be finite and cannot all be zero.

### `routing`

The section is optional; these defaults are applied when it is omitted.

| Field | Default | Description |
|---|---:|---|
| `session_sticky` | `true` | Keep a conversation on its last successful eligible model. |
| `session_ttl` | `1h` | Lifetime of model affinity. Must not be negative. |
| `cache_aware` | `true` | Include expected cache writes/reads in cost scoring. |
| `max_attempts` | `3` | Maximum backends one request may try. Every failure consumes an attempt except a malformed request and a cancelled request, which stop immediately. Must not be negative; `0` is treated as omitted. |

### `providers`

Provider map keys are arbitrary names used as the prefix of catalog IDs. `kind` selects the harness client implementation.

| `kind` | Typical backends | Base URL support |
|---|---|---|
| `anthropic` | Anthropic and Anthropic-shaped gateways | Optional override |
| `openai` | OpenAI, Ollama, LM Studio, mlx-lm, compatible servers | Optional override |
| `gemini` | Google Gemini | Provider default |
| `qwen` | Alibaba DashScope/Qwen | Optional override |

Each provider supports these fields:

| Field | Description |
|---|---|
| `kind` | Client kind. When omitted, defaults to the provider map key. |
| `api_key_env` | Environment variable containing the provider credential. |
| `api_key` | Inline credential. Takes precedence; use only in ignored local configuration. |
| `base_url` | Provider endpoint override. A local endpoint may use this without a key. |

At least one of `api_key`, `api_key_env`, or `base_url` must be configured. If an environment variable is named but empty and there is no base URL, registry initialization fails.

Examples:

```yaml
providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"

  openai:
    api_key_env: "OPENAI_API_KEY"

  local:
    kind: openai
    base_url: "http://127.0.0.1:8080/v1"

  coding_gateway:
    kind: anthropic
    api_key_env: "CODING_GATEWAY_KEY"
    base_url: "https://gateway.example.com/anthropic"
```

Octopus resolves configured provider credentials at startup and then removes ambient `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_API_KEY` from its process environment. This prevents an Anthropic SDK ambient token from leaking into requests for another Anthropic-shaped backend.

### `catalog`

Every entry requires a unique `provider/model` ID. The provider prefix must exist in `providers`.

| Field | Constraints | Description |
|---|---:|---|
| `id` | Unique `provider/model` | Routing and registry identifier. |
| `quality` | `0..1` | Relative model quality. |
| `cost_per_mtok_in` | `>= 0` | Input price per million tokens. |
| `cost_per_mtok_out` | `>= 0` | Output price per million tokens. |
| `speed` | `0..1` | Relative throughput/latency score. |
| `caps.tools` | Boolean | Tool/function-call support. |
| `caps.vision` | Boolean | Image-input support. |
| `caps.reasoning` | Boolean | Native extended reasoning support. |
| `caps.max_context` | Positive integer | Maximum combined context capacity. |
| `caps.max_output_tokens` | Optional, `>= 0` | Maximum response tokens the model accepts. Omitted or `0` means unconstrained. |

`caps.max_output_tokens` is a hard eligibility filter alongside `caps.max_context`: a model whose declared output limit is below the estimated response size is removed before scoring, rather than being discovered at the backend. The estimate is floored at the client's requested `max_tokens` (or `1024` when unset), so a client that habitually requests a generous `max_tokens` it never fills can exclude models that would in practice have coped.

Catalog prices and capabilities are operator-maintained. Keep them synchronized with provider documentation; Octopus does not fetch pricing or model metadata automatically.

## Deployment and security

### Routing endpoints

The routing endpoints are unauthenticated by default. The only thing standing between them and a caller is the loopback bind, so **any process running as any user on the machine can reach them and spend your provider credits**. That is an acceptable posture for a single-user workstation and a poor one for a shared or multi-tenant host. Set [`server.auth_token_env`](#server) to require a shared secret; it is off by default because turning it on breaks every already-configured client.

Loopback binding is a real control but a narrow one. It stops remote network access and nothing else — not other local users, not other applications, not a compromised dependency in an unrelated project on the same machine.

- `server.addr` is validated as loopback-only, and validation rejects a non-loopback address precisely because inbound requests may be unauthenticated.
- Do not expose port `8787` through a public tunnel, container port, or reverse proxy without adding authentication at that boundary.
- Keep provider keys in environment variables or an ignored `config.yaml`.
- Never commit inline `api_key` values.
- Rotate a provider key immediately if it appears in logs, shell history, or version control.

### Settings server

The settings server binds to a random loopback port and enforces two controls on every write. They defend against different attacks and neither substitutes for the other.

**A literal-loopback `Host` check** is what closes DNS rebinding. A rebinding attacker serves a page from a hostname they control, then repoints that hostname at `127.0.0.1`. Comparing `Origin` against `Host` proves nothing there, because both are attacker-controlled and agree with each other. Only the literal address distinguishes the real local UI from a hostile page, so writes are rejected unless `Host` names a loopback IP or `localhost`. Note that the CSRF token contributes nothing against rebinding: the attacker's page origin *is* the target origin, so its `GET /` is same-origin and it can simply read the token out of the served HTML.

**A per-process CSRF token** closes classic cross-site request forgery, which the `Host` check does not touch. An attacker page that POSTs directly to `http://127.0.0.1:PORT` sends a valid loopback `Host` and sails through the first check. There, however, the page is genuinely cross-origin: no CORS headers are served, so it cannot read the response to `GET /` and harvest the token, and `frame-ancestors 'none'` with `X-Frame-Options: DENY` closes the framing route to the same end. Without the token the forged write is rejected.

Writes additionally require an `X-Octopus-Settings: 1` header and a JSON content type, and any `Origin` present must match the request `Host`. Responses carry a restrictive `Content-Security-Policy`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, and `Cache-Control: no-store`.

The HTTP server uses:

- 10-second read-header timeout.
- 60-second request read timeout.
- 120-second idle timeout.
- No write timeout, allowing long-lived SSE responses.
- 32 MiB maximum inbound request body.

For a persistent local installation, run the binary under your operating system's service manager and send `SIGTERM` during upgrades. Rebuilding alone does not replace an already-running process.

## Observability and troubleshooting

The macOS Settings **Insights** section is the primary view for aggregate usage and estimated savings. It supports 7-day, 30-day, 90-day, and one-year ranges.

Octopus emits structured `slog` text records to standard error. Useful entries include:

- `octopus listening`: successful startup and bind address.
- `routing decision`: chosen model, reason, inferred profile, and eligible models.
- `provider ... failed`: candidate failure and fallback attempt.
- `using fallback model`: successful alternate backend.
- `tool schema normalized`: provider compatibility rewrite.
- `prompt cache usage`: cache creation and cache-read input tokens.
- `request handled`: endpoint, final model, requested model, stream mode, routing reason, and elapsed time.

### Octopus does not start

- Check that `server.addr` is loopback and in `host:port` form.
- Check for unknown or misspelled YAML fields.
- Ensure all catalog provider prefixes exist in `providers`.
- Ensure every configured cloud-provider environment variable is set.
- Check whether another process already owns port `8787`.

### Claude Code cannot connect

- Confirm `curl http://127.0.0.1:8787/v1/models` succeeds.
- Set `ANTHROPIC_BASE_URL` without appending `/v1/messages`; Claude Code adds the API path.
- Use a non-empty `ANTHROPIC_AUTH_TOKEN` in the Claude Code process.
- Ensure Octopus is still running after rebuilding it.

### Cache reads stay at zero

- Use an Anthropic-kind backend that supports prompt caching.
- Keep the same Claude Code conversation and backend.
- Keep turns within the requested `5m` or `1h` TTL.
- Avoid changing system blocks, tool schemas, or other content before the cache breakpoint.
- Ensure the prefix meets the provider's minimum token requirement.
- Check that fallback did not move the session to another provider.

### Every request uses the same model

- Sticky routing intentionally keeps a valid session on its successful model.
- Use a different `X-Octopus-Session-ID`, wait for `session_ttl`, restart Octopus, or disable `routing.session_sticky` to compare fresh routes.

## Local and mixed-provider recipes

### Local OpenAI-compatible server

```yaml
server:
  addr: "127.0.0.1:8787"

weights:
  quality: 0.4
  cost: 0.4
  speed: 0.2

routing:
  session_sticky: false
  cache_aware: false

providers:
  local:
    kind: openai
    base_url: "http://127.0.0.1:8080/v1"

catalog:
  - id: "local/my-model"
    quality: 0.60
    cost_per_mtok_in: 0
    cost_per_mtok_out: 0
    speed: 0.85
    caps: {tools: true, vision: false, reasoning: false, max_context: 32768}
```

Omit `classifier` to avoid any classification provider call.

### Mixed local and cloud routing

```yaml
classifier:
  model: "anthropic/claude-haiku"
  max_tokens: 256
  timeout: "10s"

weights:
  quality: 0.5
  cost: 0.4
  speed: 0.1

providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"
  local:
    kind: openai
    base_url: "http://127.0.0.1:8080/v1"

catalog:
  - id: "anthropic/claude-capable"
    quality: 0.95
    cost_per_mtok_in: 5
    cost_per_mtok_out: 25
    speed: 0.55
    caps: {tools: true, vision: true, reasoning: true, max_context: 200000}

  - id: "local/fast-model"
    quality: 0.65
    cost_per_mtok_in: 0
    cost_per_mtok_out: 0
    speed: 0.90
    caps: {tools: true, vision: false, reasoning: false, max_context: 32768}
```

The free local model has the strongest cost score, while high-difficulty tasks can apply the quality floor and move to the cloud model.

## Benchmarking

The included benchmark compares Octopus with a direct OpenAI-compatible provider call.

```bash
python3 scripts/benchmark.py
python3 scripts/benchmark.py --streaming
python3 scripts/benchmark.py --runs 5 --concurrency 3
python3 scripts/benchmark.py --output results.txt
python3 scripts/benchmark.py --router-only
```

Edit `DIRECT_BASE`, `DIRECT_API_KEY`, and `DIRECT_MODEL` near the top of the script for the direct comparison target. The report includes latency percentiles, time to first token for streaming, throughput, and the final routed model.

## Development

```bash
make build       # build ./octopus
make app         # build dist/Octopus.app on macOS
make installer   # build, sign, notarize, and staple a macOS .pkg
make release v0.1.0 # publish a versioned GitHub release and installer
make notary-profile # store notarization credentials in Keychain (one-time)
make open-app    # build and launch the macOS app
make test        # GOWORK=off go test ./...
make test-race   # race detector across all packages
make vet         # go vet ./...
make tidy        # go mod tidy and go mod verify
make run         # build and run (menu-bar app on macOS)
make clean       # remove the generated binary and app bundle
```

Tests are hermetic and do not make live provider calls.

### Repository layout

```text
octopus/
├── cmd/octopus/       Process entry point and HTTP server lifecycle
├── config/            YAML schema, loading, defaults, and validation
├── desktop/           Live router lifecycle and validated reloads
├── menubar/           Native macOS status-item integration
├── settings/          Settings API, persistence, and web interface
├── packaging/         macOS app-bundle metadata
├── registry/          Provider construction and provider/model resolution
├── router/            Classification, token estimation, scoring, affinity
├── anthropicio/       Anthropic request decoder and response encoders
├── openaiio/          OpenAI request decoder and response encoders
├── server/            HTTP endpoints, fallback, normalization, observation
├── scripts/           Build and benchmark utilities
├── config.example.yaml
├── Makefile
└── octopus.png
```

The shared provider abstraction and implementations live in [`github.com/sausheong/harness`](https://github.com/sausheong/harness). Octopus currently requires harness `v0.3.4`.

## Releasing

For maintainers publishing signed builds. Using Octopus requires none of this.

### One-time setup

Install the Developer ID Application and Developer ID Installer certificates in your Keychain, then set:

```bash
export APPLE_ID="developer@example.com"
export TEAM_ID="ABCDE12345"
export APP_SIGN_ID="Developer ID Application: Example Company (ABCDE12345)"
export PKG_SIGN_ID="Developer ID Installer: Example Company (ABCDE12345)"
export KEYCHAIN_PROFILE="octopus-notary"
```

Store the notarization credentials once — Apple prompts for the app-specific password, so it never enters your environment or shell history:

```bash
make notary-profile
```

Never commit these values or an app-specific password.

### Build an installer

```bash
make installer
```

This builds `Octopus.app`, signs it with the hardened runtime and a trusted timestamp, checks the signing team against `TEAM_ID`, packages it to install into `/Applications`, notarizes and staples the result, and validates it. Output is `dist/Octopus-<version>.pkg`, versioned from `packaging/Info.plist`.

### Publish a release

The worktree must be clean — a tag has to identify one complete commit — and `gh auth status` must succeed.

```bash
make release v0.2.0
```

The version must be exactly `vX.Y.Z`; prerelease suffixes are rejected. The target verifies the environment, runs the test suite, bumps `packaging/Info.plist`, pushes the commit and an annotated tag, creates a draft release, builds and notarizes the installer, uploads it, and publishes.

It pushes to `origin`, so check your branch first. The only commit it creates is the version bump; it never sweeps up outstanding work.

**If it fails partway**, the release stays a draft. Fix the problem and run the same command to resume. A published version cannot be overwritten.

Release notes are generated from up to eight recent commit subjects. That is a starting point, not a changelog — edit them afterwards with `gh release edit <tag> --notes-file <file>`.

## License

MIT. See [LICENSE](LICENSE).
