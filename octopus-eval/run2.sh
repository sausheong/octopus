#!/usr/bin/env bash
# Paid paired evaluation of Octopus routing versus running every turn on Opus.
#
#   ./run2.sh --yes             run workload plus blinded Opus judging
#   ./run2.sh --yes --no-judge  measure routing and workload cost only
#   ./run2.sh --yes --resume    continue a checkpointed interrupted run
#   ./run2.sh --yes --suite production --production-evidence --budget-usd N
#   ./run2.sh --help            show requirements and generated files
set -euo pipefail
umask 077

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
CONFIG="${RUN2_CONFIG:-$ROOT/config.yaml}"
PORT="${RUN2_PORT:-18787}"
OCTOPUS_BASE="http://127.0.0.1:$PORT"
DIRECT_BASE="${EVAL_BASE:-https://litellm-stg.aip.gov.sg}"
RESULTS="$HERE/run2-results.json"
SUMMARY="$HERE/run2-summary.md"
LOG="$HERE/run2.log"
INSIGHTS="$HERE/run2-insights.json"
CONFIRMED=0
NO_JUDGE=0
RESUME=0
RESTART_OCTOPUS=0
SUITE="smoke"
SUITE_SET=0
SCENARIO_FILE=""
SEED=""
BUDGET=""
OUTPUT_PREFIX="run2"
RUN_ID=""
ARMS="octopus,opus"
JUDGE_MODELS="claude-opus-4-8-global"
CONCURRENCY=1
PRODUCTION_EVIDENCE=0

usage() {
  sed -n '2,7p' "$0"
  cat <<'EOF'

Requirements:
  - macOS/Linux shell, Go, Python 3, curl, and lsof
  - EVAL_API_KEY or ANTHROPIC_AUTH_TOKEN for the configured gateway
  - config.yaml containing only the LiteLLM Haiku, Sonnet, and Opus entries

The run makes paid model calls. Workload and judge costs are reported
separately. Generated files: run2-results.json, run2-summary.md, run2.log.

Scenario selection:
  --suite NAME         tracked scenarios/NAME.json suite (default: smoke)
  --scenario-file PATH operator-supplied schema-version-1 JSON suite
  --seed INTEGER       reproducible arm order (default derives from run id)
  --budget-usd AMOUNT  stop after observed response spend reaches this limit
  --output-prefix NAME write NAME-results.json, NAME-summary.md, and related files
  --run-id ID          stable identifier recorded in checkpoints and results
  --production-evidence all fixed baselines, two judges, fifty concurrent streams
  --arms LIST          comma-separated octopus,haiku,sonnet,opus (advanced)
  --judge-models LIST  comma-separated catalogue model IDs (advanced)
  --concurrency N      isolated Octopus conversations launched in parallel
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --yes) CONFIRMED=1 ;;
    --no-judge) NO_JUDGE=1 ;;
    --resume) RESUME=1 ;;
    --suite)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      SUITE="$2"
      SUITE_SET=1
      shift
      ;;
    --scenario-file)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      SCENARIO_FILE="$2"
      shift
      ;;
    --seed)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      SEED="$2"
      shift
      ;;
    --budget-usd)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      BUDGET="$2"
      shift
      ;;
    --output-prefix)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      OUTPUT_PREFIX="$2"
      shift
      ;;
    --run-id)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      RUN_ID="$2"
      shift
      ;;
    --production-evidence)
      PRODUCTION_EVIDENCE=1
      ARMS="octopus,haiku,sonnet,opus"
      JUDGE_MODELS="claude-opus-4-8-global,claude-sonnet-4-6-asia-southeast1"
      CONCURRENCY=50
      ;;
    --arms)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      ARMS="$2"; shift ;;
    --judge-models)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      JUDGE_MODELS="$2"; shift ;;
    --concurrency)
      [ "$#" -ge 2 ] || { printf '%s requires a value\n' "$1" >&2; exit 2; }
      CONCURRENCY="$2"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

case "$OUTPUT_PREFIX" in
  *[!A-Za-z0-9._-]*|'') printf '%s\n' '--output-prefix must be a safe file-name prefix' >&2; exit 2 ;;
esac
RESULTS="$HERE/${OUTPUT_PREFIX}-results.json"
SUMMARY="$HERE/${OUTPUT_PREFIX}-summary.md"
LOG="$HERE/${OUTPUT_PREFIX}.log"
INSIGHTS="$HERE/${OUTPUT_PREFIX}-insights.json"

if [ -n "$SCENARIO_FILE" ] && [ "$SUITE_SET" -eq 1 ]; then
  printf '%s\n' '--suite and --scenario-file are mutually exclusive' >&2
  exit 2
fi

