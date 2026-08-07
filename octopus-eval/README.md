# Reproducing the Octopus evaluation

The harness source is tracked with Octopus. Generated reports, raw captures,
transcript-derived data, paid responses, and logs remain ignored. This keeps the
method reproducible without committing private or costly run artefacts.

## Routing evaluation gate

After collecting repeated judged run2 result files, evaluate them against the
thresholds fixed in `production-gates.json`:

```bash
python3 scripts/production_gate.py run-a.json run-b.json run-c.json run-d.json run-e.json
# or, from the repository root:
make eval-gate RUN2_RESULTS='run-a.json run-b.json run-c.json run-d.json run-e.json'
```

The gate requires five distinct run IDs, the same suite digest in every file,
at least 50 judged scenarios and at least 200 routed turns per run. Its two-way
bootstrap resamples both runs and scenarios, preserving repeat-run stochastic
variance instead of treating repeated observations as independent scenarios. It
checks mean quality, quality uncertainty, savings, high-tier model safety, and high-difficulty
classifier recall, and permits no max-token truncations. Missing routing evidence counts as a classifier miss. New run2 output attaches the router's prompt-free
`routed_difficulty` to each turn. Older output without it fails the recall gate
instead of silently treating missing evidence as a pass.

A passing `routing_gate_passed` value is not a production-readiness claim.
Critical-task review, provider-invoice reconciliation, concurrency, reliability,
security, release, rollback and soak evidence remain separate mandatory gates
documented in `../docs/production-readiness.md`.

## Live Haiku/Sonnet/Opus switching comparison

`run2.sh` is a separate paid, paired benchmark. The default `smoke` suite runs
six four-turn workflows through two arms:

1. Octopus with the local three-model `config.yaml` and live amortised routing.
2. The configured LiteLLM endpoint with every turn forced to Opus.

The scenarios cover customer support, Go concurrency, business analysis,
security, production migration, and technical writing. Difficulty rises and
falls inside each conversation so the result measures actual model changes,
not merely the first model chosen for unrelated one-shot prompts. A stable long
system prefix exercises prompt caching.

```bash
export ANTHROPIC_AUTH_TOKEN="..."
./run2.sh --yes
```

Scenario definitions are tracked under `scenarios/`. The production suite has
50 conversations and 200 turns per arm across coding, security, operations,
data analysis, writing, support, mathematical reasoning, tool use,
short-but-hard requests, cache-heavy sessions and subagent-like workflows. A
shared fourth turn asks each conversation to identify its most important
unverified assumption. Select it explicitly and set a spend guard:

```bash
./run2.sh --yes --suite production --production-evidence --budget-usd 75
```

`--scenario-file PATH` loads an operator-supplied schema-version-1 JSON suite.
Suite content is validated and its SHA-256 plus ordered scenario IDs are stored
in every checkpoint and result. Resume refuses a changed suite. Paired arms use
different provider-cache namespaces, and repeat-run seeds vary both arm order
and blind A/B placement. `repeat-production.json` predeclares five seeds; paid
runs are still invoked separately and each still requires `--yes`. Set a
per-run budget, then run the five `command` entries in the manifest; their
distinct `--output-prefix` values preserve every result for the aggregate gate:

`--production-evidence` runs live Octopus routing plus fixed Haiku, Sonnet and
Opus baselines. It records separate Opus and Sonnet blind judgements, executes
one scenario-specific deterministic assertion for every workflow, and launches
50 isolated Octopus conversations concurrently. Each concurrent conversation
has a unique canary, session ID, workflow ID and cache namespace; the gate
requires every canary to be echoed and rejects any cross-stream canary. Results
include every arm, judge model, assertion, stream/decision mapping, observed
concurrency and evidence cost. The production gate requires
all of them; the affordable paired smoke run cannot be mistaken for production
evidence. `--arms`, `--judge-models` and `--concurrency` remain available for
targeted experiments.

```bash
export RUN2_BUDGET_USD=75
# Print, review, and then run each of the five commands one at a time.
python3 -c 'import json; print("\n".join(r["command"] for r in json.load(open("repeat-production.json"))["runs"]))'

# After all five complete, run the aggregate gate with the exact artefacts.
python3 scripts/production_gate.py \
  production-01-results.json production-02-results.json \
  production-03-results.json production-04-results.json \
  production-05-results.json
```

