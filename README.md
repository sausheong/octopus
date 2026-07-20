# llmrouter

A lightweight LLM routing proxy that runs locally on your machine. It exposes both Anthropic-compatible (`POST /v1/messages`) and OpenAI-compatible (`POST /v1/chat/completions`, `GET /v1/models`) endpoints, and automatically routes each request to the best available model based on task complexity, cost, and speed.

Point Cursor, Continue.dev, Open WebUI, or any OpenAI-compatible client at `http://localhost:8787` and llmrouter handles the rest.

---

## How it works

Every inbound request goes through several stages before a provider is called:

```
Client request
      │
      ▼
┌─────────────┐
│   Decoder   │  Parse Anthropic or OpenAI wire format → internal types
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Classifier │  Ask a cheap model: how hard is this? needs vision/tools?
│  (optional) │  Short single-turn requests skip this entirely.
└──────┬──────┘
       │  TaskProfile{difficulty, needs_vision, needs_tools, est_tokens}
       ▼
┌─────────────┐
│   Scorer    │  Filter catalog by capability, rank by weighted score
│             │  Cache-aware: adjusts cost for prompt cache hits/misses
└──────┬──────┘
       │  Decision{chosen, eligible[], scores}
       ▼
┌─────────────┐
│  Session    │  Sticky affinity: keep long conversations on the same model
│  Affinity   │  to preserve prompt cache warmth
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Registry   │  Resolve provider/model → harness LLMProvider
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Provider   │  ChatStream → SSE or buffered response (with fallback)
└─────────────┘
```

### Classifier

The classifier is a cheap, fast model that reads a bounded window of recent conversation context and produces a `TaskProfile`:

- `difficulty`: trivial / low / medium / high
- `needs_reasoning`: requires multi-step logic, math, or planning
- `needs_vision`: request depends on understanding images
- `needs_tools`: request expects function calling
- `est_tokens_in` / `est_tokens_out`: estimated token footprint

Two cases skip the classifier entirely:
- **Trivial short-circuit**: if the last user turn is under 500 bytes, has no images, and is single-turn (no prior assistant messages), a `TrivialProfile` is used immediately — no extra round-trip.
- **No classifier configured**: if `classifier.model` is empty, `DefaultProfile` (conservative: high difficulty, needs reasoning) is always used. Safe for pure-local setups.

Classifier failures (timeout, parse error, unresolvable provider) fall back to `DefaultProfile` — a classifier hiccup never breaks routing.

### Scorer

The scorer applies three stages to the catalog:

1. **Hard capability filter**: eliminates models missing required tool or vision support, or whose context window is too small for the request. The context estimate is a deterministic lower bound from the full request (system prompt, messages, tool schemas, images) floored above the classifier's LLM-based guess.
2. **Quality floor for hard tasks**: if difficulty is `high`, models below quality 0.85 are dropped — but only when at least one model clears the floor, so the set is never emptied on quality alone.
3. **Weighted balanced score**: remaining models are ranked by `quality × wq + cost_efficiency × wc + speed × ws`, with a modest bonus for reasoning-capable models when the profile benefits from it.

Free (zero-cost) models always receive the maximum cost score so they are genuinely preferred over paid models for cost-heavy weight configurations. The top-scoring model becomes `Decision.Chosen`; the full scored list is kept in `Decision.Eligible` for fallback.

### Cache-aware scoring

When `routing.cache_aware: true` (the default), the scorer adjusts the effective input cost for each model based on whether a prompt cache hit is expected:

- **Cache read**: 0.10× input cost — a cached prefix is very cheap
- **Cache write (5 min TTL)**: 1.25× input cost
- **Cache write (1 hour TTL)**: 2.00× input cost

This means a model that already has your conversation prefix cached scores as dramatically cheaper than one that doesn't, naturally routing follow-up turns to the same provider.

### Session affinity

When `routing.session_sticky: true` (the default), the router records which model handled each conversation and pins subsequent turns to the same model for the duration of `session_ttl` (default 1 hour). This keeps prompt caches warm across turns.

Session identity is derived from `metadata.user_id` in the request, the `X-LLMRouter-Session-ID` HTTP header (takes precedence), or a deterministic SHA-256 hash of the conversation's stable prefix (system prompt + tools + first user turn).

### Provider fallback

If a provider returns an error — either immediately from `ChatStream` or as an `EventError` on the channel — the server walks `Decision.Eligible` in score order and retries each candidate. For buffered responses, the full collection must fail before falling back. Once SSE headers are written the client owns the stream and fallback is no longer possible.

### Registry

The registry maps provider names to `harness.LLMProvider` instances. Four wire protocols are supported:

| `kind`      | Backend                                      |
|-------------|----------------------------------------------|
| `anthropic` | Anthropic Messages API (also DeepSeek, MiniMax, etc.) |
| `openai`    | OpenAI Chat Completions API (also Ollama, LM Studio, mlx-lm) |
| `gemini`    | Google Gemini                                |
| `qwen`      | Qwen / Alibaba DashScope                     |

