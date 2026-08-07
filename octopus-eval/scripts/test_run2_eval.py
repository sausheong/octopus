import json
import subprocess
import io
import urllib.error
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from production_gate import evaluate
from run2_eval import (
    OPUS,
    SONNET,
    HAIKU,
    SpendBudget,
    attach_routing_evidence,
    cache_namespaces,
    checkpoint_judgement_then_charge,
    grade_assertions,
    load_checkpoint,
    load_scenario_suite,
    parse_judgement,
    post_json,
    run_concurrent_streams,
    run_arm,
    save_checkpoint,
)


class RoutingEvidenceTest(unittest.TestCase):
    def test_deterministic_assertion_graders(self):
        scenario = {"assertions": [
            {"id": "has-word", "type": "contains", "turn": 1, "value": "safe"},
            {"id": "bounded", "type": "max_words", "turn": 1, "value": 3},
            {"id": "no-secret", "type": "not_contains", "turn": "all", "value": "password"},
        ]}
        grades = grade_assertions(scenario, {"turns": [{"response": "safe concise answer"}]})
        self.assertTrue(all(grade["passed"] for grade in grades))

    def test_concurrent_mode_records_isolated_streams(self):
        lock = __import__("threading").Lock()
        active = 0
        maximum = 0
        def fake_run(arm, scenario, *args, **kwargs):
            nonlocal active, maximum
            import time
            with lock:
                active += 1
                maximum = max(maximum, active)
            time.sleep(0.02)
            with lock:
                active -= 1
            namespace = args[4]
            canary = scenario["turns"][0][1].split("exact token ", 1)[1].split(".", 1)[0]
            return {"stream_id": f"{scenario['id']}:{namespace}", "turns": [
                {"turn": 1, "model": OPUS, "response": canary, "cost_usd": 0.0}
            ]}
        scenarios = [{"id": f"s{i}", "turns": [("low", "x")]} for i in range(4)]
        with mock.patch("run2_eval.run_arm", side_effect=fake_run):
            evidence = run_concurrent_streams(4, scenarios, "o", "d", "k", 0, "run", None)
        self.assertEqual(evidence["completed_streams"], 4)
        self.assertEqual(len(set(evidence["stream_ids"])), 4)
        self.assertGreaterEqual(evidence["observed_max_concurrency"], 2)
        self.assertGreaterEqual(maximum, 2)
        self.assertTrue(evidence["isolation_verified"])
        self.assertEqual(evidence["crossovers"], [])

    def test_retry_attempts_capture_status_latency_and_usage(self):
        failure = urllib.error.HTTPError(
            "http://example", 503, "unavailable", {},
            io.BytesIO(b'{"usage":{"input_tokens":2,"output_tokens":1}}'),
        )
        class Response:
            status = 200
            def __enter__(self): return self
            def __exit__(self, *args): return False
            def read(self):
                return b'{"type":"message","model":"claude-opus-4-8-global","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}'
        with mock.patch("run2_eval.urllib.request.urlopen", side_effect=[failure, Response()]):
            with mock.patch("run2_eval.time.sleep"):
                message, _ = post_json("http://example", {"x": 1}, "key", retries=2)
        attempts = message["_eval_attempts"]
        self.assertEqual([attempt["status"] for attempt in attempts], [503, 200])
        self.assertEqual(attempts[0]["usage"]["input_tokens"], 2)

    def test_judge_scores_must_be_bounded_and_finite(self):
        with self.assertRaisesRegex(ValueError, "between 0 and 10"):
            parse_judgement('{"winner":"A","a_score":100,"b_score":5}')
        with self.assertRaisesRegex(ValueError, "finite"):
            parse_judgement('{"winner":"B","a_score":NaN,"b_score":5}')

    def test_paired_arms_never_share_provider_cache_namespace(self):
        octopus, direct = cache_namespaces("run-123")
        self.assertNotEqual(octopus, direct)
        self.assertEqual(octopus, "run-123:octopus")
        self.assertEqual(direct, "run-123:direct")

    def test_direct_arm_rejects_non_opus_response_metadata(self):
        response = {
            "type": "message", "model": "claude-sonnet-4-6-asia-southeast1",
            "content": [{"type": "text", "text": "answer"}], "usage": {},
        }
        scenario = {"id": "case", "turns": [("low", "prompt"), ("low", "follow-up")]}
        with mock.patch("run2_eval.post_json", return_value=(response, 1)):
            with self.assertRaisesRegex(RuntimeError, "refusing to mislabel"):
                run_arm("opus", scenario, "http://octopus", "http://direct", "key", 0, "cache")

    def test_budget_crossing_is_checkpointed_before_stop(self):
        response = {
            "type": "message", "model": OPUS,
            "content": [{"type": "text", "text": "paid answer"}],
            "usage": {"output_tokens": 1},
        }
        scenario = {"id": "case", "turns": [("low", "prompt"), ("low", "follow-up")]}
        existing = {"arm": "opus", "scenario_id": "case", "turns": [], "complete": False}
        snapshots = []
        with mock.patch("run2_eval.post_json", return_value=(response, 1)):
            with self.assertRaisesRegex(RuntimeError, "budget exceeded"):
                run_arm(
                    "opus", scenario, "http://octopus", "http://direct", "key", 0, "cache",
                    SpendBudget(0.000001), existing, lambda result: snapshots.append(len(result["turns"])),
                )
        self.assertEqual(len(existing["turns"]), 1)
        self.assertEqual(snapshots, [1])

    def test_judge_budget_crossing_is_checkpointed_before_stop(self):
        judgements = []
        snapshots = []
        judgement = {"scenario_id": "case", "cost_usd": 0.01}
        with self.assertRaisesRegex(RuntimeError, "budget exceeded"):
            checkpoint_judgement_then_charge(
                judgements,
                judgement,
                lambda: snapshots.append(list(judgements)),
                SpendBudget(0.001),
            )
        self.assertEqual(judgements, [judgement])
        self.assertEqual(snapshots, [[judgement]])

    def test_attaches_prompt_free_profile(self):
        turns = [{"model": "claude-opus-4-8-global"}]
        ledger = {"recent_decisions": [{
            "actual_model": "litellm/claude-opus-4-8-global",
            "difficulty": "high",
            "risk": "critical",
            "classification_source": "classifier",
            "classification_status": "completed",
            "applied_quality_floor": 0.95,
        }]}
        attach_routing_evidence(ledger, turns)
        self.assertEqual(turns[0]["routed_difficulty"], "high")
        self.assertEqual(turns[0]["routed_risk"], "critical")
        self.assertEqual(turns[0]["applied_quality_floor"], 0.95)

    def test_rejects_misaligned_models(self):
        turns = [{"model": "claude-opus-4-8-global"}]
        ledger = {"recent_decisions": [{"actual_model": "litellm/claude-haiku-4-5"}]}
        with self.assertRaises(RuntimeError):
            attach_routing_evidence(ledger, turns)


