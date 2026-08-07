# Octopus

![Octopus logo](octopus.png)

Octopus is a model dispatcher that runs on your computer. Your AI application talks to Octopus instead of talking directly to Claude, Gemini, OpenAI, Qwen, or a local model. Octopus examines each request, removes models that cannot do the job, estimates the likely cost of finishing it, and sends it to the most suitable model left.

The aim is practical: use an expensive model when its extra ability is useful, use a cheaper model when it is good enough, and avoid changing models when rebuilding a large prompt cache would cost more than the switch saves. Octopus does not make models cheaper, shorten prompts, or know the future. It makes a configurable decision from the information available at that moment.

Octopus works especially well with Claude Code. It preserves Anthropic prompt-cache instructions, tracks which model probably has a reusable cache, keeps separate state for parallel subagents, and shows the actual routed model and estimated savings in Insights. The model name displayed by Claude Code is the name Claude Code requested; use Octopus logs or Insights to see which model actually answered.

## Contents

- [How Octopus works](#how-octopus-works)
- [Quick start](#quick-start)
- [Using Octopus](#using-octopus)
  - [Claude Code](#claude-code)
  - [Codex CLI](#codex-cli)
  - [Other clients](#other-clients)
- [macOS menu bar app](#macos-menu-bar-app)
- [Prompt caching](#prompt-caching)
- [Routing details](#routing-details)
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
- Optional low-cost classifier with a bounded, privacy-safe result cache; only exact allowlisted background traffic skips classification.
- Hard tool, vision, context-window, and output-limit eligibility checks.
- Quality/cost/speed scoring with deterministic fallback order.
- Amortised routing that switches only when expected task savings repay the cache change; sticky and per-turn strategies remain available.
- Full Anthropic `cache_control` preservation, including `5m` and `1h` TTLs.
- Extended-thinking/reasoning mapping and thinking-block round trips.
- Tool calls, parallel tool results, images, streaming usage, and refusal propagation.
- Loopback-only binding, bounded request bodies, transport timeouts, and graceful shutdown.
- Native macOS menu-bar app with structured settings, an Advanced YAML editor, and immediate validated reloads.

## How Octopus works

Think of Octopus as a taxi dispatcher for AI models. The client says what it needs, Octopus checks which cars can handle the trip, estimates the cost and likely result, then sends the job to one of them. The client continues using the same Anthropic- or OpenAI-shaped API; Octopus handles the provider differences behind it.

```text
Claude Code, Codex, or another client
                  |
                  v
             Octopus
        understand the request
        remove unsuitable models
        compare quality, cost, and speed
        account for reusable prompt caches
                  |
        +---------+---------+----------+
        v         v         v          v
      Claude    Gemini    OpenAI    local model
```

### Six useful ideas

- A **provider** is the service that runs a model, such as Anthropic, Google, OpenAI, or Ollama.
- The **catalogue** is your list of available models. For each model, you describe its price, quality, speed, context window, and abilities such as tools, images, and reasoning.
- The **classifier** is a small, optional model call that labels each ordinary user turn as trivial, low, medium, or high difficulty, separately marks its consequence as ordinary, important, or critical, and estimates its size. Prompt length alone never makes a request trivial.
- A **session** is one conversation. Octopus remembers the model currently serving it, called the incumbent, so follow-up turns are judged with the right history.
- A **prompt cache** is a provider-side reusable copy of the long, unchanged beginning of a conversation. Reading it can be much cheaper than sending and processing that text again.
- A **routing strategy** controls whether Octopus may change the incumbent and how it decides whether a change is worthwhile.

### What happens to a request

1. **The client calls Octopus.** Claude Code normally uses the Anthropic-compatible endpoint; other applications can use the OpenAI-compatible endpoint.
2. **Octopus checks the request.** It rejects malformed or oversized requests before they reach a provider. The model name supplied by the client is a request label, not proof of which backend will be used.
3. **It identifies the conversation.** An explicit `X-Octopus-Session-ID` is best. Without one, Octopus derives a private identifier from the opening conversation material. Giving each subagent a distinct session ID keeps its history and cache separate.
4. **It recognises declared background traffic.** Exact, allowlisted, conversation-independent maintenance pings can go to a cheap model in an isolated session. Octopus does not assume every short request is a ping. For a recognised ping, it removes the main conversation history and cache markers before sending the provider request, so the ping does not pay to load the full working conversation.
5. **It estimates the job.** Each ordinary turn may be classified for difficulty, consequence, tool use, image use, reasoning, and likely input/output size. Identical classifier inputs are briefly cached, so parallel repeats do not pay for the same classification again. If classification fails, Octopus uses a high-difficulty, critical fallback.
6. **It removes models that cannot do the work.** A model is ineligible if it lacks a required ability, cannot fit the context or expected answer, violates the configured local-only privacy policy, or falls below the applicable strict quality floor.
7. **It compares the eligible models.** Quality, estimated dollar cost, speed, and reasoning support contribute to a weighted score. The default cost calculation uses actual catalogue prices rather than merely comparing every model with the cheapest model in the current set.
8. **It considers the rest of the task, not only this turn.** Under the default `amortized` strategy, Octopus estimates how many turns remain. It changes models only when the expected saving across those turns is large and credible enough to repay any cold-cache cost.
9. **It translates and sends the request.** Octopus converts the common request into the selected provider's wire format while preserving supported tools, images, reasoning blocks, and cache instructions.
10. **It handles safe failures.** If a provider fails before any response bytes are sent, Octopus can try the next eligible model. Once a streamed response has begun, it cannot silently swap providers without corrupting the response.
11. **It learns from the result.** Provider-reported token and cache usage updates the session forecast. Logs and Insights record the routed model, estimated cost, alternative cost, cache outcome, and estimated saving without storing prompt or response text.

### Why staying can be cheaper than switching

Suppose a long conversation is already cached on Model A. Model B may have a lower price per token, but its first turn must usually process and cache the whole conversation again. Octopus therefore compares:

```text
stay cost   = the remaining turns on Model A using its warm cache
switch cost = one cold/cache-write turn on Model B
              + later turns on Model B using its warm cache
```

If the task is almost finished, staying may be cheaper. If several turns remain, the lower price of Model B may repay the one-off switch cost. This is an estimate: Octopus cannot know exactly how many turns the task will take or whether one model will solve it in fewer turns than another.

The three strategies make different trade-offs:

| Strategy | Plain-English behaviour |
|---|---|
| `amortized` | Default. Switch only when the predicted whole-task saving repays the cache change and clears the configured confidence and saving thresholds. |
| `sticky` | Keep the first successful model for the session while it remains capable. Simple and cache-friendly, but it does not reconsider price or quality on normal follow-ups. |
| `per_turn` | Score every turn independently. Responsive to changes, but more likely to move between models and rebuild caches. |

For example, a short factual question can still be classified as low and go to a fast, inexpensive model. A concise production or security question can be classified as high or critical and require the strongest quality tier. A long conversation may remain on its current eligible model when the one-off cache rebuild would not be recovered before the task ends.

The weights express influence inside the safe candidate set. `routing.quality_floors` is different: models below the applicable floor are ineligible, and cost, speed, affinity, or a warm cache cannot bring them back. If no model clears a required floor, Octopus fails closed rather than silently lowering quality.

### Parallel subagents

Each subagent conversation with a distinct session ID gets its own incumbent and cache state. This prevents one subagent from pretending it owns another's cache. A client may also send the same `X-Octopus-Workflow-ID` for related subagents. Octopus can then prefer a model that has worked well for that workflow, while still keeping each conversation's cache accounting separate.

### What Octopus cannot know

Octopus works from configured model ratings, prices, request shape, provider usage, and recent session history. It cannot inspect the future, guarantee that the cheapest model will finish in the same number of turns, or turn estimated Insights savings into an exact provider invoice. Good routing still depends on keeping the catalogue and prices realistic.

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
  strategy: "amortized"
  data_policy: "allow_remote"
  session_ttl: "1h"
  cache_aware: true
  default_remaining_turns: 4
  min_switch_savings_usd: 0.01
  min_switch_savings_pct: 0.10
  switch_confidence: 0.60

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

1. Enable `routing.cache_aware` and use the default `routing.strategy: amortized` (or choose `sticky` for hard affinity).
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

- **General** controls the router address, classifier, scoring weights, routing strategy, forecast thresholds, cache behavior, and the fallback attempt limit.
- **Providers** configures provider kinds, endpoints, environment-variable names, and optional inline credentials. Inline credentials are stored locally in the YAML file; prefer environment variables when practical.
- **Models** edits the routing catalog, pricing, capabilities, context and output limits, and optional work-per-turn efficiency priors.
- **Advanced YAML** edits the complete configuration directly.
- **Insights** shows request volume, token usage, estimated spend, savings over time, cache efficiency, model usage, and recent switch economics.

Saving requires a CSRF token that is generated fresh each time Octopus starts and embedded in the page when it loads. A settings page left open across a restart therefore holds a token the new process will not accept, and must be reopened from the menu bar before it can save.

Settings shows the configuration in full, inline `api_key` values included, on both the Providers form and the Advanced YAML tab. An editor that hides part of the file cannot show you what you are about to change, and the file is yours on your own machine. The protection is on writes rather than reads: a save is accepted only from a request whose `Host` header is literally loopback and that carries the current CSRF token, so a web page that resolves its own hostname to `127.0.0.1` cannot alter the configuration or repoint a provider. Such a page can read what Settings serves, so treat the settings port as it is treated here — anything in `~/.octopus/config.yaml` is readable by anything running on, or reaching, your machine.

Every save is parsed and validated before replacing the existing file. A valid save reloads the router immediately without restarting the menu-bar app. If validation fails, the existing file and running router remain unchanged and the settings page shows the error. If a newly saved configuration cannot start—for example because a credential environment variable is absent—the file remains saved, the previous working router stays active when possible, and the status area reports the problem.

The settings interface follows the macOS light or dark appearance and supports keyboard navigation, visible focus states, reduced motion, and WCAG 2.2 AA contrast.

### Insights

Insights is the last item in the Settings sidebar. It records one aggregate observation each time a provider completes a request with final token usage, over the last 7, 30, 90, or 365 days. Tracking starts when an Insights-capable build first runs; history is not reconstructed from logs.

It reports net savings, actual and baseline cost, request and token counts, savings over time, cache hit rate, and a per-model usage breakdown. Savings are split three ways: **quality-baseline savings** (choosing a cheaper model than the highest-quality eligible baseline), **prompt caching** (cache reads against the chosen model's uncached cost), and **classifier overhead**, which is subtracted. Negative values are kept rather than clamped — a cache write or a classifier call really can cost more than it saved in a given period.

The **Switch economics** table is a different measurement. It explains recent amortised decisions: incumbent and candidate, forecast turns on each model, warm stay cost, candidate switch cost, estimated saving, break-even turns, decision, confidence, and the provider's actual cache-write or cache-read token outcome. This is the evidence for whether a switch was expected to pay back; it is not mixed into the quality-baseline counterfactual.

#### Baseline selection

The baseline answers: what would this same request have cost on the best model Octopus could safely have picked? For each request, Octopus applies the usual tool, vision, context, and difficulty filters, takes the highest-`quality` model still eligible, and prices it at ordinary uncached rates using the measured token counts. If the chosen model was already the highest quality, it is its own baseline.

#### Cost formulas

With `Pᵢ`/`Pₒ` the configured per-million input and output prices, and `I`, `W`, `R`, `O` the provider-reported ordinary-input, cache-write, cache-read, and output tokens:

```text
uncached(model)  = (I + W + R)/1e6 × Pᵢ(model) + O/1e6 × Pₒ(model)
chosen measured  = (I + W×write_mult + R×0.10)/1e6 × Pᵢ(chosen) + O/1e6 × Pₒ(chosen)

quality-baseline savings = uncached(baseline) - uncached(chosen)
cache savings    = uncached(chosen)  - chosen measured
net savings      = quality-baseline savings + cache savings - classifier overhead
actual cost      = chosen measured + classifier overhead
```

`write_mult` is `1.25` for a five-minute cache and `2.00` for one hour. Classifier tokens are priced at the classifier model's own catalog rates.

#### Privacy and accuracy

Daily totals, per-model aggregates, and a bounded list of the 200 most recent switch decisions live in `~/.octopus/insights.json`, written atomically with mode `0600` inside a `0700` directory. The ledger holds dates, model IDs, token totals, request counts, forecasts, cache outcomes, and USD amounts — no prompts, responses, tool definitions, session identifiers, or credentials. Quit Octopus before deleting it to reset history. Existing version-1 ledgers are upgraded in place when the next observation is written.

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

A long AI conversation repeatedly sends much of the same text: system instructions, tool definitions, and earlier messages. Some providers can keep that stable beginning in a temporary prompt cache. The first use costs more because the provider creates the cache; later reads can cost much less.

Octopus does not store that provider cache itself. It passes the client's cache instructions through, remembers the cache usage reported by the provider, and uses that evidence when deciding whether staying or switching is cheaper. For Anthropic-shaped requests it supports:

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

After a successful response, Octopus records the provider-reported ratio of cached to uncached input for that model. The session keeps a separate live-cache record for every model it has used, so switching away does not erase knowledge that an earlier model may still be warm. Returning to that model before its cache expires is priced as a cache read rather than another cold write.

### Session state and affinity

Octopus records the successful model as the session incumbent, plus per-model cache expiry and coverage, turn count, recent input growth, and recent output size. The `amortized` strategy uses that state for forecasts; the `sticky` strategy always selects the incumbent while it remains eligible. Session identity is resolved in this order:

1. `X-Octopus-Session-ID` HTTP header, or the legacy `X-LLMRouter-Session-ID` header. Either is the explicit session key: it alone determines the session.
2. A deterministic SHA-256 identifier derived from the system prompt, tools, and first genuine user turn.

Anthropic `metadata.user_id` and the OpenAI `user` field are not explicit session keys. They contribute to the derived hash instead, so two users sending the same opening prompt stay separated, while one user's distinct conversations are not merged onto a single model and a false prompt-cache prediction. They are currently routing-only and are not forwarded upstream because Harness has no provider-visible end-user metadata field.

Explicit identifiers are hashed before being stored. The in-memory session table is concurrency-safe, expires entries, and has a hard size bound. Session state is process-local and is reset when Octopus restarts.

If the incumbent is no longer eligible because of tools, vision, context size, or output limit, normal scoring selects another model regardless of strategy. A successful fallback becomes the new incumbent.

### Restart behavior

Octopus does not cache prompts itself — caching happens on the provider (for example Anthropic), based on the stable prompt prefix it receives. Octopus rebuilds requests into the selected provider's wire format, but preserves supported prompt content and cache markers deterministically. It tracks enough local state to price staying, switching, and switching back. That state is in memory and is not persisted, so a process restart resets it while provider-side cache entries may still exist.

The next request in each affected session after a restart is scored without that memory:

- **The incumbent is unknown.** The request follows the initial-routing path: it starts from the balanced score and may choose a lower cost-to-completion model when the horizon is confident and the saving margins are met.
- **Per-model cache coverage is unknown.** A cache-capable candidate is conservatively treated as requiring a full cache write (`1.25×`/`2.00×`) even if the provider may still hold a warm prefix.

Two outcomes follow, both limited to that one request:

1. Scoring still lands on the same model the conversation was already using. The provider's cache may still be warm, so the real bill reflects a cache hit even though the pre-request forecast could not know it.
2. Scoring lands on a different model. This is a genuine cache-losing switch — a real cold write at whatever provider is now serving the conversation, no different from any other mid-conversation switch.

Either way, the response carries real `cache_creation_input_tokens`/`cache_read_input_tokens` usage, and `Router.Observe` immediately repopulates the incumbent and that model's cache state. The effect is therefore confined to the first post-restart request for an active session. There is no snapshot/restore for the session table; only the deterministic session identity survives independently of process memory.

## Routing details

The earlier [How Octopus works](#how-octopus-works) section gives the everyday mental model. This section records the exact routing rules for operators who need to tune or troubleshoot them.

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
Build warm/cold cost-to-completion forecasts
        │
        ▼
Apply the configured routing strategy
        │
        ▼
Choose or retain the model
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
- `expected_remaining_turns`: `1..50`, including the current turn.
- `estimate_confidence`: `0..1`, used as the switch-confidence gate.
- `classification_confidence`: `0..1`, confidence in the semantic profile.
- `risk`: `ordinary`, `important`, or `critical`, independent of difficulty.

The classifier is skipped only for an exact, allowlisted, conversation-independent background signature. If `classifier.model` is omitted, blocked by `local_only`, unavailable, times out, or returns invalid output, Octopus applies the conservative high/critical fallback profile.

For nontrivial conversations, classification uses a bounded recent-context representation. Classification timeouts, malformed output, and unavailable classifier providers fall back to the conservative profile rather than failing the client request.

Actual request contents override classifier guesses: images require vision and tools require tool support. A deterministic estimate of the complete system prompt, history, tool schemas, tool arguments/results, requested output, and images provides a context-size floor.

### Eligibility and scoring

Models are first filtered by hard requirements:

- Vision support when images are present.
- Tool support when tools are declared.
- Enough context for estimated input plus output.
- An output limit, when `caps.max_output_tokens` declares one, at least as large as the estimated response.

`routing.quality_floors` can declare a strict minimum for every difficulty. Important risk inherits at least the medium floor, critical risk inherits at least the high floor, and an authenticated caller may raise the minimum further. A floor is a safety boundary: when it removes every otherwise-capable model, the request fails instead of degrading silently.

Remaining models receive a normalized weighted score:

```text
quality × quality_utility
+ cost × cost_utility
+ speed × speed_utility
+ optional reasoning preference
```

Weights need not sum to one; Octopus normalizes them. In the default `absolute` mode, catalogue quality and speed remain on their declared `0..1` scales and request cost uses `1 / (1 + estimated_cost_usd / cost_reference_usd)`. This makes one model's score stable when an unrelated model enters or leaves the eligible set. `relative` preserves the earlier catalogue-relative normalization. Catalogue order is the deterministic tie-breaker.

Classifier results are cached in memory for five minutes in a bounded 1,024-entry LRU and concurrent identical classifications are coalesced. Keys are SHA-256 digests over the exact classifier input and prompt version; prompt text is not retained. Ordinary sticky follow-ups are still classified so a newly difficult or critical turn can immediately make a weaker incumbent ineligible.

### Background requests and subagent workflows

Octopus does not treat every small, non-streaming, tool-free request as a background ping. Such a request can still be a genuine user turn. Background routing matches only entries in `routing.background.signatures`, using the endpoint and SHA-256 digest of the exact final user text. Tools, tool traffic, images, streaming mismatches, or a signature not explicitly marked `conversation_independent` prevent a match. A matched request receives an isolated routing session and cannot replace or refresh the main conversation's incumbent/cache record.

Independent Claude Code subagents can share placement preference by sending the same `X-Octopus-Workflow-ID`. Octopus remembers only a hash-to-model preference after a successful request. Each subagent retains its own session ID and prompt-cache state; workflow affinity never treats one conversation's cache as another's.

### Routing strategies and amortisation

`routing.strategy` controls what happens after eligibility:

| Strategy | Behaviour |
|---|---|
| `amortized` | Recommended and default. Keep or change the incumbent by comparing expected cost to complete the task, including the first cold cache write. |
| `sticky` | Keep the incumbent for the session lifetime whenever it remains eligible. Balanced scores after turn one do not change the model. |
| `per_turn` | Run the balanced quality/cost/speed scorer every turn, with current cache multipliers. It is greedy and does not amortise a switch over future turns. |

The initial request has no incumbent. Octopus starts from the balanced-score choice, then uses a confident task horizon and efficiency priors to move to a lower cost-to-completion model only when the same saving margins are met; otherwise it keeps the balanced choice. On later amortised requests it splits predicted input into a cacheable prefix `K` and uncached tail `U`, and predicts output `O`:

```text
cold(model) = ((K × write_mult + U) × Pᵢ + O × Pₒ) / 1e6
warm(model) = ((K × 0.10       + U) × Pᵢ + O × Pₒ) / 1e6

stay(A)   = forecast N_A warm turns on incumbent A
switch(B) = one cold B turn + forecast N_B-1 warm B turns
```

If B already has its own unexpired cache record, its first turn is warm too. Forecast input grows by the observed input-growth moving average, and forecast output uses the larger of the classified estimate and observed output moving average. For equal work-per-turn, the simple crossover is:

```text
break_even_turns = (cold(B) - warm(B)) / (warm(A) - warm(B))
```

Different models may need different numbers of turns. `catalog[].turn_efficiency` supplies optional progress-per-turn priors by difficulty, and Octopus uses `ceil(expected_remaining_turns / efficiency)` for each model. The neutral prior is `1.0`. This lets a more capable but dearer model be represented as requiring fewer turns instead of pretending that every model finishes in the same `N`.

A switch occurs only when the classifier confidence meets `switch_confidence` and its forecast saving is greater than both `min_switch_savings_usd` and `stay(A) × min_switch_savings_pct`. Otherwise the incumbent is retained. Quality, speed, tools, vision, context, output limits, and the high-difficulty quality floor still define which models are acceptable; dollars decide between those eligible models during the switch comparison.

This is a forecast, not clairvoyance. The classifier cannot know the true remaining turn count, and turn-efficiency priors are operator estimates. The confidence and saving margins are deliberate safeguards against oscillation and false precision. Insights records both the forecast and subsequent provider cache-token outcome so the priors can be calibrated from real work.

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
- A provider that produces no first event within 30 seconds is treated as failed before response bytes and may fall back within the attempt budget.

If every attempted backend fails, Octopus maps rate-limit, overload, invalid-request, and generic backend failures into the appropriate endpoint-specific error shape.

If no catalog entry can satisfy the request, the Anthropic endpoint returns an invalid-request error and the OpenAI endpoint returns HTTP `422`.

### Cross-provider tool-call IDs

A routed conversation can move between providers turn to turn — an easy turn on Gemini, a harder one on Claude — and each new backend receives the full prior history, including tool calls and tool results from whichever provider handled earlier turns. That history has to satisfy the *new* backend's own ID rules, not the one that originally produced it.

This matters most for Gemini: its function-call `id` field is optional and frequently absent, particularly for a single non-parallel call. The harness Gemini provider always emits a client-side ID regardless — a random, `toolu_`-shaped identifier when Gemini's own is empty — rather than falling back to the function name. A name-based fallback is not just a formatting mismatch: it is not unique, since a tool called more than once in a conversation (routine for coding agents re-reading files) would collide on the same ID. If that history is later replayed to an Anthropic-shaped backend, duplicate `tool_use` IDs are rejected outright, which used to surface as a hard failure the moment routing moved a tool-using conversation off Gemini.

Practically, this means catalog entries for Gemini can (and should) declare `caps.tools: true`. Marking Gemini as tool-incapable to avoid the collision is not necessary and defeats cross-provider routing for any tool-using session — which, for coding-agent traffic, is effectively all of it.

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

This is a compatibility gateway, not a claim of complete coverage of every field in either upstream API. Octopus guarantees the documented fields above; clients should not rely on undocumented request fields surviving cross-provider translation.

For buffered client requests, Octopus uses a provider's native non-streaming transport when Harness exposes one. Providers without that optional capability still use an internal stream that Octopus collects before returning buffered JSON. Streaming client requests always use the provider's streaming transport.

## Configuration reference

Octopus parses YAML with unknown-field rejection, so misspelled settings fail fast.

### `server`

| Field | Required | Description |
|---|---:|---|
| `addr` | Yes | Loopback `host:port`, such as `127.0.0.1:8787`, `localhost:8787`, or `[::1]:8787`. Non-loopback addresses are rejected. |
| `auth_token_env` | No | Names the environment variable holding a shared secret for the routing endpoints. The token itself is never written to the config file. Omitted or empty means no authentication. |

When `auth_token_env` is set, requests must present the secret as either `x-api-key` or `Authorization: Bearer <token>`; both are accepted because Anthropic and OpenAI clients each send their own. An `Authorization` header carrying the bare token without the `Bearer ` prefix is also accepted. Requests without a valid secret receive `401` in the error shape of the endpoint they called.

Naming a variable that is not set in the router's environment is a configuration error. Octopus fails startup or reload and keeps any last-known-good authenticated router running; it never silently publishes an open replacement. Confirm the variable is exported in the environment the router actually runs in. For the menu-bar app, that is the environment Launch Services gives the app, not your shell, so a token exported only in `.zshrc` will not be visible to it.

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
| `strategy` | `amortized` | `amortized`, `sticky`, or `per_turn`. |
| `data_policy` | `allow_remote` | `allow_remote`, `prefer_local`, or fail-closed `local_only`. |
| `session_ttl` | `1h` | Inactivity lifetime of the in-memory incumbent and per-model cache records. Must not be negative. |
| `cache_aware` | `true` | Include expected cache writes/reads in scoring and cost forecasts. |
| `default_remaining_turns` | `4` | Fallback task horizon, `1..50`, when classification is unavailable. |
| `min_switch_savings_usd` | `0.01` | Minimum absolute forecast saving required to switch. |
| `min_switch_savings_pct` | `0.10` | Minimum forecast saving as a fraction of incumbent stay cost, `0..1`. |
| `switch_confidence` | `0.60` | Minimum turn-estimate confidence required to switch, `0..1`. |
| `cost_mode` | `absolute` | Stable `absolute` dollar utility or legacy eligible-set-relative scoring. |
| `cost_reference_usd` | `0.10` | Request cost at which absolute cost utility is `0.5`. Must be positive. |
| `quality_floors` | `high: 0.85` | Strict minimum catalogue quality keyed by `trivial`, `low`, `medium`, and `high`. Missing tiers have no floor. |
| `high_quality_floor` | `0.85` | Legacy spelling for `quality_floors.high`; do not configure conflicting values. |
| `reasoning_bonus` | `0.05` | Explicit preference for eligible models with native reasoning; `0` disables it. |
| `workflow_affinity` | `true` | Prefer the last successful eligible model for sessions sharing an explicit `X-Octopus-Workflow-ID`; caches remain independent. |
| `background` | disabled | Exact SHA-256 allowlist for conversation-independent maintenance requests. |
| `max_attempts` | `3` | Maximum backends one request may try. Every failure consumes an attempt except a malformed request and a cancelled request, which stop immediately. Must not be negative; `0` is treated as omitted. |

Legacy files using `session_sticky: true` are translated to `strategy: sticky`; `false` becomes `strategy: per_turn`. When both fields are present, the explicit `strategy` takes precedence. Settings rewrites saved files using `strategy`.

`data_policy` is a placement boundary, not a scoring preference. `prefer_local` restricts scoring and fallback to eligible local providers whenever one can satisfy the request, but permits remote models when none can. `local_only` removes every remote model before scoring, sticky affinity, amortised comparison, and fallback. It also skips a remote classifier. If no local model has the required context or capabilities, Octopus returns an error without contacting the cloud.

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
| `location` | `remote` (default) or `local`. A local provider must use an absolute HTTP(S) loopback `base_url`. |
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
    location: local
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
| `turn_efficiency.trivial` | Optional, `>= 0` | Relative work per turn for trivial tasks; omitted or `0` means `1.0`. |
| `turn_efficiency.low` | Optional, `>= 0` | Relative work per turn for low-difficulty tasks. |
| `turn_efficiency.medium` | Optional, `>= 0` | Relative work per turn for medium-difficulty tasks. |
| `turn_efficiency.high` | Optional, `>= 0` | Relative work per turn for high-difficulty tasks. |

`caps.max_output_tokens` is a hard eligibility filter alongside `caps.max_context`: a model whose declared output limit is below the estimated response size is removed before scoring, rather than being discovered at the backend. The estimate is floored at the client's requested `max_tokens` (or `1024` when unset), so a client that habitually requests a generous `max_tokens` it never fills can exclude models that would in practice have coped.

Catalog prices, capabilities, and efficiency priors are operator-maintained. Keep prices and model metadata synchronized with provider documentation, and calibrate efficiency from your own completed-task data; Octopus does not fetch or infer these values automatically.

Setting `caps.tools: false` on a catalog entry to work around a real or suspected provider incompatibility removes that model from every tool-using request — which, for coding-agent traffic, is effectively all requests. Gemini entries in particular can be declared with `caps.tools: true` safely; see [Cross-provider tool-call IDs](#cross-provider-tool-call-ids).

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

The menu-bar app keeps one listener when settings are saved to the same address and atomically swaps in the validated router. In-flight streams continue on the old handler. When the address changes, the new listener is bound before the old one is drained. Invalid settings or a failed bind leave the last-known-good router running.

`GET /healthz` reports process health and `GET /readyz` reports whether a router, registry, and non-empty catalogue were successfully published. Both return deliberately minimal, credential-free JSON for local service probes.

For a persistent non-app installation, run the binary under your operating system's service manager and send `SIGTERM` during upgrades. Rebuilding alone does not replace an already-running process.

## Observability and troubleshooting

The macOS Settings **Insights** section is the primary view for aggregate usage and estimated savings. It supports 7-day, 30-day, 90-day, and one-year ranges. The version-3 ledger also retains bounded, prompt-free decision evidence: difficulty, domain, risk, classification source/status/latency, applied floor, initial/selected/actual models, and whether fallback was observed. Prompts, responses, session IDs, credentials, and classifier reasoning are not stored. Ledger snapshots are written by a bounded background writer and flushed during graceful shutdown so disk latency does not delay the final streamed event.

Octopus emits structured `slog` text records to standard error. Useful entries include:

- `octopus listening`: successful startup and bind address.
- `routing decision`: chosen model, reason, inferred profile, and eligible models.
- `provider ... failed`: candidate failure and fallback attempt.
- `using fallback model`: successful alternate backend.
- `tool schema normalized`: provider compatibility rewrite.
- `prompt cache usage`: cache creation and cache-read input tokens.
- `request handled`: endpoint, final model, requested model, stream mode, routing reason, and elapsed time.

### Authenticated routing controls

When `server.auth_token_env` is configured and the request is authenticated, a caller can safely narrow normal routing with:

- `X-Octopus-Min-Quality: 0.95` to raise the hard quality floor.
- `X-Octopus-Fixed-Model: provider/model` to require one already-eligible model.
- `X-Octopus-Highest-Quality: true` to choose the highest-quality eligible model.

These controls cannot bypass tool, vision, context, output, data-placement, or quality requirements. Fixed-model and highest-quality controls cannot be combined. Override headers on an unauthenticated/open router are ignored so another local process cannot force expensive traffic.

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

- Check `routing.strategy`. `sticky` intentionally keeps a valid session on its successful model.
- With `amortized`, Insights explains whether low forecast confidence, too few remaining turns, a still-warm incumbent, efficiency priors, or the minimum-saving margins retained it.
- Use a different `X-Octopus-Session-ID`, wait for `session_ttl`, restart Octopus, or select `strategy: per_turn` to compare fresh greedy routes.

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
  strategy: per_turn
  data_policy: local_only
  cache_aware: false
  # Match this deployment's declared quality scale. Omitting a classifier uses
  # the conservative high/critical profile, so that floor must be satisfiable.
  quality_floors: {high: 0.60}

providers:
  local:
    kind: openai
    location: local
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

routing:
  strategy: amortized
  data_policy: prefer_local

providers:
  anthropic:
    api_key_env: "ANTHROPIC_API_KEY"
  local:
    kind: openai
    location: local
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

With `prefer_local`, an eligible local model is used even when a remote model would have a higher balanced score. The cloud becomes eligible only when no local model can satisfy the request. Use `allow_remote` instead if quality/cost/speed and amortised economics should choose freely across both placements; use `local_only` when cloud fallback must never occur.

The classifier receives request content before the final model is selected. Under `local_only`, configure a classifier whose provider is marked `location: local`, or omit `classifier`; a remote classifier is skipped automatically. `prefer_local` does not provide a privacy guarantee and may still use the configured remote classifier.

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
make check       # test, race, vet, formatting, and diff checks
make eval-local  # tracked offline and local-mock evaluation tiers
make eval-gate RUN2_RESULTS='a.json b.json' # production thresholds over repeated paid runs
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
├── octopus-eval/      Tracked deterministic and paid evaluation harness
├── docs/              Production gates and engineering specifications
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

This builds `Octopus.app`, signs it with the hardened runtime and a trusted timestamp, checks the signing team against `TEAM_ID`, packages it to install into `/Applications`, notarizes and staples the result, and validates it. It produces `dist/Octopus-<version>.pkg`, a SHA-256 checksum, and a JSON Go dependency manifest, all versioned from `packaging/Info.plist`.

### Publish a release

The worktree must be clean — a tag has to identify one complete commit — and `gh auth status` must succeed.

```bash
make release v0.2.0
```

The version must be exactly `vX.Y.Z`; prerelease suffixes are rejected. The target verifies the environment, runs the complete `make check` gate, bumps `packaging/Info.plist`, pushes the commit and an annotated tag, creates a draft release, builds and notarizes the installer, uploads the installer, checksum, and dependency manifest, then publishes.

It pushes to `origin`, so check your branch first. The only commit it creates is the version bump; it never sweeps up outstanding work.

**If it fails partway**, the release stays a draft. Fix the problem and run the same command to resume. A published version cannot be overwritten.

Release notes are generated from up to eight recent commit subjects. That is a starting point, not a changelog — edit them afterwards with `gh release edit <tag> --notes-file <file>`.

## License

MIT. See [LICENSE](LICENSE).
