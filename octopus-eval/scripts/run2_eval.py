#!/usr/bin/env python3
"""Live paired evaluation: Octopus routing versus an all-Opus baseline.

The workload consists of multi-turn conversations whose difficulty rises and
falls. Both arms receive the same prompts, but each arm keeps its own generated
conversation history. Costs use provider-reported token and cache usage. A
blinded Opus judge scores the completed conversations; judge spend is reported
separately and is never counted as workload cost.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import os
import random
import re
import time
import urllib.error
import urllib.request
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path


HAIKU = "claude-haiku-4-5@20251001-global"
SONNET = "claude-sonnet-4-6-asia-southeast1"
OPUS = "claude-opus-4-8-global"
PRICES = {
    HAIKU: (1.0, 5.0),
    SONNET: (3.0, 15.0),
    OPUS: (15.0, 75.0),
}
ARM_MODELS = {"octopus": "octopus", "haiku": HAIKU, "sonnet": SONNET, "opus": OPUS}
CACHE_READ_MULTIPLIER = 0.10
CACHE_WRITE_MULTIPLIER = 1.25
SUITE_DIR = Path(__file__).parents[1] / "scenarios"
VALID_DIFFICULTIES = {"trivial", "low", "medium", "high"}

CORE_REFERENCE = """Octopus evaluation reference manual, revision 2026-08-07.

Customer and policy facts:
- Customer C-017 is on the Gold plan, is based in Singapore, renewed on
  2026-07-01, disputed the renewal on 2026-07-20, and had a renewal-notice
  email bounce.
- Gold refunds are allowed within 45 days; Standard refunds within 30 days;
  Trial cancellations within 7 days.
- When rules conflict, statutory obligations outrank signed contracts, which
  outrank the internal handbook. Agents must not invent legal conclusions.
- A bounced required notice must be escalated to Compliance before a final
  refund decision. Customer replies should acknowledge the issue without
  promising an outcome.

Business data:
- Gross subscriptions were 1,200 in January, 1,350 in February, and 1,110 in
  March. Refunds were 60, 90, and 30 respectively. Quarterly net subscriptions
  therefore equal 3,480.
- The migration window is Sunday 01:00-03:00 SGT, maximum read-only time is
  10 minutes, rollback must complete within 15 minutes, and payment records
  must remain auditable. The team has six engineers and one database specialist.

Engineering and security rules:
- Cancellation must propagate through Go contexts. Goroutines must have a
  documented owner and exit path. Shared maps require synchronisation.
- Never build SQL by concatenating user input. Use parameters, least privilege,
  bounded request sizes, and redacted structured logs.
- Production migrations require a dry run, measured rollback, reconciliation,
  explicit go/no-go criteria, and named incident ownership.

Writing rules:
- Use plain British English, concrete recommendations, and calibrated claims.
- Do not claim certainty when evidence is incomplete. Preserve important
  qualifications while removing repetition.