Any provider name can be paired with any `kind`, which allows multiple backends to coexist under distinct names (e.g. several Anthropic-compatible APIs each with their own key and base URL).

---

## Package structure

```
llmrouter/
├── cmd/router/       Entry point — loads config, wires everything, starts HTTP server
├── config/           YAML config: load, validate, types
├── registry/         Builds one LLMProvider per configured provider; resolves provider/model IDs
├── router/           Classifier, scorer, session affinity, routing decision
│   ├── classifier.go   Calls classifier model, parses TaskProfile
│   ├── profile.go      TaskProfile, profiles, EstimateRequestTokens
│   ├── scorer.go       Capability filter, quality floor, cache-aware weighted scoring
│   ├── router.go       Route() orchestration, session affinity, Observe()
│   └── turn.go         LastUserTurn — finds the last genuine user message
├── anthropicio/       Anthropic wire format: decode requests, encode SSE and buffered responses
├── openaiio/          OpenAI wire format: decode requests, encode SSE chunks and completions
├── server/            HTTP handlers for /v1/messages, /v1/chat/completions, /v1/models
└── scripts/           Benchmark and utility scripts
```

---

## Setup

### Prerequisites

- Go 1.25+
- API keys for any cloud providers you want to use
- For local inference: mlx-lm, Ollama, or LM Studio running on your machine

### Build

```bash
git clone https://github.com/sausheong/llmrouter
cd llmrouter
go build -o llmrouter ./cmd/router
```

Or use make:

```bash
make build   # compile → ./llmrouter
make test    # run all tests
make run     # build and start with config.yaml
make help    # list all targets
```

### Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`. The minimum working config is:

```yaml
server:
  addr: "127.0.0.1:8787"

weights:
  quality: 0.5
  cost: 0.3
  speed: 0.2

providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"

catalog:
  - id: "anthropic/claude-haiku-3-5-20241022"
    quality: 0.70
    cost_per_mtok_in: 1.0
    cost_per_mtok_out: 5.0
    speed: 0.95
    caps: { tools: true, vision: true, reasoning: false, max_context: 200000 }
```

### Run

```bash
export ANTHROPIC_API_KEY=sk-ant-...
./llmrouter
```

Or with a custom config path:

```bash
./llmrouter -config /path/to/config.yaml
```

---

## Configuration reference

### `server`

| Field  | Description                        |
|--------|------------------------------------|
| `addr` | Loopback listen address (e.g. `127.0.0.1:8787`). Non-loopback binding is rejected — inbound requests are unauthenticated. |

### `classifier` (optional)

| Field        | Description                                                        |
|--------------|--------------------------------------------------------------------|
| `model`      | Provider/model ID for the classifier. Empty = always use DefaultProfile (no cloud call). |
| `max_tokens` | Max tokens for classifier response (256 is plenty).               |
| `timeout`    | Per-request classifier timeout (e.g. `10s`).                     |

### `weights`

Balanced-score knobs. Need not sum to 1 — the scorer normalises them.

| Field     | Description                                       |
|-----------|---------------------------------------------------|
| `quality` | Weight for model quality score (0–1 in catalog).  |
| `cost`    | Weight for cost efficiency (inverse of request $). |
| `speed`   | Weight for model speed score (0–1 in catalog).     |

### `routing` (optional)

| Field            | Default | Description                                                        |
|------------------|---------|--------------------------------------------------------------------|
| `session_sticky` | `true`  | Pin conversation turns to the same model within the TTL.          |
| `session_ttl`    | `1h`    | How long a session pin lasts (e.g. `30m`, `2h`).                 |
| `cache_aware`    | `true`  | Adjust model cost scores based on expected prompt cache hits.      |

Set both to `false` for minimum per-request overhead when you don't need conversation affinity.

### `providers`

Each entry is a named provider. The name is the prefix of catalog IDs (e.g. provider `anthropic` → catalog ID `anthropic/claude-...`).

| Field         | Description                                                                 |
|---------------|-----------------------------------------------------------------------------|
| `kind`        | Wire protocol: `anthropic`, `openai`, `gemini`, `qwen`. Defaults to the provider name. |
| `api_key`     | Literal API key (gitignored config only).                                   |
| `api_key_env` | Name of env var holding the API key.                                        |
| `base_url`    | Override endpoint URL. Required for local servers; optional for cloud providers. |

At least one of `api_key`, `api_key_env`, or `base_url` must be set. Local servers that don't require authentication only need `base_url`.

### `catalog`

Ordered list of candidate models. The scorer considers them in order for tie-breaking.

| Field               | Description                                              |
|---------------------|----------------------------------------------------------|
| `id`                | `provider/model` — provider must be in `providers`.     |
| `quality`           | Subjective quality score, 0–1.                          |
| `cost_per_mtok_in`  | Cost per million input tokens (USD). 0 = free/local.   |
| `cost_per_mtok_out` | Cost per million output tokens (USD).                   |
| `speed`             | Relative speed score, 0–1.                              |
| `caps.tools`        | Supports function/tool calling.                         |
| `caps.vision`       | Supports image input.                                   |
| `caps.reasoning`    | Supports extended thinking / reasoning mode.            |
| `caps.max_context`  | Maximum context window in tokens (required, must be > 0). |