if [ "$CONFIRMED" -ne 1 ]; then
  printf 'This evaluation makes paid Haiku, Sonnet, and Opus calls.\n' >&2
  printf 'Run %s --yes after reviewing config.yaml and the prompt suite.\n' "$0" >&2
  exit 2
fi

for command_name in go python3 curl lsof sed shasum git; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf '%s is required\n' "$command_name" >&2
    exit 1
  }
done

suite_info="$(PYTHONPATH="$HERE/scripts" python3 -c 'import sys; from run2_eval import load_scenario_suite; s,m=load_scenario_suite(sys.argv[1], sys.argv[2] or None); print(m["name"], len(s), sum(len(x["turns"]) for x in s), sep="\t")' "$SUITE" "$SCENARIO_FILE")"
IFS=$'\t' read -r SUITE_NAME SCENARIO_COUNT TURN_COUNT <<<"$suite_info"
ARM_COUNT="$(python3 -c 'import sys; print(len(set(filter(None,sys.argv[1].split(",")))))' "$ARMS")"
JUDGE_COUNT="$(python3 -c 'import sys; print(len(set(filter(None,sys.argv[1].split(",")))))' "$JUDGE_MODELS")"
JUDGE_CALLS="$((SCENARIO_COUNT * JUDGE_COUNT))"
[ "$NO_JUDGE" -eq 1 ] && JUDGE_CALLS=0
OUTPUT_CEILING="$(python3 -c 'import sys; turns,arms,judges,streams=map(int,sys.argv[1:]); print(f"{((turns*arms+judges+streams*4)*6000*75/1_000_000):.2f}")' "$TURN_COUNT" "$ARM_COUNT" "$JUDGE_CALLS" "$CONCURRENCY")"
if [ "$SUITE_NAME" = production ] && { [ -z "$BUDGET" ] || [ "$PRODUCTION_EVIDENCE" -ne 1 ]; }; then
  printf 'The production suite requires --production-evidence and --budget-usd AMOUNT.\n' >&2
  exit 2
fi
if [ -n "$BUDGET" ]; then
  python3 -c 'import sys; assert float(sys.argv[1]) > 0' "$BUDGET" 2>/dev/null || {
    printf '%s\n' '--budget-usd must be a positive number' >&2
    exit 2
  }
fi

RUN2_KEY="${EVAL_API_KEY:-${ANTHROPIC_AUTH_TOKEN:-}}"
if [ -z "$RUN2_KEY" ]; then
  printf 'Set EVAL_API_KEY or ANTHROPIC_AUTH_TOKEN for the configured gateway.\n' >&2
  exit 1
fi
export ANTHROPIC_AUTH_TOKEN="$RUN2_KEY"

[ -f "$CONFIG" ] || { printf 'configuration not found: %s\n' "$CONFIG" >&2; exit 1; }
catalog_count="$(grep -c '^  - id:' "$CONFIG" || true)"
if [ "$catalog_count" -ne 3 ] ||
   ! grep -q 'claude-haiku' "$CONFIG" ||
   ! grep -q 'claude-sonnet' "$CONFIG" ||
   ! grep -q 'claude-opus' "$CONFIG"; then
  printf 'run2 requires config.yaml to contain exactly Haiku, Sonnet, and Opus.\n' >&2
  exit 1
fi

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  printf 'port %s is already in use; set RUN2_PORT to a free port\n' "$PORT" >&2
  exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/octopus-run2.XXXXXX")"
