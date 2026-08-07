#!/usr/bin/env bash
# One-command reproduction of the Octopus evaluation.
#
#   ./run.sh              local working tree; Tier A + Tier B, no key or spend
#   ./run.sh --ollama     also Tier C (needs `ollama` + qwen2.5:3b)
#   ./run.sh --mine       also Tier D (reads YOUR ~/.claude/projects transcripts)
#   ./run.sh --local      explicitly select the local working tree (the default)
#   ./run.sh --tier-a     deterministic offline checks only; suitable for CI
#   ./run.sh --pinned     clone and evaluate the historical pinned commit
#   ./run.sh --live       also Tier E (needs $OPENAI_API_KEY; spends a few cents)
#   ./run.sh --all        everything
#
# Full documentation, tier table and gotchas: README.md
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${WORK:-${TMPDIR:-/tmp}/octopus-eval-$$}"
PIN=5800888
PORT_MOCK=9099
PORT_OCTO=8787
PORT_OLLAMA_OCTO=8788

DO_OLLAMA=0; DO_MINE=0; DO_LIVE=0; DO_LOCAL=1; DO_TIER_A_ONLY=0
for a in "$@"; do case "$a" in
  --ollama) DO_OLLAMA=1 ;;
  --mine)   DO_MINE=1 ;;
  --local)  DO_LOCAL=1 ;;
  --tier-a) DO_TIER_A_ONLY=1 ;;
  --pinned) DO_LOCAL=0 ;;
  --live)   DO_LIVE=1 ;;
  --all)    DO_OLLAMA=1; DO_MINE=1; DO_LIVE=1 ;;
  -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
  *) echo "unknown flag: $a (try --help)"; exit 2 ;;
esac; done
[ "$DO_TIER_A_ONLY" = 1 ] && { DO_OLLAMA=0; DO_MINE=0; DO_LIVE=0; }

mkdir -p "$WORK"
RUN_LOG="$WORK/run.log"
RESULTS="$WORK/results.tsv"
EXPECTED_RESULTS="$WORK/expected-results.txt"
: >"$RUN_LOG"
: >"$RESULTS"
: >"$EXPECTED_RESULTS"
exec > >(tee "$RUN_LOG") 2>&1

PIDS=()
LOCAL_ROOT=""
SUBJECT_DESCRIPTION="not prepared"
FAIL_COUNT=0
ROUTING_CONFIG='{"scope":"production defaults exercised by production-path tests","strategy":"amortized","cost_mode":"absolute","cost_reference_usd":0.10,"high_quality_floor":0.85,"reasoning_bonus":0.05,"default_remaining_turns":4,"cache_aware":true}'

expect_result() {
  local id="$1"
  grep -Fxq "$id" "$EXPECTED_RESULTS" 2>/dev/null || printf '%s\n' "$id" >>"$EXPECTED_RESULTS"
}

clean_field() { printf '%s' "$1" | tr '\t\r\n' '   '; }
record() {
  local id="$1" tier="$2" required="$3" status="$4" check="$5" method="$6" result="$7"
  [ "$required" = yes ] && expect_result "$id"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$(clean_field "$id")" "$(clean_field "$tier")" "$(clean_field "$required")" \
    "$(clean_field "$status")" "$(clean_field "$check")" \
    "$(clean_field "$method")" "$(clean_field "$result")" >>"$RESULTS"
  if [ "$required" = yes ] && [ "$status" = FAIL ]; then FAIL_COUNT=$((FAIL_COUNT + 1)); fi
}

# Capture the command's own status before any display-only grep/tail/sed runs.
run_check() {
  local id="$1" tier="$2" required="$3" check="$4" method="$5"; shift 5
  [ "$required" = yes ] && expect_result "$id"
  LAST_OUTPUT="$WORK/${id//[^A-Za-z0-9_.-]/_}.log"
  "$@" >"$LAST_OUTPUT" 2>&1
  LAST_STATUS=$?
  if [ "$LAST_STATUS" -eq 0 ]; then
    record "$id" "$tier" "$required" PASS "$check" "$method" "completed successfully"
  else
    record "$id" "$tier" "$required" FAIL "$check" "$method" "command exited $LAST_STATUS; see console output"
  fi
}