---

## Performance

Measured overhead vs calling DeepSeek directly (3 runs × 5 prompt types, buffered):

| Prompt type | Router overhead | Notes |
|-------------|----------------|-------|
| trivial     | ~300ms         | Short-circuit fires; no classifier call |
| short       | ~300ms         | Classifier call adds ~200ms |
| medium      | ~130ms         | Similar model chosen; minimal overhead |
| code        | ~0ms           | Router often matches or beats direct |
| long        | ~350ms         | Classifier latency dominates |
| **average** | **~236ms**     | |

Streaming TTFT overhead for trivial requests is typically **< 100ms**.

To minimise overhead set `routing.session_sticky: false` and `routing.cache_aware: false` — this removes the session hash, mutex, and goroutine from the hot path.

---

## Recipes

### Pure-local with mlx-lm (macOS)

Start mlx-lm:
```bash
mlx_lm.server --model mlx-community/Qwen3-8B-4bit --port 8080
```

`config.yaml`:
```yaml
server:
  addr: "127.0.0.1:8787"

# No classifier — no cloud call required
routing:
  session_sticky: false
  cache_aware: false

weights:
  quality: 0.5
  cost: 0.3
  speed: 0.2

providers:
  mlx:
    kind: openai
    base_url: "http://localhost:8080/v1"

catalog:
  - id: "mlx/Qwen3-8B-4bit"
    quality: 0.60
    cost_per_mtok_in: 0.0
    cost_per_mtok_out: 0.0
    speed: 0.85
    caps: { tools: false, vision: false, reasoning: false, max_context: 32768 }
```

### Mixed local + cloud with smart routing

Use a cheap classifier, route trivial tasks to local MLX, hard tasks to a capable cloud model:

```yaml
classifier:
  model: "anthropic/claude-haiku-3-5-20241022"
  max_tokens: 256
  timeout: "10s"

weights:
  quality: 0.5
  cost: 0.4
  speed: 0.1

providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"
  mlx:
    kind: openai
    base_url: "http://localhost:8080/v1"

catalog:
  - id: "anthropic/claude-opus-4-0-20250514"
    quality: 0.98
    cost_per_mtok_in: 15.0
    cost_per_mtok_out: 75.0
    speed: 0.4
    caps: { tools: true, vision: true, reasoning: true, max_context: 1000000 }
  - id: "anthropic/claude-haiku-3-5-20241022"
    quality: 0.70
    cost_per_mtok_in: 1.0
    cost_per_mtok_out: 5.0
    speed: 0.95
    caps: { tools: true, vision: true, reasoning: false, max_context: 200000 }
  - id: "mlx/Qwen3-8B-4bit"
    quality: 0.60
    cost_per_mtok_in: 0.0
    cost_per_mtok_out: 0.0
    speed: 0.85
    caps: { tools: false, vision: false, reasoning: false, max_context: 32768 }
```

With `cost` weight at 0.4, the free MLX model wins for trivial tasks. Hard tasks (difficulty=high) apply the quality floor (≥0.85) and route to Opus or Haiku.

### Connecting a client

Any OpenAI-compatible client works. Set the base URL to `http://localhost:8787` and use any non-empty API key (llmrouter doesn't authenticate inbound requests).

**curl:**
```bash
curl http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer local" \
  -d '{"model":"any","messages":[{"role":"user","content":"hello"}]}'
```

**Session pinning** (keeps conversation on the same model):
```bash
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer local" \
  -H "X-LLMRouter-Session-ID: my-session-123" \
  -d '{"model":"any","messages":[...]}'
```

**Cursor / VS Code Continue / Open WebUI:**
Set the OpenAI base URL to `http://localhost:8787`. The model name you specify is ignored — llmrouter always routes to the best available model.

**List available models:**
```bash
curl http://localhost:8787/v1/models
```

---

## Benchmarking

```bash
python3 scripts/benchmark.py                          # buffered, 3 runs
python3 scripts/benchmark.py --streaming              # streaming + TTFT
python3 scripts/benchmark.py --runs 5 --concurrency 3 --output results.txt
python3 scripts/benchmark.py --router-only            # skip direct comparison
```

The script compares llmrouter against a direct provider call, reporting p50/p95 latency, TTFT (streaming), throughput, and which model the router chose for each prompt type.

Edit `DIRECT_BASE`, `DIRECT_API_KEY`, and `DIRECT_MODEL` at the top of the script to benchmark against any OpenAI-compatible provider.

---

## Development

```bash
make test        # run all tests (GOWORK=off)
make test-race   # with race detector
make vet         # go vet
make tidy        # go mod tidy + verify
```

Tests are hermetic — no live API calls. Tests tagged `//go:build live` hit real providers and are excluded from the default run.