Use `--no-judge` to skip blinded quality judging and measure only routing and
workload cost. The script refuses to run without `--yes` because it makes paid
calls. The production suite also refuses to run without `--production-evidence`
and `--budget-usd`. The
guard uses observed provider response usage; already-running concurrent calls
can finish after the threshold is crossed. Classifier spend is reported separately. The preflight
prints scenario/turn counts and a deliberately conservative maximum-output
cost, excluding input and classifier tokens. It starts a headless Octopus instance on port `18787`, leaving the normal
menu-bar app on `8787` alone. Set `RUN2_PORT` if necessary.

Each completed arm and judgement is checkpointed. If a provider error or local
interruption stops a run, use `./run2.sh --yes --resume`; completed paid calls
are reused when accounting remains complete. If interruption happens inside a
routed arm, resume preserves completed all-Opus baselines but resets Insights
and reruns the cheaper Octopus arms from turn one, preventing partial traffic
from being counted twice. A fresh run uses a new cache namespace and fresh
Insights ledger.

Every result also binds the source commit, built-binary SHA-256, configuration
SHA-256, effective-policy digest and redacted provider endpoint identity.
Aggregate gating rejects mismatched provenance and can pin an expected release
commit. Provider retry attempts record status, latency and any returned usage;
known retry spend is included. When an upstream failure omits usage, the result
explicitly retains `invoice_reconciliation_required: true` rather than claiming
complete local cost reconciliation.

The generated `run2-summary.md` reports measured workload cost, classifier
overhead, model allocation, switches, a turn-by-turn comparison, and blinded
quality scores. `run2-results.json` contains the raw prompts, responses, usage,
costs, and judge decisions. Judge spend is reported separately and excluded
from workload savings. Both arms receive the same 6,000-token client output
allowance, and any max-token stops are reported. The summary calls Octopus *superior* only when it is
cheaper, its mean quality lead is at least 0.25 points, and it wins more
scenarios; within 0.50 points while cheaper is labelled *non-inferior*.

This benchmark uses actual provider-reported usage and an independently run
all-Opus arm. It also shows a normalised Opus counterfactual that applies Opus
prices to Octopus's exact token and cache usage. Neither figure is a universal
quality claim; repeat runs are required to estimate variance.

---

**Quick start: `./run.sh`** — snapshots the current parent Octopus working tree,
including modified and untracked non-ignored source files, then builds it and
runs Tiers A and B. `./run.sh --ollama --mine` adds Tiers C and D without the
paid Tier E tests. The archived v0.3.0 result remains under `historical/`; the
current production-path tests require the current routing APIs.

Every run first writes the canonical structured result set to
`current-run-results.json`, then generates `summary.md`, `checklist.md`, and
`report.html` from it. The runner verifies that their timestamp, subject
fingerprint, check IDs, statuses, and mandatory-failure count agree before it
reports success. Missing mandatory results and duplicate check IDs fail the
run. Unavailable optional integrations are recorded as `SKIP`. The v0.3.0
checklist is archived under `historical/` and is never mixed into a current
report.
`CLAUDE.md` records harness-specific agent guidance, including the traps that
produce wrong conclusions. Load it explicitly in tools that do not discover it.
The runner renders `report.html` automatically. To render it again without
opening a browser, use `python3 render.py --no-open`. A rerendered report can be
checked against the structured result set with:

```bash
python3 scripts/report_results.py validate \
  --json current-run-results.json --summary summary.md \
  --checklist checklist.md --html report.html
```

**Default subject:** the current parent working tree. **Pinned reproduction:**
`github.com/sausheong/octopus` @ `5800888` (v0.3.0, 2026-07-28), which pulls
`github.com/sausheong/harness@v0.3.5-0.20260728054022-445614757b09`.

---

## Portability at a glance

| Tier | Tests | Needs |
|---|---|---|
| **A — anyone, offline** | T3, T3b, T3c, T6, T6b, T7 privacy policy | Go 1.22+, Python 3 |
| **B — anyone, local only** | T2, T4, T4b, T4c, sticky tests | + a free TCP port |
| **C — needs Ollama** | T5, T5b, T5c | + `ollama` and `ollama pull qwen2.5:3b` |
| **D — needs YOUR transcripts** | T5 context distribution | + `~/.claude/projects` history |
| **E — needs an LLM endpoint** | T1, T8, T9, T10, T10b | + an API key (see below) |