write_reports() {
  local status="$1" tiers="A"
  [ "$DO_TIER_A_ONLY" = 0 ] && tiers="$tiers, B"
  [ "$DO_OLLAMA" = 1 ] && tiers="$tiers, C"
  [ "$DO_MINE" = 1 ] && tiers="$tiers, D"
  [ "$DO_LIVE" = 1 ] && tiers="$tiers, E"
  local subject_root="$WORK/octopus" base_commit="unknown" worktree_state="not recorded"
  if [ "$DO_LOCAL" = 1 ] && [ -n "$LOCAL_ROOT" ]; then
    base_commit="$(git -C "$LOCAL_ROOT" rev-parse --short HEAD 2>/dev/null || printf unknown)"
    if git -C "$LOCAL_ROOT" diff --quiet HEAD -- 2>/dev/null &&
       ! git -C "$LOCAL_ROOT" ls-files --others --exclude-standard 2>/dev/null |
         grep -Ev '^octopus-eval(/|$)' | grep -q .; then
      worktree_state="clean outside excluded octopus-eval artifacts"
    else
      worktree_state="modified or untracked source included; octopus-eval excluded"
    fi
  else
    base_commit="$(git -C "$subject_root" rev-parse --short HEAD 2>/dev/null || printf unknown)"
    worktree_state="pinned checkout with evaluation tests injected"
  fi
  python3 "$HERE/scripts/report_results.py" generate \
    --results "$RESULTS" --expected "$EXPECTED_RESULTS" --log "$RUN_LOG" \
    --summary "$HERE/summary.md" --checklist "$HERE/checklist.md" \
    --json "$HERE/current-run-results.json" --subject "$SUBJECT_DESCRIPTION" \
    --tiers "$tiers" --status "$status" --fingerprint auto --subject-root "$subject_root" \
    --base-commit "$base_commit" --worktree-state "$worktree_state" \
    --routing-config "$ROUTING_CONFIG"
}
cleanup() {
  local status="$1"
  [ "$FAIL_COUNT" -gt 0 ] && status=1
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  # Wait only for servers started by the harness. A bare `wait` also includes
  # the process-substitution `tee` on Linux, which cannot exit until this shell
  # closes stdout and therefore deadlocks cleanup.
  for p in "${PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  echo "  [cleanup] stopped background processes; work dir: $WORK"
  if ! write_reports "$status"; then
    echo "  [report] ERROR: structured report generation or mandatory-result validation failed"
    status=1
  elif python3 "$HERE/render.py" --no-open >/dev/null 2>&1 &&
       python3 "$HERE/scripts/report_results.py" validate \
         --json "$HERE/current-run-results.json" --summary "$HERE/summary.md" \
         --checklist "$HERE/checklist.md" --html "$HERE/report.html"; then
    echo "  [summary] $HERE/summary.md"
    echo "  [checklist] $HERE/checklist.md"
    echo "  [report] $HERE/report.html"
  else
    echo "  [report] ERROR: rendering or report-integrity validation failed"
    status=1
  fi
  trap - EXIT
  exit "$status"
}
trap 'cleanup $?' EXIT

hr() { printf '\n=== %s ===\n' "$1"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1 is required"; exit 1; }; }

# Register the mandatory A/B checks before execution. Report generation then
# fails if the script exits or branches around any promised result.
for mandatory_id in REPORT-SELF BUILD A-FULL A-RACE A-VET A-ROUTING A-FEATURE T7-PRIVACY A-XOVER A-SENS A-ECON; do
  expect_result "$mandatory_id"
done
[ "$DO_TIER_A_ONLY" = 0 ] && for mandatory_id in B-START T2-REQUEST T2-INSPECT T4; do
  expect_result "$mandatory_id"
done
[ "$DO_MINE" = 1 ] && expect_result T5-DIST

hr "prerequisites"
need go; need python3; need git
[ "$DO_LOCAL" = 1 ] && need rsync
echo "  go      $(go version | awk '{print $3}')"
echo "  python  $(python3 --version | awk '{print $2}')"
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && { exec 3>&-; return 0; } || return 1; }
if [ "$DO_TIER_A_ONLY" = 0 ]; then
  for p in $PORT_MOCK $PORT_OCTO; do
    port_busy "$p" && { echo "  ERROR: port $p already in use — free it first (stale test process?)"; exit 1; }
  done
  echo "  ports $PORT_MOCK/$PORT_OCTO free"
else
  echo "  mode    Tier A only (no listeners or network calls)"
fi
run_check REPORT-SELF setup yes "Report generator self-tests" "structured-result and mandatory-check regression tests" \
  python3 "$HERE/scripts/test_report_results.py"

