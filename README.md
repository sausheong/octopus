# Octopus

![Octopus logo](octopus.png)

Octopus is a local LLM routing gateway for coding agents and OpenAI-compatible applications. It exposes Anthropic and OpenAI APIs on one loopback-only server, classifies each request, filters models by capability and context size, and chooses a backend using configurable quality, cost, and speed weights.

It is designed to work particularly well with Claude Code: Anthropic prompt-cache markers are preserved end to end, conversations remain on the backend that owns their cache, and cache creation/read usage is included in responses and logs.

## Contents

- [Quick start](#quick-start)
- [macOS menu bar app](#macos-menu-bar-app)
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

## macOS menu bar app

### Build and launch

Requirements are macOS 13 or newer, Xcode Command Line Tools, and Go 1.25 or newer.

```bash
make app
open dist/Octopus.app
```

For regular use, copy `dist/Octopus.app` to `/Applications`. The locally built app is ad-hoc signed rather than distributed through the App Store; macOS may ask you to confirm the first launch.

### Signed installer

Release maintainers can build a Developer ID-signed, notarized installer package. The required Developer ID Application and Developer ID Installer certificates must be installed in the current user's Keychain.

Set the release environment variables:

```bash
export APPLE_ID="developer@example.com"
export TEAM_ID="ABCDE12345"
export APP_SIGN_ID="Developer ID Application: Example Company (ABCDE12345)"
export PKG_SIGN_ID="Developer ID Installer: Example Company (ABCDE12345)"
export KEYCHAIN_PROFILE="octopus-notary"
```

Store the notarization credentials once. Apple prompts securely for the app-specific password; it does not need to be placed in an environment variable or command history.

```bash
make notary-profile
```

Then create the installer:

```bash
make installer
```

The target builds `Octopus.app`, signs it with the hardened runtime and a trusted timestamp, verifies that its signing team matches `TEAM_ID`, creates a signed package that installs the app in `/Applications`, submits the package using `KEYCHAIN_PROFILE`, waits for notarization, staples the ticket, and validates the finished installer. The result is written to `dist/Octopus-<version>.pkg`, where the version comes from `packaging/Info.plist`.

`KEYCHAIN_PROFILE` supplies the stored Apple ID credentials during notarization. `APPLE_ID` and `TEAM_ID` are used when creating that profile; `make installer` also requires them so an incomplete release environment fails before signing begins. Never commit these values or an app-specific password to the repository.

### Publishing a release

Before publishing, review and commit every change that belongs in the release. The release command deliberately refuses to run with modified, staged, or untracked files because a Git tag must identify one complete, reproducible commit.

```bash
git status --short
git add <files-to-release>
git commit -m "Describe the release changes"
git status --short            # must produce no output
```

If every current file belongs in the release, `git add -A` can replace the selective `git add` command. Review the staged contents with `git diff --cached` before committing; do not include local configuration, credentials, build products, or unrelated work.

Confirm that GitHub CLI authentication and the Apple notarization profile are ready:

```bash
gh auth status
xcrun notarytool history --keychain-profile "$KEYCHAIN_PROFILE"
```

With all five release environment variables set, publish a three-component semantic version:

```bash
make release v0.1.0
```

The version must use the exact `vX.Y.Z` form; prerelease suffixes and partial versions are not accepted. The release target performs the following operations:

1. Verifies the clean worktree, required environment variables, signing identities, notarization profile, GitHub authentication, and current Git branch.
2. Runs the complete test suite.
3. Updates `packaging/Info.plist` and increments its build number when the requested version differs, then commits and pushes that version change.
4. Creates and pushes an annotated Git tag.
5. Creates a draft GitHub release. Its notes contain a short description, up to eight recent commit subjects, installation guidance, and a full-changelog link when an earlier tag exists.
6. Runs `make installer` to build, sign, notarize, staple, and validate `dist/Octopus-X.Y.Z.pkg`.
7. Uploads the installer and publishes the draft as the latest GitHub release.

The command pushes commits and tags to `origin`; verify the current branch and remote before running it. It does not collect or commit outstanding development work automatically—the only commit it creates is the version metadata update.

#### Release troubleshooting

`error: the Git worktree must be clean before creating a release` means `git status --short` reports at least one modified, staged, or untracked file. Review those files, commit the ones intended for the release, and remove, ignore, or separately preserve anything that should not be published. Then rerun the release command.

The GitHub release remains a draft if building, signing, notarization, or upload fails. Correct the problem and run the same command again to resume it. A published version cannot be overwritten by this target.

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

Every save is parsed and validated before replacing the existing file. A valid save reloads the router immediately without restarting the menu-bar app. If validation fails, the existing file and running router remain unchanged and the settings page shows the error. If a newly saved configuration cannot start—for example because a credential environment variable is absent—the file remains saved, the previous working router stays active when possible, and the status area reports the problem.

The settings interface follows the macOS light or dark appearance and supports keyboard navigation, visible focus states, reduced motion, and WCAG 2.2 AA contrast.

### Insights and savings calculation

Insights is the final item in the Settings sidebar. It records one aggregate observation when a provider completes a request with final token usage. Tracking begins after an Insights-capable build starts; Octopus does not reconstruct earlier history from logs.

#### What Insights provides

The range selector supports the last 7, 30, 90, or 365 days. Every range shows:

- **Net savings**: estimated savings after routing, prompt-cache effects, and classifier overhead.
- **Actual cost**: the chosen model's cache-adjusted cost plus any classifier cost.
- **Baseline cost**: estimated uncached cost of using the highest-quality eligible model instead.
- **Requests and tokens**: completed requests and provider-reported input, output, cache-creation, and cache-read tokens.
- **Savings over time**: cumulative actual cost and cumulative net savings across the selected daily range.
- **Model routing**: savings from choosing a less expensive model rather than the baseline model.
- **Prompt caching**: the chosen model's uncached cost minus its measured cache-adjusted cost. An initial cache write can make this negative.
- **Classifier overhead**: input and output cost of nontrivial request classification, subtracted from savings.
- **Cache hit rate**: cache-read input tokens divided by all reported input tokens, including ordinary input and cache creation.
- **Model usage**: completed requests, tokens, and serving cost grouped by the model that returned the response. Classifier cost appears in the summary rather than this table.

If a request uses models whose catalog prices are all zero, Insights still counts its requests and tokens but warns that savings may be understated.

#### Baseline selection

The baseline is selected independently for every request:

1. Octopus applies the normal tool, vision, context-window, and high-difficulty quality filters.
2. It considers only the models left in that request's eligible set.
3. It selects the model with the highest configured `quality` value. If the chosen model ties for highest quality, it remains the baseline.
4. It prices the baseline using the completed request's measured input and output token quantities at ordinary uncached catalog rates.

This counterfactual answers: "What would the same measured request have cost on the highest-quality model Octopus could safely have selected?"

#### Cost formulas

Let `Pᵢ(model)` and `Pₒ(model)` be the configured input and output prices per million tokens. Provider-reported token counts are:

- `I`: ordinary input tokens
- `W`: cache-creation input tokens
- `R`: cache-read input tokens
- `O`: output tokens

The uncached cost of a model is:

```text
uncached(model) = ((I + W + R) / 1,000,000 × Pᵢ(model))
                 + (O / 1,000,000 × Pₒ(model))
```

The measured cost of the chosen model is:

```text
chosen measured = ((I + W × write_multiplier + R × 0.10) / 1,000,000 × Pᵢ(chosen))
                  + (O / 1,000,000 × Pₒ(chosen))
```

The cache write multiplier is `1.25` for a five-minute cache and `2.00` for a one-hour cache. Classifier input and output tokens are priced separately using the classifier model's catalog prices.

The displayed savings components are:

```text
routing savings = uncached(baseline) - uncached(chosen)
cache savings = uncached(chosen) - chosen measured
net savings = routing savings + cache savings - classifier overhead
actual cost = chosen measured + classifier overhead
```

Octopus retains negative routing, cache, and net savings. A negative value truthfully indicates that the chosen route, a cache write, or classifier overhead cost more than the baseline for that period.

#### Persistence and privacy

Insights stores daily totals and per-model aggregates in `~/.octopus/insights.json`. The containing `~/.octopus` directory uses mode `0700`; the ledger is atomically replaced with mode `0600`.

The ledger contains no prompts, responses, system instructions, tool definitions, session identifiers, API keys, or other credentials. It contains dates, model IDs, token totals, request counts, and calculated USD amounts. Quit Octopus before manually removing `~/.octopus/insights.json` to reset history.

#### Accuracy and limitations

These figures are estimates, not provider invoices. They depend on current catalog prices and provider-reported usage. Requests without final provider usage are not counted. Changing catalog prices affects future observations only; historical aggregates retain the economics calculated when each request completed.

The counterfactual uses the completed request's measured token quantities for both the chosen and baseline models. A different baseline model might have produced a different number of output tokens in reality. Provider-specific taxes, volume discounts, batch discounts, tiered pricing, and charges not represented by `cost_per_mtok_in` or `cost_per_mtok_out` are outside the calculation.

### Provider credentials at launch

Start Octopus from an environment containing the provider credentials named in the configuration. For a local development build:

```bash
ANTHROPIC_API_KEY="sk-ant-..." dist/Octopus.app/Contents/MacOS/octopus
```

Applications opened from Finder do not inherit shell-only exports. For routine Finder launches, provide credentials through your login-session environment, use the optional inline-key field in the local configuration, or use a local provider that does not require a key. Prefer environment-variable names such as `ANTHROPIC_API_KEY`; an inline key is stored in `~/.octopus/config.yaml`, which Octopus protects with mode `0600` but does not encrypt.

### Remove the app

Quit Octopus, remove `Octopus.app`, and optionally remove `~/.octopus` if you no longer want the configuration and Insights history. Removing `~/.octopus` deletes user data and is not performed by the build or app.

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

The model label in Claude Code describes the client-side model name, not necessarily the backend Octopus selected. A launcher can pass `--model octopus` to make that distinction visible; the routed provider and model remain available in Octopus logs and Insights.

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

1. `X-Octopus-Session-ID` HTTP header, or the legacy `X-LLMRouter-Session-ID` header. Either is the explicit session key: it alone determines the session.
2. A deterministic SHA-256 identifier derived from the system prompt, tools, and first genuine user turn.

Anthropic `metadata.user_id` and the OpenAI `user` field are not explicit keys. They contribute to the derived hash instead, so two users sending the same opening prompt stay separated, while one user's distinct conversations are not merged onto a single model and a false prompt-cache prediction.

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

Naming a variable that is not set in the environment resolves to an empty token, which disables authentication rather than rejecting every request. Octopus does not warn about this, so confirm the variable is exported in the environment the router actually runs in — for the menu bar app, that is the environment Launch Services gives the app, not your shell.

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

## License

MIT. See [LICENSE](LICENSE).
