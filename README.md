# Octopus

![Octopus logo](octopus.png)

Octopus is a local LLM routing gateway for coding agents and OpenAI-compatible applications. It exposes Anthropic and OpenAI APIs on one loopback-only server, classifies each request, filters models by capability and context size, and chooses a backend using configurable quality, cost, and speed weights.

It is designed to work particularly well with Claude Code: Anthropic prompt-cache markers are preserved end to end, conversations remain on the backend that owns their cache, and cache creation/read usage is included in responses and logs.

## Contents

- [Quick start](#quick-start)
- [Claude Code setup](#claude-code-setup)
- [Prompt caching](#prompt-caching)
- [Routing behavior](#routing-behavior)
- [API compatibility](#api-compatibility)
- [Configuration reference](#configuration-reference)
- [Deployment and security](#deployment-and-security)
- [Observability and troubleshooting](#observability-and-troubleshooting)
- [Local and mixed-provider recipes](#local-and-mixed-provider-recipes)
- [Benchmarking](#benchmarking)
- [Development](#development)
- [Rename compatibility](#rename-compatibility)

## Highlights

- Anthropic-compatible `POST /v1/messages`, streaming and buffered.
- OpenAI-compatible `POST /v1/chat/completions`, streaming and buffered.
- OpenAI-compatible `GET /v1/models` generated from the configured catalog.
- Routing across Anthropic, OpenAI, Gemini, Qwen, local models, and compatible gateways.
- Optional low-cost classifier with a zero-call short circuit for trivial requests.
- Hard tool, vision, and context-window eligibility checks.
- Quality/cost/speed scoring with deterministic fallback order.
- Sticky conversation routing for prompt-cache continuity.
- Full Anthropic `cache_control` preservation, including `5m` and `1h` TTLs.
- Extended-thinking/reasoning mapping and thinking-block round trips.
- Tool calls, parallel tool results, images, streaming usage, and refusal propagation.
- Loopback-only binding, bounded request bodies, transport timeouts, and graceful shutdown.

## Quick start

### Requirements

- Go 1.25 or newer.
- An API key for each configured cloud provider, or a running local inference server.

### Install and build

```bash
git clone https://github.com/sausheong/octopus.git
cd octopus
make build
```

This builds `./octopus` from `./cmd/octopus`. The equivalent Go command is:

```bash
GOWORK=off go build -o octopus ./cmd/octopus
```

### Configure

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

### Run

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
./octopus
```

Use another configuration file with:

```bash
./octopus -config /path/to/config.yaml
```

The process logs `octopus listening` when it is ready. It handles `SIGINT` and `SIGTERM` by draining in-flight requests for up to 30 seconds.

## Claude Code setup

Run Octopus and Claude Code in separate terminals so the upstream provider credential and Claude Code's local gateway token are not confused.

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

`ANTHROPIC_AUTH_TOKEN` can be any non-empty value because Octopus does not authenticate inbound loopback requests. It never forwards this client token: upstream requests are built with credentials from `config.yaml`.

The `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` gateway configuration follows Anthropic's [Claude Code LLM gateway guidance](https://docs.anthropic.com/en/docs/claude-code/llm-gateway).

Claude Code may request a specific model name, but Octopus intentionally treats the inbound model as advisory and selects a catalog model itself.

### Verify Claude Code prompt caching

1. Enable `routing.session_sticky` and `routing.cache_aware` or omit them to use their `true` defaults.
2. Start a Claude Code session with a sufficiently large, stable prompt prefix.
3. Send at least two turns within the cache TTL.
4. Inspect the Octopus log for `prompt cache usage`.
5. Confirm a later response reports a positive `cache_read_input_tokens` value.

A zero cache count does not always mean forwarding failed. Providers impose minimum cacheable-prefix sizes, and a new or changed prefix may create a cache before it can be read.

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

1. `X-Octopus-Session-ID` HTTP header.
2. Legacy `X-LLMRouter-Session-ID` header.
3. Anthropic `metadata.user_id` or OpenAI `user` request field.
4. A deterministic SHA-256 identifier derived from the system prompt, tools, and first genuine user turn.

Explicit identifiers are hashed before being stored. The in-memory session table is concurrency-safe, expires entries, and has a hard size bound. Session state is process-local and is reset when Octopus restarts.

If the sticky model is no longer eligible because of tools, vision, or context size, normal scoring selects another model. A successful fallback becomes the new sticky model.

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
        ├── failure before response → next eligible model
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

- Failure while opening a stream triggers the next model.
- A closed stream or `EventError` before the first meaningful event triggers fallback.
- Buffered responses may fall back after a later collection failure because no client bytes have been written yet.
- Once streaming response bytes have been sent, Octopus cannot transparently switch providers.

If every eligible backend fails, Octopus maps rate-limit, overload, invalid-request, and generic backend failures into the appropriate endpoint-specific error shape.

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

Catalog prices and capabilities are operator-maintained. Keep them synchronized with provider documentation; Octopus does not fetch pricing or model metadata automatically.

## Deployment and security

Octopus intentionally has no inbound authentication. To reduce exposure:

- `server.addr` is validated as loopback-only.
- Do not expose port `8787` through a public tunnel, container port, or reverse proxy without adding authentication at that boundary.
- Keep provider keys in environment variables or an ignored `config.yaml`.
- Never commit inline `api_key` values.
- Rotate a provider key immediately if it appears in logs, shell history, or version control.

The HTTP server uses:

- 10-second read-header timeout.
- 60-second request read timeout.
- 120-second idle timeout.
- No write timeout, allowing long-lived SSE responses.
- 32 MiB maximum inbound request body.

For a persistent local installation, run the binary under your operating system's service manager and send `SIGTERM` during upgrades. Rebuilding alone does not replace an already-running process.

## Observability and troubleshooting

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
make test        # GOWORK=off go test ./...
make test-race   # race detector across all packages
make vet         # go vet ./...
make tidy        # go mod tidy and go mod verify
make run         # build and run with ./config.yaml
make clean       # remove the generated binary
```

Tests are hermetic and do not make live provider calls.

### Repository layout

```text
octopus/
├── cmd/octopus/       Process entry point and HTTP server lifecycle
├── config/            YAML schema, loading, defaults, and validation
├── registry/          Provider construction and provider/model resolution
├── router/            Classification, token estimation, scoring, affinity
├── anthropicio/       Anthropic request decoder and response encoders
├── openaiio/          OpenAI request decoder and response encoders
├── server/            HTTP endpoints, fallback, normalization, observation
├── scripts/           Benchmark utility
├── config.example.yaml
├── Makefile
└── octopus.png
```

The shared provider abstraction and implementations live in [`github.com/sausheong/harness`](https://github.com/sausheong/harness). Octopus currently requires harness `v0.3.4`.

## Rename compatibility

The project was previously named `llmrouter`.

- The repository and Go module are now `github.com/sausheong/octopus`.
- The executable is now `octopus`.
- The command package is now `./cmd/octopus`.
- `X-Octopus-Session-ID` replaces `X-LLMRouter-Session-ID`.
- The legacy session header remains accepted for existing clients.