Tiers A and B are fully deterministic and reproduce exactly. C is
hardware-dependent (timings will differ). **D produces *your* numbers, not
ours** — that is the intent. **E is the only tier that spends money** (a few cents) and
the only one that will not work outside an org with access to the endpoint.

---

## Pinned manual setup

```bash
# 1. Pin the subject (FULL clone — a --depth 1 clone only works while
#    5800888 is still the tip, and breaks as soon as upstream pushes)
git clone https://github.com/sausheong/octopus.git
cd octopus && git checkout 5800888

# 2. Add the Go tests + a headless entrypoint
cp <this-dir>/scripts/terra_*.go router/
mkdir -p cmd/octotest && cp <this-dir>/scripts/octotest-main.go cmd/octotest/main.go
GOWORK=off go build -o /tmp/octotest ./cmd/octotest
```

**Why a headless entrypoint is required:** on macOS, `cmd/octopus` hard-codes
`~/.octopus/config.yaml` (ignoring `--config`) and launches a menubar app.
`octotest` is a copy of the *non-darwin* `cmd/octopus/main.go` wiring — same
config → registry → router → server chain, no UI. It creates no
`~/.octopus`.

`terra_sweep_test.go` defines the `terraCatalog()` and `multipliers()` helpers
used by most of the other Go tests — copy all of `terra_*.go` or none.

---

## Running

For a deterministic CI/release check that opens no listeners and makes no
network calls, run:

```bash
./run.sh --tier-a
```

### Tier A — offline, deterministic

```bash
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go test ./router -run 'TestTerra|TestSweep|TestCache|TestWhere|TestLocal|TestWhy|TestOpus' -v
GOWORK=off go test ./router -run '^TestEvalFeature' -v
GOWORK=off go test ./config ./router ./server -run 'TestParseAndMarshalDataPolicy|TestValidateLocalProvider|TestProviderLocation|TestLocalOnly|TestPreferLocal' -v
python3 scripts/t6_economics.py         # per-turn + switch economics
python3 scripts/t6b_breakeven.py        # cumulative cost curve, crossover
python3 scripts/t3c_sensitivity.py      # 200k randomised catalogs (seeded)
```

`t3c_sensitivity.py` takes optional `[seed] [n]`. The seeded default reports
both the production absolute per-turn retention rate and the explicitly
labelled legacy relative rate; amortised final choices are covered by the Go
production-path scenarios.

### Tier B — local mock upstream

```bash
python3 scripts/mock_upstream.py 9099 /tmp/cap &     # records the upstream request
/tmp/octotest --config configs/config-mock-nr.yaml & # port 8787

python3 scripts/t4_latency.py 200                    # T4  small-payload latency
python3 scripts/t4b_latency_ext.py                   # T4b streaming/tools/concurrency
# T2: inspect semantic preservation and documented routing transformations
curl -s localhost:8787/v1/messages -H 'content-type: application/json' \
     -X POST --data-binary @configs/probe.json
diff <(jq -S . configs/probe.json) <(jq -S . /tmp/cap/req-0001.bin)
```

Octopus is a provider-neutral router, so it rebuilds requests and does not
promise byte-for-byte passthrough. T2 requires message content, tool-schema
semantics, ordered arrays, and cache controls to survive. Deterministic JSON
object-key ordering is informational. End-user metadata remains routing-only
until Harness exposes a provider-visible field for it.

For T4c (payload scaling) use `configs/config-mock-big.yaml` instead — the
default 200k `max_context` makes the eligibility filter reject large requests
before they can be timed.

For the sticky tests, run **two** mocks (9099 and 9100) and use
`configs/config-sticky-{true,false}.yaml`.

### Tier C — Ollama

```bash
ollama pull qwen2.5:3b
/tmp/octotest --config configs/config-ollama.yaml &   # port 8788
python3 scripts/t5b_ollama_ext.py       # throughput, context, tools, concurrency
python3 scripts/t5c_ollama_numctx.py    # progressively measures accepted bounds
```

