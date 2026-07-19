# llmrouter

A lightweight LLM routing proxy that runs locally on your machine. It exposes both Anthropic-compatible (`POST /v1/messages`) and OpenAI-compatible (`POST /v1/chat/completions`, `GET /v1/models`) endpoints, and automatically routes each request to the best available model based on task complexity, cost, and speed.

Point Cursor, Continue.dev, Open WebUI, or any OpenAI-compatible client at `http://localhost:8787` and llmrouter handles the rest.

---

## How it works

Every inbound request goes through three stages before a provider is called:

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
└──────┬──────┘
       │  Decision{chosen, eligible[], scores}
       ▼
┌─────────────┐
│  Registry   │  Resolve provider/model → harness LLMProvider
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Provider   │  ChatStream → SSE or buffered response
└─────────────┘
```

### Classifier

The classifier is a cheap, fast model (typically Haiku) that reads a bounded window of recent conversation context and produces a `TaskProfile`:

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

The scorer applies two passes to the catalog:

1. **Hard capability filter**: eliminates models missing required tool or vision support, or whose context window is too small. Reasoning support is a preference, so a classifier failure cannot make ordinary local models unavailable.
2. **Quality floor for hard tasks**: if difficulty is `high`, models below quality 0.85 are dropped — but only when at least one model clears the floor, so the set is never emptied on quality alone.
3. **Weighted balanced score**: remaining models are ranked by `quality × wq + cost_efficiency × wc + speed × ws`, with a modest bonus for reasoning-capable models when the profile benefits from it. Weights are normalised so they need not sum to 1.

The top-scoring model (earliest in catalog order on ties) becomes `Decision.Chosen`. The full scored list is kept in `Decision.Eligible` for fallback.

### Provider fallback

If `ChatStream` fails on the chosen model (rate limit, server down, network error), the server walks `Decision.Eligible` in score order and retries each candidate until one succeeds. Fallback only happens before headers are written — once a streaming response starts, the client owns the channel.

### Registry

The registry maps provider names to `harness.LLMProvider` instances. Four wire protocols are supported:

| `kind`      | Backend                                      |
|-------------|----------------------------------------------|
| `anthropic` | Anthropic Messages API (also DeepSeek, etc.) |
| `openai`    | OpenAI Chat Completions API                  |
| `gemini`    | Google Gemini                                |
| `qwen`      | Qwen / Alibaba DashScope                     |

Any provider name can be paired with any `kind`, which allows multiple Anthropic-compatible backends to coexist under distinct names.

---

## Package structure

```
llmrouter/
├── cmd/router/       Entry point — loads config, wires everything, starts HTTP server
├── config/           YAML config: load, validate, types
├── registry/         Builds one LLMProvider per configured provider; resolves provider/model IDs
├── router/           Classifier, scorer, profile types, routing decision
│   ├── classifier.go   Calls classifier model, parses TaskProfile
│   ├── profile.go      TaskProfile, DefaultProfile, TrivialProfile, isTrivial
│   ├── scorer.go       Capability filter, quality floor, weighted scoring
│   ├── router.go       Route() orchestration
│   └── turn.go         LastUserTurn — finds the last genuine user message
├── anthropicio/       Anthropic wire format: decode requests, encode SSE and buffered responses
├── openaiio/          OpenAI wire format: decode requests, encode SSE chunks and completions
└── server/            HTTP handlers for /v1/messages, /v1/chat/completions, /v1/models
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
| `addr` | Listen address (e.g. `127.0.0.1:8787`) |

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

### `default_model` (deprecated)

Accepted only for compatibility with older files and ignored. When no catalog entry satisfies the required capabilities and context size, the router returns a clear request error instead of sending the request to an unsuitable fallback.

### `providers`

Each entry is a named provider. The name is the prefix of catalog IDs (e.g. provider `anthropic` → catalog ID `anthropic/claude-...`).

| Field         | Description                                                                 |
|---------------|-----------------------------------------------------------------------------|
| `kind`        | Wire protocol: `anthropic`, `openai`, `gemini`, `qwen`. Defaults to the provider name. |
| `api_key`     | Literal API key (gitignored config only).                                   |
| `api_key_env` | Name of env var holding the API key.                                        |
| `base_url`    | Override endpoint URL. Required for local servers; optional for cloud providers. |

At least one of `api_key`, `api_key_env`, or `base_url` must be set.

### `catalog`

Ordered list of candidate models. The scorer considers them in order for tie-breaking.

| Field               | Description                                              |
|---------------------|----------------------------------------------------------|
| `id`                | `provider/model` — provider must be in `providers`.     |
| `quality`           | Subjective quality score, 0–1.                          |
| `cost_per_mtok_in`  | Cost per million input tokens (USD).                    |
| `cost_per_mtok_out` | Cost per million output tokens (USD).                   |
| `speed`             | Relative speed score, 0–1.                              |
| `caps.tools`        | Supports function/tool calling.                         |
| `caps.vision`       | Supports image input.                                   |
| `caps.reasoning`    | Supports extended thinking / reasoning mode.            |
| `caps.max_context`  | Maximum context window in tokens.                       |

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

# No classifier — always use DefaultProfile, no cloud call required
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

Use Haiku as a cheap classifier, route trivial tasks to local MLX, hard tasks to Opus:

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

### Connecting a client

Any OpenAI-compatible client works. Set the base URL to `http://localhost:8787` and use any non-empty API key (llmrouter doesn't authenticate inbound requests).

**curl:**
```bash
curl http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer local" \
  -d '{"model":"any","messages":[{"role":"user","content":"hello"}]}'
```

**Cursor / VS Code Continue / Open WebUI:**
Set the OpenAI base URL to `http://localhost:8787` in the extension settings. The model name you set in the client is ignored — llmrouter always routes to the best available model.

**List available models:**
```bash
curl http://localhost:8787/v1/models
```

---

## Development

```bash
go test ./...      # run all tests
go vet ./...
go build ./...
```

Tests are hermetic — no live API calls. Tests tagged `//go:build live` hit real providers and are excluded from the default run.
