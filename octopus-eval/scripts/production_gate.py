#!/usr/bin/env python3
"""Evaluate repeat run2 result sets against predeclared production gates."""

from __future__ import annotations

import argparse
import json
import random
import sys
from pathlib import Path


def percentile(values: list[float], probability: float) -> float:
    ordered = sorted(values)
    if not ordered:
        raise ValueError("cannot calculate a percentile from no values")
    position = (len(ordered) - 1) * probability
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    fraction = position - lower
    return ordered[lower] * (1 - fraction) + ordered[upper] * fraction


def bootstrap_lower(deltas: list[float], samples: int = 10_000) -> float:
    rng = random.Random(20260807)
    means = []
    for _ in range(samples):
        draw = [rng.choice(deltas) for _ in deltas]
        means.append(sum(draw) / len(draw))
    return percentile(means, 0.025)


def hierarchical_bootstrap_lower(
    observations: dict[str, dict[str, float]], samples: int = 10_000
) -> float:
    """Two-way bootstrap across independent runs and scenario clusters."""
    run_ids = sorted(observations)
    if not run_ids:
        raise ValueError("cannot bootstrap empty run/scenario observations")
    scenario_ids = sorted(observations[run_ids[0]])
    if not scenario_ids:
        raise ValueError("cannot bootstrap empty run/scenario observations")
    expected = set(scenario_ids)
    if any(set(values) != expected for values in observations.values()):
        raise ValueError("every repeat must judge the same scenario set")
    rng = random.Random(20260807)
    means = []
    for _ in range(samples):
        sampled_runs = [rng.choice(run_ids) for _ in run_ids]
        sampled_scenarios = [rng.choice(scenario_ids) for _ in scenario_ids]
        values = [
            observations[run_id][scenario_id]
            for run_id in sampled_runs
            for scenario_id in sampled_scenarios
        ]
        means.append(sum(values) / len(values))
    return percentile(means, 0.025)