Note: Ollama.app owns `:11434` and **respawns** if killed. To test the
server-side `OLLAMA_CONTEXT_LENGTH` fix, run a second instance on another port
(`OLLAMA_HOST=127.0.0.1:11500 OLLAMA_CONTEXT_LENGTH=32768 ollama serve &`) and
pass that host to `t5c`.

### Tier D — your own transcripts

```bash
python3 scripts/t5_ctx_distribution.py
```

Reads `~/.claude/projects/**/*.jsonl`, dedupes on `message.id`, and reports the
context distribution. **Expect different numbers from ours** — that is the point.

Run the complete local, no-spend A-D set with:

```bash
./run.sh --ollama --mine
```

### Tier E — live endpoint (spends money)

**No AIP key? Use your own Anthropic key.** These tests are not gateway-specific
— they need any endpoint speaking Anthropic `/v1/messages` that reports
`cache_read_input_tokens`, which `api.anthropic.com` does natively. Three env
vars redirect everything; no file edits:

| var | default | notes |
|---|---|---|
| `EVAL_BASE` | `https://litellm-stg.aip.gov.sg` | endpoint base URL |
| `EVAL_MODEL` | `claude-haiku-4-5@20251001-global` | Anthropic model id |
| `EVAL_API_KEY` | falls back to `OPENAI_API_KEY`, then `ANTHROPIC_API_KEY` | never written to disk |

```bash
# Direct Anthropic — no gateway needed
export EVAL_BASE=https://api.anthropic.com
export EVAL_MODEL=claude-haiku-4-5-20251001
export EVAL_API_KEY=$ANTHROPIC_API_KEY
./run.sh --live          # uses configs/config-anthropic.yaml automatically
```

**Running T1 this way is more valuable than our own run**, because it closes the
one gap we left open: we verified Octopus's key-reordering is cache-safe only on
the `Octopus → litellm → Vertex` path. A direct-Anthropic run settles it outright.

**What a plain Anthropic key cannot do:** T10/T10b compare caching *across
providers*, so they need a multi-model gateway. With Anthropic alone you get
T1, T8 and T9 (the cache-layer questions); T10 will report the non-Anthropic
models as UNAVAILABLE. Point `EVAL_MODELS` at whatever your gateway exposes:

```bash
EVAL_MODELS="gpt-4o-mini,gemini-2.0-flash,claude-haiku-4-5-20251001" \
  python3 scripts/t10_cross_provider_cache.py
```

Original AIP-internal invocation:

```bash
export OPENAI_API_KEY=...
python3 scripts/t1_cache.py direct       # baseline, then:
/tmp/octotest --config configs/config-litellm.yaml &
python3 scripts/t1_cache.py octopus      # does the proxy void the cache?
python3 scripts/t9_prefix_control.py     # which layer holds the cache
python3 scripts/t10_cross_provider_cache.py   # do GPT/Gemini cache?
python3 scripts/t10b_cache_discount.py        # what is the cache worth?
```

---

## Gotchas that will bite you

- **Cache write-visibility lag.** A cache write is *not* reliably readable ~3s
  later. Our first attempt at T8 put two requests 3s apart, both missed, and
  read as "not a prefix cache" — it was. Leave a gap, or repeat.
- **Anthropic caching is opt-in.** Without `cache_control` markers there is no
  caching at all, and `CacheTTL` returns 0, which disables Octopus's entire
  cache-aware path. GPT/Gemini cache automatically with no marker.
- **Current routing defaults to `strategy: amortized`.** Historical
  `session_sticky` files are translated for compatibility. Scorer-only tests,
  sticky tests, and amortised Router tests are deliberately reported as
  separate modes so cache affinity cannot be mistaken for production switching.
- **Session identity** is derived from `system prompt + tools + FIRST user
  message` (later messages are not hashed). Change the opening message and you
  get a new session; grow the conversation and you do not.
- **Timings are machine-specific.** Tier B/C numbers will not match ours.
- **Kill leftover listeners between runs.** Several of our early numbers were
  measured against stale processes; verify ports are free before benchmarking.

## Not reproducible from here

- The **$11,711 / 81.4% controller** spend figures come from
  `docs/cost-efficiency-review-2026-07-25.md`, not from these scripts.
- Anything on `api.anthropic.com` directly — we had no direct Anthropic key, so
  the claim that Octopus's key-reordering is harmless is **verified only for the
  `Octopus → litellm → Vertex` path**.