cd "$WORK"
if [ "$DO_LOCAL" = 1 ]; then
  LOCAL_SUBJECT="${LOCAL_OCTOPUS:-$HERE/..}"
  LOCAL_ROOT="$(git -C "$LOCAL_SUBJECT" rev-parse --show-toplevel 2>/dev/null)" || {
    echo "  ERROR: local subject is not a Git working tree: $LOCAL_SUBJECT"
    exit 1
  }
  hr "copying local subject"
  mkdir -p "$WORK/octopus"
  # Copy tracked files plus untracked, non-ignored files from the working tree.
  # This includes current edits while excluding ignored credentials, config,
  # build output, and Git metadata. Exclude this evaluation directory itself.
  (
    cd "$LOCAL_ROOT"
    git ls-files -z --cached --others --exclude-standard |
      while IFS= read -r -d '' path; do
        case "$path" in
          octopus-eval|octopus-eval/*) continue ;;
        esac
        [ -e "$path" ] || [ -L "$path" ] || continue
        printf '%s\0' "$path"
      done |
      rsync -a --from0 --files-from=- ./ "$WORK/octopus/"
  ) || { echo "local working-tree copy failed"; exit 1; }
  cd "$WORK/octopus"
  echo "  mode        local snapshot (no clone or checkout)"
  echo "  source      current working tree at $LOCAL_ROOT"
  echo "  contents    modified and untracked non-ignored source files included"
  SUBJECT_DESCRIPTION="current local working tree at $LOCAL_ROOT (local snapshot; no clone or checkout)"
else
  hr "fetching subject @ $PIN"
  # Full clone: a --depth 1 clone only works while $PIN is still the tip.
  git clone -q https://github.com/sausheong/octopus.git octopus || { echo "clone failed"; exit 1; }
  cd octopus
  git checkout -q "$PIN" 2>/dev/null || { echo "  WARNING: commit $PIN not found; using tip $(git rev-parse --short HEAD)"; }
  echo "  HEAD $(git rev-parse --short HEAD)"
  SUBJECT_DESCRIPTION="github.com/sausheong/octopus @ $(git rev-parse --short HEAD) (pinned reproduction)"
fi

mkdir -p cmd/octotest && cp "$HERE/scripts/octotest-main.go" cmd/octotest/main.go
run_check BUILD setup yes "Build headless Octopus" "GOWORK=off go build ./cmd/octotest" \
  env GOWORK=off go build -o "$WORK/octotest" ./cmd/octotest
cat "$LAST_OUTPUT"
[ "$LAST_STATUS" -eq 0 ] || exit 1
echo "  built octotest (headless entrypoint — macOS cmd/octopus hard-codes ~/.octopus and opens a menubar app)"

hr "TIER A — offline, deterministic"
run_check A-FULL A yes "Complete Octopus test suite" "unmodified subject; GOWORK=off go test ./..." \
  env GOWORK=off go test ./...
tail -2 "$LAST_OUTPUT" || true
run_check A-RACE A yes "Complete Octopus race suite" "unmodified subject; GOWORK=off go test -race ./..." \
  env GOWORK=off go test -race ./...
tail -2 "$LAST_OUTPUT" || true
run_check A-VET A yes "Static Go analysis" "unmodified subject; GOWORK=off go vet ./..." \
  env GOWORK=off go vet ./...
tail -2 "$LAST_OUTPUT" || true

# Evaluation-only tests are injected only after the complete product suite, so
# their results cannot be mistaken for product-owned test coverage.
cp "$HERE"/scripts/terra_*.go router/
run_check A-ROUTING A yes "Production and legacy routing evaluations" "production absolute/amortized scenarios plus labelled legacy comparisons" \
  env GOWORK=off go test ./router -run 'TestTerra|TestSweep|TestCache|TestWhere|TestLocal|TestWhy|TestOpus' -count=1
tail -2 "$LAST_OUTPUT" || true
run_check A-FEATURE A yes "Classifier and routing feature evaluations" "classifier cache, sticky bypass, background isolation, and workflow affinity" \
  env GOWORK=off go test ./router -run '^TestEvalFeature' -count=1
tail -2 "$LAST_OUTPUT" || true
run_check T7-PRIVACY A yes "Local-only privacy boundary" "config, classifier, eligibility, and fallback tests" \
  env GOWORK=off go test ./config ./router ./server -run 'TestParseAndMarshalDataPolicy|TestValidateLocalProvider|TestProviderLocation|TestLocalOnly|TestPreferLocal'
tail -2 "$LAST_OUTPUT" || true
run_check A-XOVER A yes "Cumulative switching crossover" "deterministic Python model" \
  python3 "$HERE/scripts/t6b_breakeven.py"
grep -E "crossover" "$LAST_OUTPUT" | sed 's/^/ /' || true
run_check A-SENS A yes "Routing sensitivity" "seeded catalogue sweep" \
  python3 "$HERE/scripts/t3c_sensitivity.py"
grep -E "production absolute per-turn retain rate|legacy relative per-turn retain rate" "$LAST_OUTPUT" | sed 's/^/  /' || true
run_check A-ECON A yes "Per-turn switching economics" "deterministic cost model" \
  python3 "$HERE/scripts/t6_economics.py"
grep -E "break-even|switch turn" "$LAST_OUTPUT" | head -4 | sed 's/^/  /' || true

if [ "$DO_TIER_A_ONLY" = 1 ]; then
  hr "done — Tier A"
  exit 0
fi

hr "TIER B — local mock upstream (no network, no spend)"
CAP="$WORK/cap"; mkdir -p "$CAP"
python3 "$HERE/scripts/mock_upstream.py" $PORT_MOCK "$CAP" >/dev/null 2>&1 & PIDS+=($!)
sleep 1
"$WORK/octotest" --config "$HERE/configs/config-mock-nr.yaml" >"$WORK/octotest.log" 2>&1 & PIDS+=($!)
sleep 3
if ! port_busy $PORT_OCTO; then
  record B-START B yes FAIL "Local test services" "start mock and Octopus" "Octopus did not become ready"
  echo "  ERROR: octotest failed to start; see $WORK/octotest.log"; exit 1
fi
record B-START B yes PASS "Local test services" "start mock and Octopus" "listeners became ready"

echo "  T2 — semantic request capture (what the client sent vs what upstream received):"
if curl -sf "http://127.0.0.1:$PORT_OCTO/v1/messages" -H 'content-type: application/json' \
     -X POST --data-binary @"$HERE/configs/probe.json" >/dev/null; then
  record T2-REQUEST B yes PASS "Request reaches mock upstream" "Anthropic request through Octopus" "capture created"
else
  record T2-REQUEST B yes FAIL "Request reaches mock upstream" "Anthropic request through Octopus" "request failed"
fi
python3 - "$CAP/req-0001.bin" "$HERE/configs/probe.json" <<'PY'
import json,sys
got=json.load(open(sys.argv[1])); sent=json.load(open(sys.argv[2]))
assert got['system'][0]['text'] == sent['system'][0]['text']
assert got['messages'][0]['content'][0]['text'] == sent['messages'][0]['content'][0]['text']
assert got['system'][0]['cache_control'] == sent['system'][0]['cache_control']
assert got['messages'][0]['content'][0]['cache_control'] == sent['messages'][0]['content'][0]['cache_control']
assert got['tools'][0]['name'] == sent['tools'][0]['name']
assert got['tools'][0]['input_schema'] == sent['tools'][0]['input_schema']
assert got['tools'][0]['input_schema']['required'] == sent['tools'][0]['input_schema']['required']
print("    sent tool props    :", list(sent['tools'][0]['input_schema']['properties']))
print("    received tool props:", list(got['tools'][0]['input_schema']['properties']), " <- deterministic JSON key order")
print("    schema semantics   : preserved")
print("    end-user metadata  : routing-only; upstream forwarding awaits Harness support")
print("    upstream stream    :", bool(got.get('stream')), "(matches buffered client)")
print("    cache_control kept :", got['system'][0].get('cache_control'))
PY
inspect_status=$?
if [ "$inspect_status" -eq 0 ]; then
  record T2-INSPECT B yes PASS "Captured request is valid" "parse upstream JSON" "request fields reported"
else
  record T2-INSPECT B yes FAIL "Captured request is valid" "parse upstream JSON" "inspection failed"
fi
echo "  T4 — latency (small payload):"
run_check T4 B yes "Proxy latency overhead" "120 requests against local mock" \
  python3 "$HERE/scripts/t4_latency.py" 120
tail -3 "$LAST_OUTPUT" | sed 's/^/   /' || true

if [ "$DO_OLLAMA" = 1 ]; then
  hr "TIER C — Ollama (local inference)"
  if ! command -v ollama >/dev/null 2>&1; then
    echo "  SKIP: ollama not installed"
    record T5 C no SKIP "Ollama evaluation" "local Ollama" "ollama is not installed"
  elif ! curl -sf -m 5 http://localhost:11434/v1/models >/dev/null 2>&1; then
    echo "  SKIP: ollama not running (start it, then: ollama pull qwen2.5:3b)"
    record T5 C no SKIP "Ollama evaluation" "local Ollama" "Ollama is not running"
  else
    expect_result T5-INFER
    expect_result T5-CONTEXT
    "$WORK/octotest" --config "$HERE/configs/config-ollama.yaml" >"$WORK/octo-ollama.log" 2>&1 & PIDS+=($!)
    sleep 3
    echo "  real inference through Octopus -> Ollama:"
    if curl -sf -m 180 "http://127.0.0.1:$PORT_OLLAMA_OCTO/v1/messages" -H 'content-type: application/json' \
      -X POST -d '{"model":"ollama/qwen2.5:3b","max_tokens":32,"messages":[{"role":"user","content":"Reply with exactly: local inference works"}]}' \
      >"$WORK/ollama-response.json"; then
      head -c 300 "$WORK/ollama-response.json" | sed 's/^/    /'; echo
      record T5-INFER C yes PASS "Inference through Octopus and Ollama" "real local request" "HTTP response received"
    else
      record T5-INFER C yes FAIL "Inference through Octopus and Ollama" "real local request" "request failed"
    fi
    echo "  effective context probe (measured bounds; no assumed limit):"
    run_check T5-CONTEXT C yes "Effective Ollama context" "progressive prompts with truncation detection" \
      python3 "$HERE/scripts/t5c_ollama_numctx.py"
    cat "$LAST_OUTPUT" | sed 's/^/   /'
  fi
fi

if [ "$DO_MINE" = 1 ]; then
  hr "TIER D — YOUR transcript context distribution (expect different numbers to ours)"
  run_check T5-DIST D yes "Transcript context distribution" "deduplicated local Claude transcripts" \
    python3 "$HERE/scripts/t5_ctx_distribution.py"
  cat "$LAST_OUTPUT" | sed 's/^/  /'
fi

if [ "$DO_LIVE" = 1 ]; then
  hr "TIER E — live endpoint (SPENDS MONEY)"
  KEY_SET="${EVAL_API_KEY:-${OPENAI_API_KEY:-${ANTHROPIC_API_KEY:-}}}"
  if [ -z "$KEY_SET" ]; then
    echo "  SKIP: no key. Set EVAL_API_KEY (or OPENAI_API_KEY / ANTHROPIC_API_KEY)."
    echo "        Direct Anthropic works too:"
    echo "          export EVAL_BASE=https://api.anthropic.com"
    echo "          export EVAL_MODEL=claude-haiku-4-5-20251001"
    echo "          export EVAL_API_KEY=\$ANTHROPIC_API_KEY"
    record T1 E no SKIP "Live cache evaluation" "paid endpoint" "no API key configured"
  else
    expect_result T1-DIRECT
    expect_result T1-OCTO
    expect_result T9
    LIVE_CFG="$HERE/configs/config-litellm.yaml"
    case "${EVAL_BASE:-}" in *api.anthropic.com*) LIVE_CFG="$HERE/configs/config-anthropic.yaml";; esac
    echo "  endpoint: ${EVAL_BASE:-https://litellm-stg.aip.gov.sg}   config: $(basename "$LIVE_CFG")"
    echo "  T1 baseline (direct):"
    run_check T1-DIRECT E yes "Direct cache baseline" "two live requests" \
      python3 "$HERE/scripts/t1_cache.py" direct
    grep -E "cache_(creation|read)" "$LAST_OUTPUT" | sed 's/^/   /' || true
    last_pid_index=$((${#PIDS[@]} - 1))
    kill "${PIDS[$last_pid_index]}" 2>/dev/null; sleep 1
    "$WORK/octotest" --config "$LIVE_CFG" >"$WORK/octo-live.log" 2>&1 & PIDS+=($!)
    sleep 3
    echo "  T1 through Octopus:"
    run_check T1-OCTO E yes "Cache through Octopus" "two live routed requests" \
      python3 "$HERE/scripts/t1_cache.py" octopus
    grep -E "cache_(creation|read)" "$LAST_OUTPUT" | sed 's/^/   /' || true
    echo "  T9 which layer holds the cache:"
    run_check T9 E yes "Cache-layer identification" "live prefix-control requests" \
      python3 "$HERE/scripts/t9_prefix_control.py"
    tail -6 "$LAST_OUTPUT" | sed 's/^/   /' || true
  fi
fi

hr "done"
echo "  Mandatory failures:  $FAIL_COUNT"
echo "  Summary/checklist/report are generated automatically during cleanup."
if [ "$DO_LOCAL" = 1 ]; then
  echo "  Tiers C/D locally:   ./run.sh --ollama --mine"
else
  echo "  Tiers C/D:           ./run.sh --ollama --mine"
fi
echo "  Tier E (paid):       ./run.sh --live"
[ "$FAIL_COUNT" -eq 0 ]