"""

# A stable, realistic long prefix makes provider prompt-cache behaviour visible.
# Archive rows are intentionally irrelevant; a good answer should ignore them.
ARCHIVE = "\n".join(
    f"Archive row {index:03d}: control sample {index % 17:02d}; retained for audit; "
    "not authoritative for customer, policy, financial, engineering, or security decisions."
    for index in range(1, 261)
)
SYSTEM_TEXT = (
    "You are completing a controlled evaluation. Follow the current user request, "
    "use the authoritative reference manual when relevant, ignore irrelevant archive "
    "rows, state uncertainty honestly, and keep answers concise unless analysis is requested.\n\n"
    + CORE_REFERENCE
    + "\nHistorical archive (non-authoritative):\n"
    + ARCHIVE
)


SCENARIOS = [
    {
        "id": "customer_support",
        "name": "Customer support and policy",
        "turns": [
            ("trivial", "Customer C-017 is on which plan? Reply with the plan name only."),
            ("low", "Draft a two-sentence acknowledgement of C-017's refund request. Do not promise an outcome."),
            (
                "high",
                "Analyse C-017's disputed renewal in detail. Reconcile the plan refund window, the bounced notice, "
                "the policy-precedence rule, and the limit on making legal conclusions. Give the support agent a "
                "recommended next action, the facts still needing confirmation, and wording that avoids creating "
                "an unauthorised promise. Separate facts, inference, risk, and recommendation.",
            ),
            ("low", "Turn that analysis into a four-bullet hand-off for the Compliance queue."),
        ],
    },
    {
        "id": "go_debugging",
        "name": "Go debugging and concurrency",
        "turns": [
            ("trivial", "In one sentence, what does O(n) mean?"),
            (
                "medium",
                "Fix this Go function so it cannot race when called concurrently. Return code and one sentence of explanation:\n"
                "var totals = map[string]int{}\nfunc add(k string, n int) { totals[k] += n }",
            ),
            (
                "high",
                "Now design the production version. Many workers update per-account totals while a periodic snapshot is "
                "written to durable storage. Shutdown can happen at any point. Explain ownership, locking or message-passing, "
                "context cancellation, bounded backpressure, snapshot consistency, error propagation, and how tests would "
                "prove there are no leaked goroutines or lost acknowledged updates. Include compact Go pseudocode.",
            ),
            ("medium", "Give a minimal implementation checklist ordered by the failures it prevents."),
        ],
    },
    {
        "id": "business_analysis",
        "name": "Business data analysis",
        "turns": [
            ("trivial", "What were quarterly net subscriptions? Reply with the number only."),
            (
                "medium",
                "Write parameterised PostgreSQL that returns monthly gross subscriptions, refunds, net subscriptions, "
                "and refund rate for a supplied start and end date. State how division by zero is handled.",
            ),
            (
                "high",
                "The refund rate rose in February and net subscriptions fell in March. Develop a careful investigation plan "
                "that distinguishes seasonality, acquisition mix, product defects, billing errors, and policy changes. Specify "
                "the cohort cuts, counterfactuals, data-quality checks, and stopping conditions needed before claiming a cause. "
                "Do not infer causation from the three aggregate rows alone.",
            ),
            ("low", "Summarise the decision for an executive in three bullets, preserving the main uncertainty."),
        ],
    },
    {
        "id": "security_review",
        "name": "Application security review",
        "turns": [
            (
                "trivial",
                "Name the main vulnerability in: query := \"SELECT * FROM users WHERE email='\" + email + \"'\"",
            ),
            ("medium", "Replace it with safe Go database/sql code and avoid revealing whether an account exists."),
            (
                "high",
                "Threat-model the complete password-reset flow for an internet service. Cover enumeration, token entropy and "
                "storage, replay, expiry, race conditions, session invalidation, email-link leakage, rate limits, audit logging, "
                "operator access, and denial-of-service trade-offs. Prioritise mitigations by likelihood and impact, and identify "
                "what must be tested rather than merely asserted.",
            ),
            ("low", "Produce a P0/P1/P2 remediation list with no more than two items per priority."),
        ],
    },
    {
        "id": "migration_planning",
        "name": "Production migration planning",
        "turns": [
            ("trivial", "How long is the stated Sunday migration window? Reply with the duration only."),
            ("medium", "Compare rolling and blue-green deployment for the stated payment-system constraints in a small table."),
            (
                "high",
                "Design a minute-by-minute migration plan that fits the stated window, six-engineer team, database-specialist "
                "constraint, 10-minute read-only limit, 15-minute rollback limit, and auditability requirement. Include role "
                "assignment, preflight evidence, data reconciliation, go/no-go gates, observability, rollback triggers, customer "
                "communication, and the exact evidence required before declaring success. Flag any timing assumption that has "
                "not actually been proven.",
            ),
            ("medium", "Write the final go/no-go checklist as ten yes-or-no questions."),
        ],
    },
    {
        "id": "technical_writing",
        "name": "Technical writing and synthesis",
        "turns": [
            ("trivial", "Correct this sentence: 'The results proves the system are cheaper.'"),
            (
                "medium",
                "Rewrite this claim in plain, careful English: 'Octopus always picks the perfect model and guarantees massive "
                "savings without affecting quality.' Keep it to two sentences.",
            ),
            (
                "high",
                "Write a 350-word internal note explaining how a model router can reduce cost without pretending that model "
                "quality, number of turns, cache reuse, and future task length are known with certainty. Explain why per-turn "
                "cheapness is not the same as cost to completion, why switching can incur a cache penalty, and what evidence "
                "an evaluation must show before claiming superior results. Use plain British English and a decisive conclusion.",
            ),
            ("low", "Give the note a factual title and a one-sentence standfirst."),
        ],
    },
]


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--octopus-base", default="http://127.0.0.1:18787")
    parser.add_argument("--direct-base", default="https://litellm-stg.aip.gov.sg")
    parser.add_argument("--api-key-env", default="ANTHROPIC_AUTH_TOKEN")
    parser.add_argument("--insights", required=True)
    parser.add_argument("--results", required=True)
    parser.add_argument("--summary", required=True)
    parser.add_argument("--no-judge", action="store_true")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--restart-octopus", action="store_true")
    parser.add_argument("--run-id")
    parser.add_argument("--seed", type=int, help="arm-order seed; defaults to a run-id-derived seed")
    parser.add_argument("--pause", type=float, default=1.5)
    parser.add_argument("--budget-usd", type=float, help="stop when observed API response spend reaches this limit")
    parser.add_argument("--arms", default="octopus,opus", help="comma-separated: octopus,haiku,sonnet,opus")
    parser.add_argument("--judge-models", default=OPUS, help="comma-separated judge model IDs")
    parser.add_argument("--concurrency", type=int, default=1, help="parallel isolated conversation streams")
    parser.add_argument("--source-commit", default="unknown")
    parser.add_argument("--binary-sha256", default="unknown")
    parser.add_argument("--config-sha256", default="unknown")
    parser.add_argument("--policy-digest", default="unknown")
    parser.add_argument("--provider-identity", default="unknown")
    suite_group = parser.add_mutually_exclusive_group()
    suite_group.add_argument(
        "--suite",
        default="smoke",
        help="tracked suite name under scenarios/ (default: smoke)",
    )
    suite_group.add_argument(
        "--scenario-file",
        type=Path,
        help="external JSON scenario suite; its content digest is checkpointed",
    )
    return parser.parse_args()


class BudgetExceeded(RuntimeError):
    pass


class SpendBudget:
    """Best-effort live spend guard based on provider-reported response usage."""
    def __init__(self, limit, initial=0.0):
        if limit is not None and limit <= 0:
            raise ValueError("--budget-usd must be positive")
        self.limit = limit
        self.spent = initial
        self.lock = threading.Lock()

    def before_request(self):
        with self.lock:
            if self.limit is not None and self.spent >= self.limit:
                raise RuntimeError(f"evaluation budget exhausted at {money(self.spent)}")

    def charge(self, cost):
        with self.lock:
            self.spent += cost
            if self.limit is not None and self.spent > self.limit:
                raise BudgetExceeded(
                    f"evaluation budget exceeded at {money(self.spent)}; "
                    "already-running concurrent calls can also complete"
                )


def parse_csv_choices(value, allowed, label):
    choices = list(dict.fromkeys(item.strip() for item in value.split(",") if item.strip()))
    unknown = [item for item in choices if item not in allowed]
    if not choices or unknown:
        raise ValueError(f"invalid {label}: {', '.join(unknown) if unknown else 'empty selection'}")
    return choices


def load_scenario_suite(suite="smoke", scenario_file=None):
    """Load and validate a tracked or operator-supplied scenario suite."""
    path = Path(scenario_file) if scenario_file else SUITE_DIR / f"{suite}.json"
    try:
        raw = path.read_bytes()
    except FileNotFoundError as exc:
        raise ValueError(f"scenario suite not found: {path}") from exc
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError(f"invalid scenario suite JSON at {path}: {exc}") from exc
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        raise ValueError(f"scenario suite {path} must use schema_version 1")
    name = value.get("name")
    if not isinstance(name, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", name):
        raise ValueError(f"scenario suite {path} has an invalid name")
    source = value.get("scenarios")
    if not isinstance(source, list) or not source:
        raise ValueError(f"scenario suite {path} must contain scenarios")

    shared_final_turn = value.get("shared_final_turn")
    shared_assertions = value.get("shared_assertions", [])
    if not isinstance(shared_assertions, list):
        raise ValueError(f"scenario suite {path} shared_assertions must be a list")
    assertions_by_scenario = value.get("assertions_by_scenario", {})
    if not isinstance(assertions_by_scenario, dict):
        raise ValueError(f"scenario suite {path} assertions_by_scenario must be an object")
    if shared_final_turn is not None:
        if not isinstance(shared_final_turn, dict):
            raise ValueError(f"scenario suite {path} shared_final_turn must be an object")
        shared_difficulty = shared_final_turn.get("difficulty")
        shared_prompt = shared_final_turn.get("prompt")
        if shared_difficulty not in VALID_DIFFICULTIES:
            raise ValueError(f"scenario suite {path} shared_final_turn has invalid difficulty")
        if not isinstance(shared_prompt, str) or not shared_prompt.strip():
            raise ValueError(f"scenario suite {path} shared_final_turn has no prompt")
        shared_final_turn = (shared_difficulty, shared_prompt.strip())

    scenarios = []
    seen = set()
    for index, scenario in enumerate(source, start=1):
        if not isinstance(scenario, dict):
            raise ValueError(f"scenario {index} in {path} must be an object")
        scenario_id = scenario.get("id")
        if not isinstance(scenario_id, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", scenario_id):
            raise ValueError(f"scenario {index} in {path} has an invalid id")
        if scenario_id in seen:
            raise ValueError(f"duplicate scenario id {scenario_id!r} in {path}")
        seen.add(scenario_id)
        display_name = scenario.get("name")
        if not isinstance(display_name, str) or not display_name.strip():
            raise ValueError(f"scenario {scenario_id!r} has no display name")
        domains = scenario.get("domains")
        if not isinstance(domains, list) or not domains or not all(
            isinstance(domain, str) and domain.strip() for domain in domains
        ):
            raise ValueError(f"scenario {scenario_id!r} must declare one or more domains")
        source_turns = scenario.get("turns")
        if not isinstance(source_turns, list) or len(source_turns) < 2:
            raise ValueError(f"scenario {scenario_id!r} must have at least two turns")
        turns = []
        for turn_index, turn in enumerate(source_turns, start=1):
            if not isinstance(turn, dict):
                raise ValueError(f"scenario {scenario_id!r} turn {turn_index} must be an object")
            difficulty = turn.get("difficulty")
            prompt = turn.get("prompt")
            if difficulty not in VALID_DIFFICULTIES:
                raise ValueError(
                    f"scenario {scenario_id!r} turn {turn_index} has invalid difficulty {difficulty!r}"
                )
            if not isinstance(prompt, str) or not prompt.strip():
                raise ValueError(f"scenario {scenario_id!r} turn {turn_index} has no prompt")
            turns.append((difficulty, prompt.strip()))
        if shared_final_turn is not None:
            turns.append(shared_final_turn)
        assertions = (
            list(shared_assertions)
            + list(assertions_by_scenario.get(scenario_id, []))
            + list(scenario.get("assertions", []))
        )
        assertion_ids = set()
        for assertion in assertions:
            if not isinstance(assertion, dict) or not isinstance(assertion.get("id"), str):
                raise ValueError(f"scenario {scenario_id!r} has an invalid assertion")
            if assertion["id"] in assertion_ids:
                raise ValueError(f"scenario {scenario_id!r} has duplicate assertion {assertion['id']!r}")
            assertion_ids.add(assertion["id"])
            if assertion.get("type") not in {"non_empty", "contains", "not_contains", "regex", "max_words", "exact"}:
                raise ValueError(f"scenario {scenario_id!r} assertion {assertion['id']!r} has invalid type")
            turn = assertion.get("turn", "all")
            if turn != "all" and (not isinstance(turn, int) or not 1 <= turn <= len(turns)):
                raise ValueError(f"scenario {scenario_id!r} assertion {assertion['id']!r} has invalid turn")
        scenarios.append({
            "id": scenario_id,
            "name": display_name.strip(),
            "domains": list(dict.fromkeys(domain.strip() for domain in domains)),
            "turns": turns,
            "assertions": assertions,
        })

    unknown_assertion_scenarios = set(assertions_by_scenario) - seen
    if unknown_assertion_scenarios:
        raise ValueError(f"scenario suite {path} has assertions for unknown scenarios")

    metadata = {
        "name": name,
        "path": str(path.resolve()),
        "sha256": hashlib.sha256(raw).hexdigest(),
        "scenario_ids": [scenario["id"] for scenario in scenarios],
    }
    return scenarios, metadata


def model_name(value):
    return (value or "").split("/", 1)[-1]


def cache_namespaces(run_id):
    """Return stable, deliberately isolated cache keys for paired arms."""
    return f"{run_id}:octopus", f"{run_id}:direct"


def arm_cache_namespace(run_id, arm):
    if arm == "opus":
        return f"{run_id}:direct"
    return f"{run_id}:{arm}"


def run_concurrent_streams(
    count, scenarios, octopus_base, direct_base, key, pause, run_id, budget,
    existing_results=None, checkpoint_result=None,
):
    if count <= 1:
        return {"declared_concurrency": 1, "completed_streams": 0, "stream_ids": [], "turns": 0, "cost_usd": 0.0, "results": []}
    active = maximum = 0
    lock = threading.Lock()

    completed = {result["concurrency_index"]: result for result in (existing_results or [])}

    def worker(index, scenario):
        nonlocal active, maximum
        with lock:
            active += 1
            maximum = max(maximum, active)
        try:
            canary = f"OCTOPUS_STREAM_{index:03d}_{hashlib.sha256(f'{run_id}:{index}'.encode()).hexdigest()[:12]}"
            isolated = dict(scenario)
            isolated["turns"] = list(scenario["turns"])
            difficulty, prompt = isolated["turns"][0]
            isolated["turns"][0] = (
                difficulty,
                f"Begin your reply with the exact token {canary}. Do not repeat any other stream token.\n\n{prompt}",
            )
            result = run_arm(
                "octopus", isolated, octopus_base, direct_base, key, pause,
                f"{run_id}:concurrent:{index}", budget,
            )
            result.update({
                "concurrency_index": index,
                "canary": canary,
                "session_id": result["stream_id"],
                "workflow_id": result["stream_id"],
                "cache_namespace": f"{run_id}:concurrent:{index}",
                "decision_mapping": [
                    {
                        "turn": turn["turn"], "model": turn["model"],
                        "session_id": result["stream_id"],
                        "workflow_id": result["stream_id"],
                        "cache_namespace": f"{run_id}:concurrent:{index}",
                    }
                    for turn in result["turns"]
                ],
            })
            if checkpoint_result:
                checkpoint_result(result)
            return result
        finally:
            with lock:
                active -= 1

    selected = [scenarios[index % len(scenarios)] for index in range(count)]
    with ThreadPoolExecutor(max_workers=count) as executor:
        futures = [
            executor.submit(worker, index, scenario)
            for index, scenario in enumerate(selected)
            if index not in completed
        ]
        results = list(completed.values()) + [future.result() for future in as_completed(futures)]
    canaries = {result["concurrency_index"]: result["canary"] for result in results}
    crossovers = []
    for result in results:
        text = "\n".join(turn["response"] for turn in result["turns"])
        result["own_canary_echoed"] = result["canary"] in result["turns"][0]["response"]
        foreign = [canary for index, canary in canaries.items() if index != result["concurrency_index"] and canary in text]
        result["foreign_canaries"] = foreign
        if foreign:
            crossovers.append({"stream_id": result["stream_id"], "foreign_canaries": foreign})
    return {
        "declared_concurrency": count,
        "observed_max_concurrency": maximum,
        "completed_streams": len(results),
        "stream_ids": sorted(result["stream_id"] for result in results),
        "turns": sum(len(result["turns"]) for result in results),
        "cost_usd": sum(
            turn["cost_usd"] + turn.get("retry_cost_usd", 0)
            for result in results for turn in result["turns"]
        ),
        "canaries_echoed": sum(result["own_canary_echoed"] for result in results),
        "crossovers": crossovers,
        "isolation_verified": len(results) == count and not crossovers and all(result["own_canary_echoed"] for result in results),
        "results": results,
    }


def usage_cost(usage, model):
    model = model_name(model)
    if model not in PRICES:
        raise ValueError(f"no price for model {model!r}")
    input_price, output_price = PRICES[model]
    uncached = int(usage.get("input_tokens") or 0)
    created = int(usage.get("cache_creation_input_tokens") or 0)
    read = int(usage.get("cache_read_input_tokens") or 0)
    output = int(usage.get("output_tokens") or 0)
    return (
        uncached * input_price
        + created * input_price * CACHE_WRITE_MULTIPLIER
        + read * input_price * CACHE_READ_MULTIPLIER
        + output * output_price
    ) / 1_000_000


def response_text(message):
    return "\n".join(
        block.get("text", "")
        for block in message.get("content", [])
        if block.get("type") == "text"
    ).strip()


def post_json(url, payload, key, session_id=None, retries=8):
    body = json.dumps(payload, separators=(",", ":")).encode()
    attempts = []
    for attempt in range(retries):
        request = urllib.request.Request(url, data=body, method="POST")
        request.add_header("content-type", "application/json")
        request.add_header("anthropic-version", "2023-06-01")
        if key:
            request.add_header("x-api-key", key)
            request.add_header("Authorization", f"Bearer {key}")
        if session_id:
            request.add_header("X-Octopus-Session-ID", session_id)
        started = time.monotonic()
        try:
            with urllib.request.urlopen(request, timeout=240) as response:
                decoded = json.loads(response.read().decode())
            latency = int((time.monotonic() - started) * 1000)
            attempts.append({"attempt": attempt + 1, "outcome": "success", "status": response.status, "latency_ms": latency})
            decoded["_eval_attempts"] = attempts
            return decoded, latency
        except urllib.error.HTTPError as exc:
            raw_detail = exc.read().decode(errors="replace")[:1000]
            exc.close()
            try:
                error_value = json.loads(raw_detail)
                failed_usage = error_value.get("usage") or error_value.get("error", {}).get("usage")
            except json.JSONDecodeError:
                failed_usage = None
            attempts.append({
                "attempt": attempt + 1, "outcome": "http_error", "status": exc.code,
                "latency_ms": int((time.monotonic() - started) * 1000), "usage": failed_usage,
            })
            if exc.code not in (408, 429, 500, 502, 503, 504) or attempt + 1 == retries:
                raise RuntimeError(f"HTTP {exc.code} from {url}: {raw_detail}; attempts={attempts}") from exc
        except (TimeoutError, urllib.error.URLError) as exc:
            attempts.append({
                "attempt": attempt + 1, "outcome": "network_error", "status": None,
                "latency_ms": int((time.monotonic() - started) * 1000), "usage": None,
            })
            if attempt + 1 == retries:
                raise RuntimeError(f"request failed for {url}: {exc}; attempts={attempts}") from exc
        time.sleep(min(30, 2 ** attempt))
    raise AssertionError("unreachable")


def request_payload(model, messages, cache_namespace, max_tokens=6000, cache=True):
    system_text = SYSTEM_TEXT + f"\nEvaluation cache namespace: {cache_namespace}.\n"
    system = {"type": "text", "text": system_text}
    if cache:
        system["cache_control"] = {"type": "ephemeral", "ttl": "5m"}
    return {
        "model": model,
        "max_tokens": max_tokens,
        "system": [system],
        "messages": messages,
    }


def run_arm(
    arm, scenario, octopus_base, direct_base, key, pause, cache_namespace,
    budget=None, existing=None, checkpoint_turn=None,
):
    result = existing or {"arm": arm, "scenario_id": scenario["id"], "turns": [], "complete": False}
    turns = result["turns"]
    messages = []
    for index, turn in enumerate(turns):
        difficulty, prompt = scenario["turns"][index]
        if turn.get("prompt") != prompt or turn.get("difficulty") != difficulty:
            raise RuntimeError(f"partial {arm} arm no longer matches scenario {scenario['id']}")
        messages.extend([
            {"role": "user", "content": prompt},
            {"role": "assistant", "content": turn["response"]},
        ])
    is_octopus = arm == "octopus"
    endpoint = (octopus_base if is_octopus else direct_base).rstrip("/") + "/v1/messages"
    requested_model = ARM_MODELS[arm]
    stream_id = result.setdefault("stream_id", f"run2:{scenario['id']}:{arm}:{cache_namespace}")
    session_id = stream_id if is_octopus else None
    for turn_index, (difficulty, prompt) in enumerate(
        scenario["turns"][len(turns):], start=len(turns) + 1
    ):
        if budget:
            budget.before_request()
        messages.append({"role": "user", "content": prompt})
        message, latency_ms = post_json(
            endpoint,
            request_payload(requested_model, messages, cache_namespace),
            None if is_octopus else key,
            session_id=session_id,
        )
        if message.get("type") == "error" or not message.get("content"):
            raise RuntimeError(f"invalid response for {scenario['id']} turn {turn_index}: {message}")
        text = response_text(message)
        attempts = message.pop("_eval_attempts", [])
        returned_model = model_name(message.get("model"))
        if not returned_model:
            raise RuntimeError(f"response omitted model metadata for {scenario['id']} turn {turn_index}")
        if not is_octopus and returned_model != requested_model:
            raise RuntimeError(
                f"fixed-{arm} baseline returned {returned_model!r}, expected {requested_model!r}; "
                "refusing to mislabel or misprice the baseline"
            )
        actual_model = returned_model
        usage = message.get("usage") or {}
        turn_cost = usage_cost(usage, actual_model)
        failed_attempts = [attempt for attempt in attempts if attempt["outcome"] != "success"]
        retry_cost = 0.0
        retry_usage_complete = True
        for failed in failed_attempts:
            if failed.get("usage") and not is_octopus:
                retry_cost += usage_cost(failed["usage"], requested_model)
            else:
                retry_usage_complete = False
        turns.append({
            "turn": turn_index,
            "difficulty": difficulty,
            "prompt": prompt,
            "response": text,
            "model": actual_model,
            "latency_ms": latency_ms,
            "stop_reason": message.get("stop_reason"),
            "usage": usage,
            "cost_usd": turn_cost,
            "attempts": attempts,
            "retry_cost_usd": retry_cost,
            "retry_usage_complete": retry_usage_complete,
        })
        messages.append({"role": "assistant", "content": text})
        if checkpoint_turn:
            checkpoint_turn(result)
        if budget:
            budget.charge(turn_cost + retry_cost)
        time.sleep(pause)
    result["complete"] = True
    result["objective_assertions"] = grade_assertions(scenario, result)
    if checkpoint_turn:
        checkpoint_turn(result)
    return result


def transcript_for_judge(scenario, result):
    items = []
    for (difficulty, prompt), turn in zip(scenario["turns"], result["turns"]):
        items.append({
            "difficulty_label": difficulty,
            "user": prompt,
            "assistant": turn["response"],
        })
    return items


def grade_assertions(scenario, result):
    grades = []
    for assertion in scenario.get("assertions", []):
        selected = result["turns"] if assertion.get("turn", "all") == "all" else [
            result["turns"][assertion["turn"] - 1]
        ]
        texts = [turn.get("response", "") for turn in selected]
        combined = "\n".join(texts)
        kind = assertion["type"]
        value = assertion.get("value")
        if kind == "non_empty":
            passed = all(text.strip() for text in texts)
        elif kind == "contains":
            passed = str(value).casefold() in combined.casefold()
        elif kind == "not_contains":
            passed = str(value).casefold() not in combined.casefold()
        elif kind == "regex":
            passed = re.search(str(value), combined, re.IGNORECASE | re.MULTILINE) is not None
        elif kind == "max_words":
            passed = len(combined.split()) <= int(value)
        elif kind == "exact":
            passed = combined.strip().casefold() == str(value).strip().casefold()
        else:
            raise AssertionError(kind)
        grades.append({
            "id": assertion["id"], "type": kind, "turn": assertion.get("turn", "all"),
            "passed": passed,
        })
    return grades


def parse_judgement(text):
    match = re.search(r"\{.*\}", text, re.DOTALL)
    if not match:
        raise ValueError(f"judge did not return JSON: {text[:400]}")
    value = json.loads(match.group(0))
    winner = str(value.get("winner", "")).upper()
    if winner not in {"A", "B", "TIE"}:
        raise ValueError(f"invalid winner: {winner!r}")
    value["winner"] = winner
    value["a_score"] = float(value["a_score"])
    value["b_score"] = float(value["b_score"])
    for field in ("a_score", "b_score"):
        if not math.isfinite(value[field]) or not 0 <= value[field] <= 10:
            raise ValueError(f"judge {field} must be finite and between 0 and 10")
    value["reason"] = str(value.get("reason", ""))[:600]
    return value


def judge_scenario(scenario, octopus, opus, direct_base, key, pause, seed, judge_model=OPUS, budget=None):
    # Reproducible per run, but varied across repeat-run seeds to expose A/B
    # position bias rather than assigning every repeat identically.
    label_key = f"{seed}:{judge_model}:{scenario['id']}".encode()
    octopus_is_a = int(hashlib.sha256(label_key).hexdigest(), 16) % 2 == 0
    a = octopus if octopus_is_a else opus
    b = opus if octopus_is_a else octopus
    judge_input = {
        "scenario": scenario["name"],
        "reference_facts": CORE_REFERENCE,
        "candidate_A": transcript_for_judge(scenario, a),
        "candidate_B": transcript_for_judge(scenario, b),
    }
    prompt = (
        "You are a strict blind evaluator. Compare two complete multi-turn assistant transcripts. "
        "Do not infer model identity or reward verbosity. Score correctness, use of supplied facts, "
        "instruction following, calibrated uncertainty, continuity, and practical usefulness. Penalise "
        "invented facts and missed constraints. Return JSON only with this exact shape: "
        '{"winner":"A|B|TIE","a_score":0.0,"b_score":0.0,"reason":"brief evidence-based reason"}. '
        "Scores are 0 to 10. A tie means neither is meaningfully better. Limit reason to 60 words.\n\n"
        + json.dumps(judge_input, ensure_ascii=False)
    )
    payload = {
        "model": judge_model,
        "max_tokens": 700,
        "system": "Judge the supplied outputs impartially. Output valid JSON and nothing else.",
        "messages": [{"role": "user", "content": prompt}],
    }
    if budget:
        budget.before_request()
    message, latency_ms = post_json(direct_base.rstrip("/") + "/v1/messages", payload, key)
    attempts = message.pop("_eval_attempts", [])
    returned_model = model_name(message.get("model"))
    if returned_model != judge_model:
        raise RuntimeError(
            f"judge returned {returned_model!r}, expected {judge_model!r}; refusing to misprice judging"
        )
    judge_cost = usage_cost(message.get("usage") or {}, judge_model)
    failed_attempts = [attempt for attempt in attempts if attempt["outcome"] != "success"]
    retry_cost = sum(
        usage_cost(attempt["usage"], judge_model)
        for attempt in failed_attempts if attempt.get("usage")
    )
    judgement = parse_judgement(response_text(message))
    if octopus_is_a:
        octopus_score, opus_score = judgement["a_score"], judgement["b_score"]
        winner = "octopus" if judgement["winner"] == "A" else "opus" if judgement["winner"] == "B" else "tie"
    else:
        octopus_score, opus_score = judgement["b_score"], judgement["a_score"]
        winner = "octopus" if judgement["winner"] == "B" else "opus" if judgement["winner"] == "A" else "tie"
    time.sleep(pause)
    return {
        "scenario_id": scenario["id"],
        "judge_model": judge_model,
        "octopus_was": "A" if octopus_is_a else "B",
        "winner": winner,
        "octopus_score": octopus_score,
        "opus_score": opus_score,
        "reason": judgement["reason"],
        "usage": message.get("usage") or {},
        "cost_usd": judge_cost,
        "latency_ms": latency_ms,
        "attempts": attempts,
        "retry_cost_usd": retry_cost,
        "retry_usage_complete": all(attempt.get("usage") for attempt in failed_attempts),
    }


def checkpoint_judgement_then_charge(judgements, judgement, checkpoint, budget):
    """Durably record a paid judge response before enforcing the spend limit."""
    judgements.append(judgement)
    checkpoint()
    if budget:
        budget.charge(judgement["cost_usd"] + judgement.get("retry_cost_usd", 0))


def load_insights(path, expected_requests=None):
    last_requests = 0
    for _ in range(120):
        try:
            ledger = json.loads(Path(path).read_text())
            last_requests = sum(
                int(day.get("requests") or 0)
                for day in ledger.get("days", {}).values()
            )
            if expected_requests is None or last_requests >= expected_requests:
                return ledger
        except (FileNotFoundError, json.JSONDecodeError):
            pass
        time.sleep(0.25)
    raise RuntimeError(
        f"Insights ledger at {path} contains {last_requests} of "
        f"{expected_requests if expected_requests is not None else 'the expected'} requests"
    )


def attach_routing_evidence(ledger, turns):
    """Attach prompt-free decision evidence to routed turns in completion order."""
    decisions = ledger.get("recent_decisions", [])
    if len(decisions) < len(turns):
        raise RuntimeError(
            f"Insights has {len(decisions)} decisions for {len(turns)} routed turns"
        )
    decisions = decisions[-len(turns):]
    for turn, decision in zip(turns, decisions):
        actual = model_name(decision.get("actual_model"))
        if actual and actual != turn["model"]:
            raise RuntimeError(
                f"Insights/turn model mismatch: {actual} != {turn['model']}"
            )
        turn["routed_difficulty"] = decision.get("difficulty")
        turn["routed_risk"] = decision.get("risk")
        turn["classification_source"] = decision.get("classification_source")
        turn["classification_status"] = decision.get("classification_status")
        turn["applied_quality_floor"] = decision.get("applied_quality_floor")


def save_checkpoint(path, run_id, suite_metadata, runs, judgements, execution):
    value = {
        "schema_version": 3,
        "run_id": run_id,
        "suite": suite_metadata,
        "execution": execution,
        "runs": runs,
        "judgements": judgements,
    }
    destination = Path(path)
    temporary = destination.with_name(destination.name + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")
    temporary.replace(destination)


def load_checkpoint(path, suite_metadata):
    value = json.loads(Path(path).read_text())
    if value.get("schema_version") != 3 or not value.get("run_id"):
        raise RuntimeError(f"unsupported run2 checkpoint: {path}")
    recorded = value.get("suite") or {}
    if recorded.get("sha256") != suite_metadata["sha256"]:
        raise RuntimeError(
            "checkpoint scenario suite does not match the selected suite; "
            "start a new run without --resume"
        )
    if recorded.get("scenario_ids") != suite_metadata["scenario_ids"]:
        raise RuntimeError("checkpoint scenario order does not match the selected suite")
    execution = value.get("execution") or {}
    required = (
        "arm_order_seed", "octopus_cache_namespace", "direct_cache_namespace",
        "cache_namespace_by_arm", "arms", "judge_models", "concurrency",
    )
    if any(execution.get(field) in (None, "") for field in required):
        raise RuntimeError(f"checkpoint lacks reproducibility metadata: {path}")
    return value


def ledger_totals(ledger):
    fields = ("actual_cost_usd", "classifier_overhead_usd", "requests", "amortized_switches")
    totals = {field: 0 for field in fields}
    for day in ledger.get("days", {}).values():
        for field in fields:
            totals[field] += day.get(field, 0) or 0
    return totals


def money(value):
    return f"${value:,.6f}"


def build_summary(data, scenarios):
    cost = data["costs"]
    quality = data["quality"]
    allocation = data["model_allocation"]
    lines = [
        "# Octopus switching evaluation: Haiku, Sonnet and Opus",
        "",
        f"- Generated: {data['generated_at']}",
        f"- Suite: `{data['configuration']['suite']['name']}` ({data['configuration']['suite']['sha256'][:12]})",
        f"- Workload: {len(scenarios)} scenarios, {sum(len(s['turns']) for s in scenarios)} turns per arm",
        "- Client output allowance: 6,000 tokens per workload turn for both arms",
        "- Comparison: measured Octopus routing versus independently measured all-Opus execution",
        "- Prices: Haiku $1/$5, Sonnet $3/$15, Opus $15/$75 per million input/output tokens",
        "- Prompt caching: provider-reported 5-minute cache writes and reads priced at 1.25x and 0.10x input",
        "",
        "## Result",
        "",
        f"**{quality['verdict']}**",
        "",
        "| Measure | Octopus | All Opus | Difference |",
        "|---|---:|---:|---:|",
        f"| Paid workload cost | {money(cost['octopus_total_usd'])} | {money(cost['opus_total_usd'])} | {money(cost['octopus_total_usd'] - cost['opus_total_usd'])} |",
        f"| Mean blinded quality | {quality['octopus_mean']:.2f}/10 | {quality['opus_mean']:.2f}/10 | {quality['delta']:+.2f} |",
        f"| Conversation wins | {quality['octopus_wins']} | {quality['opus_wins']} | {quality['ties']} ties |",
        f"| Max-token stops | {quality['octopus_truncations']} | {quality['opus_truncations']} | — |",
        "",
        f"Octopus saved **{money(cost['savings_usd'])} ({cost['savings_percent']:.1f}%)** against the measured all-Opus workload. "
        f"This includes {money(cost['classifier_overhead_usd'])} of classifier overhead.",
        "",
        "## What this run actually shows",
        "",
        (
            "- The cost-reduction objective passed."
            if cost["savings_usd"] > 0
            else "- The cost-reduction objective failed."
        ),
        (
            "- The superior-results objective passed under the predefined rule."
            if quality["verdict"] == "Octopus was cheaper and superior on this suite."
            else "- The superior-results objective did not pass under the predefined rule."
        ),
        (
            "- Opus was never selected, so this run measured Haiku/Sonnet switching against an all-Opus baseline; "
            "it did not demonstrate effective three-way routing."
            if allocation.get(OPUS, 0) == 0
            else f"- Opus served {allocation.get(OPUS, 0)} routed turns, so all three catalogue tiers participated."
        ),
        "- Adjacent model-path changes include both voluntary amortised switches and mandatory changes when the incumbent becomes ineligible.",
        "",
        "## Cost accounting",
        "",
        "| Component | Cost |",
        "|---|---:|",
        f"| Octopus model responses | {money(cost['octopus_main_usd'])} |",
        f"| Octopus classifier overhead | {money(cost['classifier_overhead_usd'])} |",
        f"| **Octopus total** | **{money(cost['octopus_total_usd'])}** |",
        f"| Measured all-Opus responses | {money(cost['opus_total_usd'])} |",
        f"| Opus price applied to Octopus's exact token/cache usage | {money(cost['normalized_opus_counterfactual_usd'])} |",
        f"| Blinded judge (excluded from workload comparison) | {money(cost['judge_usd'])} |",
        "",
        "The measured baseline captures real differences in answer length and the conversation history each arm created. "
        "The normalised counterfactual holds Octopus token/cache usage fixed and changes only the model price.",
        "",
        "## Routing and switching",
        "",
        "| Model | Turns | Share |",
        "|---|---:|---:|",
    ]
    total_turns = sum(allocation.values()) or 1
    for model in (HAIKU, SONNET, OPUS):
        count = allocation.get(model, 0)
        lines.append(f"| `{model}` | {count} | {count / total_turns * 100:.1f}% |")
    lines += [
        "",
        f"Observed **{data['switch_count']}** model changes across {len(scenarios)} Octopus conversations. "
        f"The Insights ledger recorded {int(data['insights']['amortized_switches'])} decisions explicitly classified as amortised switches.",
        "",
        "| Scenario | Model path | Switches | Octopus cost | Opus cost |",
        "|---|---|---:|---:|---:|",
    ]
    by_arm = data["runs"]
    for scenario in scenarios:
        octopus = by_arm[scenario["id"]]["octopus"]
        opus = by_arm[scenario["id"]]["opus"]
        path = " -> ".join(turn["model"].replace("claude-", "") for turn in octopus["turns"])
        switches = sum(
            left["model"] != right["model"]
            for left, right in zip(octopus["turns"], octopus["turns"][1:])
        )
        lines.append(
            f"| {scenario['name']} | `{path}` | {switches} | "
            f"{money(sum(t['cost_usd'] for t in octopus['turns']))} | "
            f"{money(sum(t['cost_usd'] for t in opus['turns']))} |"
        )

    lines += ["", "## Blinded quality", ""]
    if data["judgements"]:
        lines += [
            "| Scenario | Winner | Octopus | Opus | Judge's reason |",
            "|---|---|---:|---:|---|",
        ]
        names = {scenario["id"]: scenario["name"] for scenario in scenarios}
        for judgement in data["judgements"]:
            reason = judgement["reason"].replace("|", "\\|").replace("\n", " ")
            lines.append(
                f"| {names[judgement['scenario_id']]} | {judgement['winner']} | "
                f"{judgement['octopus_score']:.1f} | {judgement['opus_score']:.1f} | {reason} |"
            )
    else:
        lines.append("Judging was skipped with `--no-judge`; no quality claim can be made.")

    lines += [
        "",
        "## Turn-by-turn evidence",
        "",
        "| Scenario | Turn | Intended difficulty | Routed model | Octopus cost | Opus cost |",
        "|---|---:|---|---|---:|---:|",
    ]
    for scenario in scenarios:
        octopus = by_arm[scenario["id"]]["octopus"]["turns"]
        opus = by_arm[scenario["id"]]["opus"]["turns"]
        for routed, baseline in zip(octopus, opus):
            lines.append(
                f"| {scenario['name']} | {routed['turn']} | {routed['difficulty']} | "
                f"`{routed['model']}` | {money(routed['cost_usd'])} | {money(baseline['cost_usd'])} |"
            )

    fixed_costs = data["costs"].get("fixed_arm_costs_usd", {})
    concurrent = data.get("concurrent_workload", {})
    attempts = data.get("attempt_accounting", {})
    provenance = data["configuration"].get("provenance", {})
    lines += [
        "", "## Production evidence", "",
        "| Arm | Measured response and known retry cost | Objective assertions | Passed |",
        "|---|---:|---:|---:|",
    ]
    for arm in data["configuration"].get("arms", ["octopus", "opus"]):
        objective = data.get("objective_grading", {}).get("by_arm", {}).get(arm, {})
        lines.append(
            f"| `{arm}` | {money(fixed_costs.get(arm, 0))} | "
            f"{objective.get('assertions', 0)} | {objective.get('passed', 0)} |"
        )
    lines += [
        "",
        f"- Judges: {', '.join(data['configuration'].get('judge_models', [])) or 'none'}",
        f"- Concurrent streams: {concurrent.get('completed_streams', 0)} completed; "
        f"observed maximum {concurrent.get('observed_max_concurrency', 1)}; "
        f"canary crossovers {len(concurrent.get('crossovers', []))}",
        f"- Retry failures: {attempts.get('failed_attempts', 0)}; known retry spend "
        f"{money(attempts.get('known_retry_cost_usd', 0))}; invoice reconciliation required: "
        f"{str(attempts.get('invoice_reconciliation_required', False)).lower()}",
        f"- Provenance: commit `{provenance.get('source_commit', 'unknown')}`, binary "
        f"`{provenance.get('binary_sha256', 'unknown')}`, config "
        f"`{provenance.get('config_sha256', 'unknown')}`, provider "
        f"`{provenance.get('provider_endpoint_identity', 'unknown')}`",
    ]

    lines += [
        "",
        "## Interpretation limits",
        "",
        "- This is a small paired sample, not a universal model ranking. Repeat runs are needed for confidence intervals.",
        "- Any max-token stop makes that conversation's quality comparison less conclusive; counts are reported above.",
        "- The intended difficulty labels describe suite design; Octopus uses its live classifier and may disagree.",
        "- The blind judge is Opus. Random A/B labels reduce position bias, but same-family style bias can remain.",
        "- Provider aliases, prices, cache rules, model behaviour, and gateway configuration can change.",
        "- A cheaper run is not superior unless the quality evidence supports that claim. The verdict uses predefined thresholds: "
        "superior requires a mean advantage of at least 0.25 and more wins; non-inferior means within 0.50 while cheaper.",
        "- Judge spend is excluded from the product workload comparison because it is evaluation overhead, not an Octopus operating cost.",
        "",
        "Raw prompts, responses, usage, costs, label assignments, and judgements are in `run2-results.json`.",
        "",
    ]
    return "\n".join(lines)


def main():
    args = parse_args()
    try:
        scenarios, suite_metadata = load_scenario_suite(args.suite, args.scenario_file)
        arms = parse_csv_choices(args.arms, ARM_MODELS, "arms")
        judge_models = parse_csv_choices(args.judge_models, PRICES, "judge models")
    except ValueError as exc:
        raise SystemExit(str(exc)) from exc
    if not {"octopus", "opus"}.issubset(arms):
        raise SystemExit("run2 requires octopus and opus arms; haiku and sonnet are optional fixed baselines")
    if args.concurrency < 1:
        raise SystemExit("--concurrency must be at least 1")
    provenance = {
        "source_commit": args.source_commit,
        "binary_sha256": args.binary_sha256,
        "config_sha256": args.config_sha256,
        "effective_policy_digest": args.policy_digest,
        "provider_endpoint_identity": args.provider_identity,
    }
    key = os.environ.get(args.api_key_env)
    if not key:
        raise SystemExit(f"{args.api_key_env} is not set")

    checkpoint_path = args.results + ".partial"
    if args.resume:
        checkpoint = load_checkpoint(checkpoint_path, suite_metadata)
        run_id = checkpoint["run_id"]
        execution = checkpoint["execution"]
        if execution.get("arms") != arms or execution.get("judge_models") != judge_models:
            raise RuntimeError("arms or judge models do not match the checkpoint")
        if execution.get("concurrency", 1) != args.concurrency:
            raise RuntimeError("concurrency does not match the checkpoint")
        if execution.get("provenance") != provenance:
            raise RuntimeError("build/config/provider provenance does not match the checkpoint")
        if args.seed is not None and args.seed != execution["arm_order_seed"]:
            raise RuntimeError("--seed does not match the checkpointed arm-order seed")
        arm_order_seed = execution["arm_order_seed"]
        octopus_cache_namespace = execution["octopus_cache_namespace"]
        direct_cache_namespace = execution["direct_cache_namespace"]
        cache_namespace_by_arm = execution["cache_namespace_by_arm"]
        runs = checkpoint.get("runs", {})
        judgements = checkpoint.get("judgements", [])
        execution["budget_stop_after_checkpoint"] = False
        if args.restart_octopus:
            for scenario_runs in runs.values():
                scenario_runs.pop("octopus", None)
            judgements = []
            octopus_cache_namespace = hashlib.sha256(
                f"{run_id}:octopus:{time.time_ns()}:{os.getpid()}".encode()
            ).hexdigest()[:16]
            execution["octopus_cache_namespace"] = octopus_cache_namespace
            cache_namespace_by_arm["octopus"] = octopus_cache_namespace
            save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
            print("discarded partial Octopus arms after resetting Insights", flush=True)
        print(f"resuming run {run_id}", flush=True)
    else:
        run_id = args.run_id or hashlib.sha256(
            f"{time.time_ns()}:{os.getpid()}".encode()
        ).hexdigest()[:16]
        runs = {}
        judgements = []
        arm_order_seed = args.seed
        if arm_order_seed is None:
            arm_order_seed = int(hashlib.sha256(run_id.encode()).hexdigest()[:16], 16)
        octopus_cache_namespace, direct_cache_namespace = cache_namespaces(run_id)
        execution = {
            "arm_order_seed": arm_order_seed,
            "octopus_cache_namespace": octopus_cache_namespace,
            "direct_cache_namespace": direct_cache_namespace,
            "cache_namespace_by_arm": {arm: arm_cache_namespace(run_id, arm) for arm in arms},
            "arms": arms,
            "judge_models": judge_models,
            "concurrency": args.concurrency,
            "provenance": provenance,
        }
        cache_namespace_by_arm = execution["cache_namespace_by_arm"]
        save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
    already_spent = sum(
        float(turn.get("cost_usd") or 0) + float(turn.get("retry_cost_usd") or 0)
        for scenario_runs in runs.values()
        for arm in scenario_runs.values()
        for turn in arm.get("turns", [])
    ) + sum(float(item.get("cost_usd") or 0) + float(item.get("retry_cost_usd") or 0) for item in judgements) + sum(
        float(turn.get("cost_usd") or 0) + float(turn.get("retry_cost_usd") or 0)
        for result in execution.get("concurrent_results", [])
        for turn in result.get("turns", [])
    )
    try:
        budget = SpendBudget(args.budget_usd, already_spent)
    except ValueError as exc:
        raise SystemExit(str(exc)) from exc
    if args.restart_octopus:
        print(f"new Octopus cache namespace {octopus_cache_namespace}", flush=True)
    rng = random.Random(arm_order_seed)
    for scenario in scenarios:
        print(f"\n=== {scenario['name']} ===", flush=True)
        order = list(arms)
        rng.shuffle(order)
        scenario_runs = runs.setdefault(scenario["id"], {})
        for arm in order:
            existing = scenario_runs.get(arm)
            if existing and existing.get("complete"):
                print(f"  reusing checkpointed {arm} arm", flush=True)
                continue
            print(f"  running {arm} arm", flush=True)
            if existing is None:
                existing = {"arm": arm, "scenario_id": scenario["id"], "turns": [], "complete": False}
                scenario_runs[arm] = existing
            def checkpoint_turn(_result):
                save_checkpoint(
                    checkpoint_path, run_id, suite_metadata, runs, judgements, execution
                )
            try:
                scenario_runs[arm] = run_arm(
                    arm,
                    scenario,
                    args.octopus_base,
                    args.direct_base,
                    key,
                    args.pause,
                    cache_namespace_by_arm[arm],
                    budget,
                    existing,
                    checkpoint_turn,
                )
            except BudgetExceeded:
                execution["budget_stop_after_checkpoint"] = True
                save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
                raise
            save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
            models = ", ".join(turn["model"] for turn in scenario_runs[arm]["turns"])
            arm_cost = sum(turn["cost_usd"] for turn in scenario_runs[arm]["turns"])
            print(f"    models: {models}", flush=True)
            print(f"    cost:   {money(arm_cost)}", flush=True)
    if not args.no_judge:
        print("\n=== blinded quality judging ===", flush=True)
        judged_ids = {(item["scenario_id"], item.get("judge_model", OPUS)) for item in judgements}
        for scenario in scenarios:
            for judge_model in judge_models:
                if (scenario["id"], judge_model) in judged_ids:
                    print(f"  {scenario['id']} [{judge_model}]: reusing checkpointed judgement", flush=True)
                    continue
                judgement = judge_scenario(
                    scenario,
                    runs[scenario["id"]]["octopus"],
                    runs[scenario["id"]]["opus"],
                    args.direct_base,
                    key,
                    args.pause,
                    arm_order_seed,
                    judge_model,
                    budget,
                )
                try:
                    checkpoint_judgement_then_charge(
                        judgements,
                        judgement,
                        lambda: save_checkpoint(
                            checkpoint_path, run_id, suite_metadata, runs, judgements, execution
                        ),
                        budget,
                    )
                except BudgetExceeded:
                    execution["budget_stop_after_checkpoint"] = True
                    save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
                    raise
                print(
                    f"  {scenario['id']} [{judge_model}]: {judgement['winner']} "
                    f"(Octopus {judgement['octopus_score']:.1f}, Opus {judgement['opus_score']:.1f})",
                    flush=True,
                )

    octopus_turns = [
        turn
        for scenario in scenarios
        for turn in runs[scenario["id"]]["octopus"]["turns"]
    ]
    opus_turns = [
        turn
        for scenario in scenarios
        for turn in runs[scenario["id"]]["opus"]["turns"]
    ]
    ledger = execution.get("main_insights_ledger")
    if ledger is None:
        ledger = load_insights(args.insights, expected_requests=len(octopus_turns))
        execution["main_insights_ledger"] = ledger
    insights = ledger_totals(ledger)
    attach_routing_evidence(ledger, octopus_turns)
    save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
    octopus_main = sum(turn["cost_usd"] for turn in octopus_turns)
    octopus_retry_cost = sum(turn.get("retry_cost_usd", 0) for turn in octopus_turns)
    classifier_overhead = float(insights["classifier_overhead_usd"])
    octopus_total = float(insights["actual_cost_usd"])
    # The fresh ledger should be authoritative. Retain a safe fallback if an
    # upstream omitted enough usage that Tracker could not price a request.
    if int(insights["requests"]) != len(octopus_turns):
        octopus_total = octopus_main + classifier_overhead
    octopus_total += octopus_retry_cost
    opus_total = sum(turn["cost_usd"] + turn.get("retry_cost_usd", 0) for turn in opus_turns)
    normalized_opus = sum(usage_cost(turn["usage"], OPUS) for turn in octopus_turns)
    judge_cost = sum(judgement["cost_usd"] + judgement.get("retry_cost_usd", 0) for judgement in judgements)
    savings = opus_total - octopus_total
    savings_pct = savings / opus_total * 100 if opus_total else 0.0

    allocation = {model: 0 for model in PRICES}
    switch_count = 0
    for scenario in scenarios:
        path = [turn["model"] for turn in runs[scenario["id"]]["octopus"]["turns"]]
        for model in path:
            allocation[model] = allocation.get(model, 0) + 1
        switch_count += sum(left != right for left, right in zip(path, path[1:]))

    if judgements:
        octopus_mean = sum(item["octopus_score"] for item in judgements) / len(judgements)
        opus_mean = sum(item["opus_score"] for item in judgements) / len(judgements)
        octopus_wins = sum(item["winner"] == "octopus" for item in judgements)
        opus_wins = sum(item["winner"] == "opus" for item in judgements)
        ties = sum(item["winner"] == "tie" for item in judgements)
        delta = octopus_mean - opus_mean
        octopus_truncations = sum(turn.get("stop_reason") == "max_tokens" for turn in octopus_turns)
        opus_truncations = sum(turn.get("stop_reason") == "max_tokens" for turn in opus_turns)
        if octopus_truncations or opus_truncations:
            verdict = "Octopus was cheaper, but max-token stops make the quality verdict inconclusive."
        elif savings > 0 and delta >= 0.25 and octopus_wins > opus_wins:
            verdict = "Octopus was cheaper and superior on this suite."
        elif savings > 0 and delta >= -0.50:
            verdict = "Octopus was cheaper and quality was non-inferior on this suite."
        elif savings > 0:
            verdict = "Octopus was cheaper, but this run shows a material quality regression."
        else:
            verdict = "Octopus did not reduce measured workload cost on this run."
    else:
        octopus_mean = opus_mean = delta = 0.0
        octopus_wins = opus_wins = ties = 0
        octopus_truncations = sum(turn.get("stop_reason") == "max_tokens" for turn in octopus_turns)
        opus_truncations = sum(turn.get("stop_reason") == "max_tokens" for turn in opus_turns)
        verdict = "Cost was measured, but quality judging was skipped."

    concurrent_evidence = execution.get("concurrent_evidence")
    if concurrent_evidence is None:
        concurrent_lock = threading.Lock()
        concurrent_results = execution.setdefault("concurrent_results", [])
        def checkpoint_concurrent(result):
            with concurrent_lock:
                concurrent_results[:] = [
                    item for item in concurrent_results
                    if item.get("concurrency_index") != result["concurrency_index"]
                ]
                concurrent_results.append(result)
                save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
        concurrent_evidence = run_concurrent_streams(
            args.concurrency, scenarios, args.octopus_base, args.direct_base,
            key, args.pause, run_id, budget, concurrent_results, checkpoint_concurrent,
        )
        execution["concurrent_evidence"] = concurrent_evidence
        save_checkpoint(checkpoint_path, run_id, suite_metadata, runs, judgements, execution)
    arm_costs = {
        arm: sum(
            turn["cost_usd"] + turn.get("retry_cost_usd", 0)
            for scenario in scenarios
            for turn in runs[scenario["id"]][arm]["turns"]
        )
        for arm in arms
    }
    objective_grades = [
        grade
        for scenario in scenarios
        for arm in arms
        for grade in runs[scenario["id"]][arm].get("objective_assertions", [])
    ]
    objective_by_arm = {
        arm: [
            grade
            for scenario in scenarios
            for grade in runs[scenario["id"]][arm].get("objective_assertions", [])
        ]
        for arm in arms
    }
    attempt_records = [
        item
        for scenario in scenarios
        for arm in arms
        for turn in runs[scenario["id"]][arm]["turns"]
        for item in [turn]
    ] + judgements + [
        turn for result in concurrent_evidence.get("results", []) for turn in result.get("turns", [])
    ]
    failed_attempts = sum(
        attempt.get("outcome") != "success"
        for item in attempt_records for attempt in item.get("attempts", [])
    )
    known_retry_cost = sum(float(item.get("retry_cost_usd") or 0) for item in attempt_records)
    invoice_reconciliation_required = any(
        not item.get("retry_usage_complete", True) for item in attempt_records
    )

    data = {
        "schema_version": 1,
        "generated_at": dt.datetime.now().astimezone().isoformat(timespec="seconds"),
        "configuration": {
            "run_id": run_id,
            "arm_order_seed": arm_order_seed,
            "budget_usd": args.budget_usd,
            "suite": suite_metadata,
            "direct_cache_namespace": direct_cache_namespace,
            "octopus_cache_namespace": octopus_cache_namespace,
            "cache_namespace_by_arm": cache_namespace_by_arm,
            "arms": arms,
            "judge_models": judge_models,
            "concurrency": args.concurrency,
            "provenance": provenance,
            "strategy": "amortized",
            "classifier": HAIKU,
            "catalog": list(PRICES),
            "system_prefix_characters": len(
                SYSTEM_TEXT + f"\nEvaluation cache namespace: {octopus_cache_namespace}.\n"
            ),
            "workload_max_tokens": 6000,
            "scenario_count": len(scenarios),
            "turns_per_arm": len(octopus_turns),
        },
        "runs": runs,
        "judgements": judgements,
        "insights": insights,
        "model_allocation": allocation,
        "switch_count": switch_count,
        "concurrent_workload": concurrent_evidence,
        "objective_grading": {
            "assertions": len(objective_grades),
            "passed": sum(grade["passed"] for grade in objective_grades),
            "by_arm": {
                arm: {
                    "assertions": len(grades),
                    "passed": sum(grade["passed"] for grade in grades),
                }
                for arm, grades in objective_by_arm.items()
            },
        },
        "attempt_accounting": {
            "failed_attempts": failed_attempts,
            "known_retry_cost_usd": known_retry_cost,
            "invoice_reconciliation_required": invoice_reconciliation_required,
            "note": "Provider failures without usage cannot be priced locally and require invoice reconciliation.",
        },
        "costs": {
            "octopus_main_usd": octopus_main,
            "classifier_overhead_usd": classifier_overhead,
            "octopus_total_usd": octopus_total,
            "opus_total_usd": opus_total,
            "normalized_opus_counterfactual_usd": normalized_opus,
            "savings_usd": savings,
            "savings_percent": savings_pct,
            "judge_usd": judge_cost,
            "fixed_arm_costs_usd": arm_costs,
            "concurrent_evidence_usd": concurrent_evidence["cost_usd"],
        },
        "quality": {
            "verdict": verdict,
            "octopus_mean": octopus_mean,
            "opus_mean": opus_mean,
            "delta": delta,
            "octopus_wins": octopus_wins,
            "opus_wins": opus_wins,
            "ties": ties,
            "octopus_truncations": octopus_truncations,
            "opus_truncations": opus_truncations,
        },
    }
    Path(args.results).write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    Path(args.summary).write_text(build_summary(data, scenarios) + "\n")
    Path(checkpoint_path).unlink(missing_ok=True)
    print("\n=== result ===", flush=True)
    print(f"  {verdict}", flush=True)
    print(f"  Octopus: {money(octopus_total)}", flush=True)
    print(f"  All Opus: {money(opus_total)}", flush=True)
    print(f"  Savings: {money(savings)} ({savings_pct:.1f}%)", flush=True)
    if judgements:
        print(f"  Quality: Octopus {octopus_mean:.2f}, Opus {opus_mean:.2f}", flush=True)
    print(f"  Summary: {args.summary}", flush=True)
    print(f"  Raw data: {args.results}", flush=True)


if __name__ == "__main__":
    main()