class ScenarioSuiteTest(unittest.TestCase):
    def test_custom_prefix_evidence_is_git_ignored(self):
        evaluation_root = Path(__file__).parents[1]
        repository_root = evaluation_root.parent
        candidates = [
            "octopus-eval/production-01-results.json",
            "octopus-eval/production-01-results.json.partial",
            "octopus-eval/production-01-summary.md",
            "octopus-eval/production-01-insights.json",
            "octopus-eval/production-01.log",
        ]
        for candidate in candidates:
            completed = subprocess.run(
                ["git", "check-ignore", "--quiet", "--", candidate],
                cwd=repository_root,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, candidate)

    def test_smoke_suite_is_external_and_multi_turn(self):
        scenarios, metadata = load_scenario_suite("smoke")
        self.assertEqual(metadata["name"], "smoke")
        self.assertEqual(len(scenarios), 6)
        self.assertTrue(all(len(scenario["turns"]) >= 2 for scenario in scenarios))

    def test_production_suite_has_required_coverage(self):
        scenarios, metadata = load_scenario_suite("production")
        self.assertGreaterEqual(len(scenarios), 50)
        self.assertEqual(len(metadata["scenario_ids"]), len(set(metadata["scenario_ids"])))
        self.assertTrue(all(len(scenario["turns"]) >= 3 for scenario in scenarios))

        domains = {
            domain
            for scenario in scenarios
            for domain in scenario["domains"]
        }
        self.assertTrue({
            "coding", "security", "operations", "data_analysis", "writing",
            "support", "math_reasoning", "tool_workflows", "short_but_hard",
            "cache_heavy", "subagent_workflows",
        }.issubset(domains))

        counts = {difficulty: 0 for difficulty in ("trivial", "low", "medium", "high")}
        for scenario in scenarios:
            for difficulty, _ in scenario["turns"]:
                counts[difficulty] += 1
        self.assertGreaterEqual(counts["high"], 50)
        self.assertGreaterEqual(counts["medium"], 50)
        self.assertGreaterEqual(counts["low"] + counts["trivial"], 50)
        self.assertEqual(sum(len(scenario["assertions"]) for scenario in scenarios), 50)

    def test_invalid_duplicate_id_is_rejected(self):
        suite = {
            "schema_version": 1,
            "name": "bad",
            "scenarios": [{
                "id": "duplicate", "name": "One", "domains": ["coding"],
                "turns": [
                    {"difficulty": "low", "prompt": "One"},
                    {"difficulty": "high", "prompt": "Two"},
                ],
            }] * 2,
        }
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bad.json"
            path.write_text(json.dumps(suite))
            with self.assertRaisesRegex(ValueError, "duplicate scenario id"):
                load_scenario_suite(scenario_file=path)

    def test_checkpoint_is_bound_to_exact_suite(self):
        _, smoke = load_scenario_suite("smoke")
        _, production = load_scenario_suite("production")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "checkpoint.json"
            execution = {
                "arm_order_seed": 7,
                "octopus_cache_namespace": "run:octopus",
                "direct_cache_namespace": "run:direct",
                "cache_namespace_by_arm": {"octopus": "run:octopus", "opus": "run:direct"},
                "arms": ["octopus", "opus"],
                "judge_models": [OPUS],
                "concurrency": 1,
            }
            save_checkpoint(path, "run", smoke, {}, [], execution)
            self.assertEqual(load_checkpoint(path, smoke)["run_id"], "run")
            with self.assertRaisesRegex(RuntimeError, "does not match"):
                load_checkpoint(path, production)

    def test_production_suite_shape_is_compatible_with_gate(self):
        scenarios, _ = load_scenario_suite("production")
        runs = {}
        for scenario in scenarios:
            turns = []
            for difficulty, _ in scenario["turns"]:
                turns.append({
                    "difficulty": difficulty,
                    "routed_difficulty": difficulty,
                    "model": OPUS,
                })
            runs[scenario["id"]] = {
                "octopus": {"turns": turns},
                "opus": {"turns": [{} for _ in turns]},
            }
        gates = {
            "mean_quality_delta_min": -0.25,
            "quality_ci_lower_min": -0.5,
            "savings_percent_min": 30,
            "high_difficulty_recall_min": 0.95,
            "high_quality_floor": 0.95,
            "model_quality": {OPUS: 0.98},
        }
        report = evaluate([self._write_result(runs)], gates)
        self.assertTrue(report["routing_gate_passed"])
        self.assertEqual(report["high_turns"], 50)
        self.assertEqual(report["minimum_turns_observed_per_run"], 200)

    def test_full_production_evidence_contract(self):
        scenarios, metadata = load_scenario_suite("production")
        gates = json.loads((Path(__file__).parents[1] / "production-gates.json").read_text())
        paths = []
        for run_index in range(5):
            runs = {}
            judgements = []
            for scenario in scenarios:
                octopus_turns = [{
                    "turn": turn_index,
                    "difficulty": difficulty,
                    "prompt": prompt,
                    "routed_difficulty": difficulty,
                    "model": OPUS,
                    "stop_reason": "end_turn",
                    "usage": {"input_tokens": 10, "output_tokens": 10},
                    "cost_usd": 0.01,
                } for turn_index, (difficulty, prompt) in enumerate(scenario["turns"], 1)]
                objective = [
                    {"id": assertion["id"], "passed": True}
                    for assertion in scenario["assertions"]
                ]
                def fixed_turns(model):
                    return [{
                        **{key: turn[key] for key in ("turn", "difficulty", "prompt", "stop_reason")},
                        "model": model,
                        "usage": {"input_tokens": 10, "output_tokens": 10},
                        "cost_usd": 0.02,
                    } for turn in octopus_turns]
                runs[scenario["id"]] = {
                    "octopus": {
                        "turns": octopus_turns,
                        "objective_assertions": objective,
                    },
                    "haiku": {"turns": fixed_turns(HAIKU), "objective_assertions": objective},
                    "sonnet": {"turns": fixed_turns(SONNET), "objective_assertions": objective},
                    "opus": {"turns": fixed_turns(OPUS), "objective_assertions": objective},
                }
                for judge in (OPUS, SONNET):
                    judgements.append({
                        "scenario_id": scenario["id"], "judge_model": judge,
                        "octopus_score": 8.4, "opus_score": 8.5,
                    })
            value = {
                "configuration": {
                    "run_id": f"production-{run_index}",
                    "suite": metadata,
                    "arms": ["octopus", "haiku", "sonnet", "opus"],
                    "judge_models": [OPUS, SONNET],
                    "provenance": {
                        "source_commit": "a" * 40,
                        "binary_sha256": "b" * 64,
                        "config_sha256": "c" * 64,
                        "effective_policy_digest": "d" * 64,
                        "provider_endpoint_identity": "https://example.invalid",
                    },
                },
                "costs": {"octopus_total_usd": 4, "opus_total_usd": 10},
                "attempt_accounting": {
                    "failed_attempts": 0, "known_retry_cost_usd": 0,
                    "invoice_reconciliation_required": False,
                },
                "judgements": judgements,
                "runs": runs,
                "concurrent_workload": {
                    "observed_max_concurrency": 50,
                    "completed_streams": 50,
                    "stream_ids": [f"run-{run_index}-stream-{i}" for i in range(50)],
                    "canaries_echoed": 50, "crossovers": [], "isolation_verified": True,
                    "results": [
                        {"session_id": f"s{i}", "workflow_id": f"s{i}", "cache_namespace": f"c{i}",
                         "decision_mapping": [{}], "turns": [{}]}
                        for i in range(50)
                    ],
                },
            }
            handle = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
            json.dump(value, handle)
            handle.close()
            path = Path(handle.name)
            paths.append(path)
            self.addCleanup(path.unlink, missing_ok=True)
        report = evaluate(paths, gates)
        self.assertTrue(report["routing_gate_passed"])
        self.assertEqual(report["objective_assertions"], 250)

    def _write_result(self, runs):
        scenario_ids = list(runs)
        handle = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
        json.dump({
            "configuration": {
                "run_id": "synthetic-run",
                "suite": {
                    "name": "synthetic",
                    "sha256": "c" * 64,
                    "scenario_ids": scenario_ids,
                },
            },
            "costs": {"octopus_total_usd": 5, "opus_total_usd": 10},
            "judgements": [
                {"scenario_id": scenario_id, "octopus_score": 8.4, "opus_score": 8.5}
                for scenario_id in scenario_ids
            ],
            "runs": runs,
        }, handle)
        handle.close()
        self.addCleanup(Path(handle.name).unlink, missing_ok=True)
        return Path(handle.name)


if __name__ == "__main__":
    unittest.main()