PID=""
cleanup() {
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT HUP INT TERM

exec > >(tee "$LOG") 2>&1

if [ "$RESUME" -eq 0 ]; then
  rm -f "$INSIGHTS" "$RESULTS.partial"
elif [ ! -f "$INSIGHTS" ] || [ ! -f "$RESULTS.partial" ]; then
  printf 'cannot resume: run2-insights.json and run2-results.json.partial are both required\n' >&2
  exit 1
else
  workload_complete="$(python3 -c 'import json,sys; p=json.load(open(sys.argv[1])); r=p.get("runs",{}); ids=p.get("suite",{}).get("scenario_ids",[]); arms=p.get("execution",{}).get("arms",[]); print("yes" if ids and arms and set(r)==set(ids) and all(all(r[i].get(a,{}).get("complete") for a in arms) for i in ids) else "no")' "$RESULTS.partial")"
  if [ "$workload_complete" != yes ]; then
    # An interrupted arm can have observations that were persisted before the
    # arm checkpoint. Reset routing economics and rerun only the cheaper
    # Octopus arms; completed all-Opus arms remain checkpointed.
    budget_checkpoint="$(python3 -c 'import json,sys; print("yes" if json.load(open(sys.argv[1])).get("execution",{}).get("budget_stop_after_checkpoint") else "no")' "$RESULTS.partial")"
    if [ "$budget_checkpoint" != yes ]; then
      rm -f "$INSIGHTS"
      RESTART_OCTOPUS=1
    fi
  fi
fi

printf '\n=== run2: paired Octopus versus all-Opus evaluation ===\n'
printf '  config       %s\n' "$CONFIG"
printf '  Octopus      %s\n' "$OCTOPUS_BASE"
printf '  direct API   %s\n' "$DIRECT_BASE"
printf '  judging      %s\n' "$([ "$NO_JUDGE" -eq 1 ] && printf disabled || printf 'enabled (cost reported separately)')"
printf '  workload     %s scenarios, %s turns per arm\n' "$SCENARIO_COUNT" "$TURN_COUNT"
printf '  arms         %s\n' "$ARMS"
printf '  judges       %s\n' "$JUDGE_MODELS"
printf '  concurrency  %s isolated streams\n' "$CONCURRENCY"
printf '  output cap   $%s if every call used all 6,000 output tokens; input and classifier spend are additional\n' "$OUTPUT_CEILING"
printf '  budget       %s\n' "$([ -n "$BUDGET" ] && printf '$%s observed response spend' "$BUDGET" || printf 'not set (smoke/custom suite)')"
if [ -n "$SCENARIO_FILE" ]; then
  printf '  scenarios    %s\n' "$SCENARIO_FILE"
else
  printf '  suite        %s\n' "$SUITE"
fi

printf '\n=== build isolated headless Octopus ===\n'
(
  cd "$ROOT"
  GOWORK=off go build -trimpath -o "$WORK/octotest-insights" "$HERE/scripts/octotest-insights-main.go"
)
cp "$CONFIG" "$WORK/config.yaml"
sed -i.bak -E "s#addr: \"127\\.0\\.0\\.1:[0-9]+\"#addr: \"127.0.0.1:$PORT\"#" "$WORK/config.yaml"
rm -f "$WORK/config.yaml.bak"
SOURCE_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
BINARY_SHA256="$(shasum -a 256 "$WORK/octotest-insights" | awk '{print $1}')"
CONFIG_SHA256="$(shasum -a 256 "$CONFIG" | awk '{print $1}')"
POLICY_DIGEST="$CONFIG_SHA256"
PROVIDER_IDENTITY="$(python3 -c 'import sys,urllib.parse; u=urllib.parse.urlsplit(sys.argv[1]); print("%s://%s" % (u.scheme, u.hostname or "unknown"))' "$DIRECT_BASE")"

printf '\n=== start Octopus with a fresh Insights ledger ===\n'
"$WORK/octotest-insights" \
  --config "$WORK/config.yaml" \
  --insights "$INSIGHTS" \
  >"$WORK/octopus.log" 2>&1 &
PID=$!

ready=0
for _ in $(seq 1 60); do
  if curl -fsS "$OCTOPUS_BASE/v1/models" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$PID" 2>/dev/null; then
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  printf 'Octopus did not become ready. Log follows:\n' >&2
  sed -n '1,240p' "$WORK/octopus.log" >&2
  exit 1
fi

printf '\n=== paid workload ===\n'
python_args=(
  "$HERE/scripts/run2_eval.py"
  --octopus-base "$OCTOPUS_BASE"
  --direct-base "$DIRECT_BASE"
  --api-key-env ANTHROPIC_AUTH_TOKEN
  --insights "$INSIGHTS"
  --results "$RESULTS"
  --summary "$SUMMARY"
)
if [ -n "$SCENARIO_FILE" ]; then
  python_args+=(--scenario-file "$SCENARIO_FILE")
else
  python_args+=(--suite "$SUITE")
fi
if [ -n "$SEED" ]; then
  python_args+=(--seed "$SEED")
fi
if [ -n "$BUDGET" ]; then
  python_args+=(--budget-usd "$BUDGET")
fi
if [ -n "$RUN_ID" ]; then
  python_args+=(--run-id "$RUN_ID")
fi
python_args+=(--arms "$ARMS" --judge-models "$JUDGE_MODELS" --concurrency "$CONCURRENCY")
python_args+=(
  --source-commit "$SOURCE_COMMIT"
  --binary-sha256 "$BINARY_SHA256"
  --config-sha256 "$CONFIG_SHA256"
  --policy-digest "$POLICY_DIGEST"
  --provider-identity "$PROVIDER_IDENTITY"
)
if [ "$NO_JUDGE" -eq 1 ]; then
  python_args+=(--no-judge)
fi
if [ "$RESUME" -eq 1 ]; then
  python_args+=(--resume)
fi
if [ "$RESTART_OCTOPUS" -eq 1 ]; then
  python_args+=(--restart-octopus)
fi
python3 "${python_args[@]}"

printf '\n=== completed ===\n'
printf '  summary   %s\n' "$SUMMARY"
printf '  raw data %s\n' "$RESULTS"
printf '  log       %s\n' "$LOG"