def evaluate(paths: list[Path], gates: dict) -> dict:
    scenario_deltas: dict[str, list[float]] = {}
    run_scenario_deltas: dict[str, dict[str, float]] = {}
    judgement_count = 0
    octopus_cost = opus_cost = 0.0
    high_turns = safe_high_turns = 0
    classified_high = 0
    turns_per_run: list[int] = []
    expected_suite = None
    expected_judge_models = None
    expected_provenance = None
    attempt_reports = []
    run_ids_seen = set()
    truncations = 0
    arm_cost_totals: dict[str, float] = {}
    arm_objectives: dict[str, list[bool]] = {}
    quality = gates["model_quality"]
    floor = gates["high_quality_floor"]

    for path in paths:
        result = json.loads(path.read_text())
        attempt_reports.append(result.get("attempt_accounting") or {})
        provenance = result.get("configuration", {}).get("provenance") or {}
        if expected_provenance is None:
            expected_provenance = provenance
        elif provenance != expected_provenance:
            raise ValueError(f"{path}: build/config/provider provenance does not match other runs")
        run_id = result.get("configuration", {}).get("run_id")
        if not isinstance(run_id, str) or not run_id:
            raise ValueError(f"{path}: missing run_id")
        if run_id in run_ids_seen:
            raise ValueError(f"{path}: duplicate run_id {run_id!r}")
        run_ids_seen.add(run_id)
        suite = result.get("configuration", {}).get("suite") or {}
        scenario_ids = suite.get("scenario_ids")
        suite_signature = (suite.get("name"), suite.get("sha256"), tuple(scenario_ids or ()))
        if not suite_signature[0] or not suite_signature[1] or not scenario_ids:
            raise ValueError(f"{path}: missing scenario suite identity")
        if expected_suite is None:
            expected_suite = suite_signature
        elif suite_signature != expected_suite:
            raise ValueError(f"{path}: scenario suite does not match the other result files")
        run_ids = set(result.get("runs", {}))
        if run_ids != set(scenario_ids):
            raise ValueError(f"{path}: run scenario IDs do not match the suite manifest")
        octopus_cost += result["costs"]["octopus_total_usd"]
        opus_cost += result["costs"]["opus_total_usd"]
        declared_arms = result.get("configuration", {}).get("arms", ["octopus", "opus"])
        required_arms = gates.get("required_arms", ["octopus", "opus"])
        if not set(required_arms).issubset(declared_arms):
            raise ValueError(f"{path}: missing required evaluation arms")
        judge_models = result.get("configuration", {}).get("judge_models") or ["legacy-default"]
        if expected_judge_models is None:
            expected_judge_models = tuple(judge_models)
        elif tuple(judge_models) != expected_judge_models:
            raise ValueError(f"{path}: judge-model configuration does not match other runs")
        seen_in_run = set()
        seen_judge_pairs = set()
        per_scenario_judges: dict[str, list[float]] = {}
        run_scenario_deltas[run_id] = {}
        for judgement_index, judgement in enumerate(result.get("judgements", [])):
            scenario_id = judgement.get("scenario_id")
            if not isinstance(scenario_id, str) or not scenario_id:
                raise ValueError(f"{path}: judgement {judgement_index + 1} has no scenario_id")
            judge_model = judgement.get("judge_model", "legacy-default")
            pair = (scenario_id, judge_model)
            if pair in seen_judge_pairs:
                raise ValueError(f"{path}: duplicate judgement for scenario/judge {pair!r}")
            seen_judge_pairs.add(pair)
            seen_in_run.add(scenario_id)
            delta = judgement["octopus_score"] - judgement["opus_score"]
            scenario_deltas.setdefault(scenario_id, []).append(delta)
            per_scenario_judges.setdefault(scenario_id, []).append(delta)
            judgement_count += 1
        if seen_in_run != run_ids:
            missing = sorted(run_ids - seen_in_run)
            extra = sorted(seen_in_run - run_ids)
            raise ValueError(
                f"{path}: judgement scenario IDs do not match runs "
                f"(missing={missing}, extra={extra})"
            )
        expected_pairs = {(scenario_id, judge_model) for scenario_id in run_ids for judge_model in judge_models}
        if seen_judge_pairs != expected_pairs:
            raise ValueError(f"{path}: judgement scenario/judge matrix is incomplete")
        run_scenario_deltas[run_id] = {
            scenario_id: sum(values) / len(values)
            for scenario_id, values in per_scenario_judges.items()
        }
        run_turns = 0
        for scenario in result["runs"].values():
            if not set(required_arms).issubset(scenario):
                raise ValueError(f"{path}: every scenario requires all declared production arms")
            reference_turns = scenario["octopus"]["turns"]
            reference_keys = [
                (turn.get("turn"), turn.get("difficulty"), turn.get("prompt"))
                for turn in reference_turns
            ]
            expected_models = {
                "haiku": "claude-haiku-4-5@20251001-global",
                "sonnet": "claude-sonnet-4-6-asia-southeast1",
                "opus": "claude-opus-4-8-global",
            }
            for arm in required_arms:
                turns = scenario[arm]["turns"]
                if gates.get("validate_fixed_arms") and arm in expected_models:
                    keys = [(turn.get("turn"), turn.get("difficulty"), turn.get("prompt")) for turn in turns]
                    if keys != reference_keys:
                        raise ValueError(f"{path}: {arm} turn keys do not match Octopus")
                    if any(turn.get("model") != expected_models[arm] for turn in turns):
                        raise ValueError(f"{path}: {arm} returned unexpected model metadata")
                    if any(
                        not isinstance(turn.get("usage"), dict)
                        or sum(int(value or 0) for value in turn["usage"].values() if isinstance(value, (int, float))) <= 0
                        for turn in turns
                    ):
                        raise ValueError(f"{path}: {arm} has missing provider usage")
                arm_cost_totals[arm] = arm_cost_totals.get(arm, 0.0) + sum(
                    float(turn.get("cost_usd") or 0) for turn in turns
                )
                arm_objectives.setdefault(arm, []).extend(
                    grade.get("passed") is True
                    for grade in scenario[arm].get("objective_assertions", [])
                )
            for turn in scenario["octopus"]["turns"]:
                run_turns += 1
                if turn.get("difficulty") == "high":
                    high_turns += 1
                    if quality.get(turn["model"], -1) >= floor:
                        safe_high_turns += 1
                    # Missing routing evidence is a failed classification, not
                    # an excuse to remove the turn from the denominator.
                    classified_high += turn.get("routed_difficulty") == "high"
                truncations += turn.get("stop_reason") == "max_tokens"
            truncations += sum(
                turn.get("stop_reason") == "max_tokens"
                for turn in scenario["opus"]["turns"]
            )
        turns_per_run.append(run_turns)

    if not scenario_deltas:
        raise ValueError("result files contain no blinded judgements")
    # Repeated runs of the same workflow are repeated measurements, not new
    # independent scenarios. Average within scenario, then bootstrap scenarios.
    # This prevents repetition from manufacturing a falsely narrow interval.
    cluster_means = [
        sum(values) / len(values)
        for _, values in sorted(scenario_deltas.items())
    ]
    mean_delta = sum(cluster_means) / len(cluster_means)
    ci_lower = hierarchical_bootstrap_lower(run_scenario_deltas)
    savings = (opus_cost - octopus_cost) / opus_cost * 100 if opus_cost else 0.0
    recall = classified_high / high_turns if high_turns else None
    objective_by_run = [
        [grade for scenario in json.loads(result_path.read_text())["runs"].values()
         for grade in scenario["octopus"].get("objective_assertions", [])]
        for result_path in paths
    ]
    objective = [grade for run_grades in objective_by_run for grade in run_grades]
    objective_rate = sum(grade.get("passed") is True for grade in objective) / len(objective) if objective else None
    concurrency_values = [
        json.loads(path.read_text()).get("concurrent_workload", {})
        for path in paths
    ]
    evidence_checks = {
        "minimum_runs": len(run_ids_seen) >= gates.get("minimum_runs", 1),
        "minimum_judged_scenarios": len(cluster_means) >= gates.get("minimum_judged_scenarios", 1),
        "minimum_turns_per_run": bool(turns_per_run) and min(turns_per_run) >= gates.get("minimum_turns_per_run", 1),
    }
    checks = {
        "mean_quality": mean_delta >= gates["mean_quality_delta_min"],
        "quality_ci_lower": ci_lower >= gates["quality_ci_lower_min"],
        "savings": savings >= gates["savings_percent_min"],
        "high_tier_safety": high_turns > 0 and safe_high_turns == high_turns,
        "high_difficulty_recall": recall is not None and recall >= gates["high_difficulty_recall_min"],
        "truncations": truncations <= gates.get("max_truncations", 0),
        "objective_assertions": (
            True if gates.get("minimum_objective_assertions", 0) == 0 and not objective
            else all(len(run_grades) >= gates.get("minimum_objective_assertions", 0) for run_grades in objective_by_run)
            and objective_rate is not None
            and objective_rate >= gates.get("objective_pass_rate_min", 0)
        ),
        "multiple_judges": len(expected_judge_models or ()) >= gates.get("minimum_judge_models", 1),
        "concurrent_streams": all(
            item.get("observed_max_concurrency", 1) >= gates.get("minimum_concurrency", 1)
            and item.get("completed_streams", 0) >= gates.get("minimum_concurrent_streams", 0)
            and len(set(item.get("stream_ids", []))) == item.get("completed_streams", 0)
            and item.get("isolation_verified", gates.get("minimum_concurrent_streams", 0) == 0)
            and not item.get("crossovers", [])
            and item.get("canaries_echoed", 0) == item.get("completed_streams", 0)
            and all(
                result.get("session_id") == result.get("workflow_id")
                and result.get("cache_namespace")
                and len(result.get("decision_mapping", [])) == len(result.get("turns", []))
                for result in item.get("results", [])
            )
            for item in concurrency_values
        ),
        "provenance": (
            not gates.get("require_provenance", False)
            or all(expected_provenance.get(field) not in (None, "", "unknown") for field in (
                "source_commit", "binary_sha256", "config_sha256",
                "effective_policy_digest", "provider_endpoint_identity",
            ))
        ) and (
            not gates.get("expected_release_commit")
            or expected_provenance.get("source_commit") == gates["expected_release_commit"]
        ),
        "attempt_accounting": (
            not gates.get("require_attempt_accounting", False)
            or all(
                "failed_attempts" in report
                and "known_retry_cost_usd" in report
                and "invoice_reconciliation_required" in report
                for report in attempt_reports
            )
        ),
        **evidence_checks,
    }
    return {
        "routing_gate_passed": all(checks.values()),
        "checks": checks,
        "runs": len(run_ids_seen),
        "judged_scenarios": len(cluster_means),
        "judgements": judgement_count,
        "quality_bootstrap_unit": "two-way run and scenario",
        "mean_quality_delta": mean_delta,
        "quality_bootstrap_95pct_lower": ci_lower,
        "savings_percent": savings,
        "high_turns": high_turns,
        "high_turns_meeting_floor": safe_high_turns,
        "high_difficulty_recall": recall,
        "max_token_truncations": truncations,
        "objective_assertions": len(objective),
        "objective_pass_rate": objective_rate,
        "judge_models": list(expected_judge_models or ()),
        "provenance": expected_provenance,
        "attempt_accounting": {
            "failed_attempts": sum(report.get("failed_attempts", 0) for report in attempt_reports),
            "known_retry_cost_usd": sum(report.get("known_retry_cost_usd", 0) for report in attempt_reports),
            "invoice_reconciliation_required": any(
                report.get("invoice_reconciliation_required", True) for report in attempt_reports
            ),
        },
        "arm_costs_usd": arm_cost_totals,
        "arm_objective_pass_rate": {
            arm: (sum(values) / len(values) if values else None)
            for arm, values in arm_objectives.items()
        },
        "octopus_savings_vs_fixed_percent": {
            arm: ((cost - arm_cost_totals.get("octopus", 0)) / cost * 100 if cost else None)
            for arm, cost in arm_cost_totals.items() if arm != "octopus"
        },
        "minimum_turns_observed_per_run": min(turns_per_run),
        "suite": {
            "name": expected_suite[0],
            "sha256": expected_suite[1],
            "scenario_count": len(expected_suite[2]),
        },
        "note": "This is only the routing evaluation gate. Critical-failure, reliability, concurrency, cost-reconciliation, security, release, and soak gates require separate evidence before production readiness can be declared.",
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("results", nargs="+", type=Path)
    parser.add_argument("--gates", type=Path, default=Path(__file__).parents[1] / "production-gates.json")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--expected-commit", help="require every result to match this release commit")
    args = parser.parse_args()
    gates = json.loads(args.gates.read_text())
    if args.expected_commit:
        gates["expected_release_commit"] = args.expected_commit
    report = evaluate(args.results, gates)
    rendered = json.dumps(report, indent=2) + "\n"
    if args.output:
        args.output.write_text(rendered)
    sys.stdout.write(rendered)
    return 0 if report["routing_gate_passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
