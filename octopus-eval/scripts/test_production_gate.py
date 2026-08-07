import json
import tempfile
import unittest
from pathlib import Path

from production_gate import evaluate, hierarchical_bootstrap_lower


GATES = {
    "mean_quality_delta_min": -0.25,
    "quality_ci_lower_min": -0.5,
    "savings_percent_min": 30,
    "high_difficulty_recall_min": 0.95,
    "high_quality_floor": 0.95,
    "model_quality": {"opus": 0.98, "haiku": 0.72},
}


class ProductionGateTest(unittest.TestCase):
    def result(self, model="opus", routed="high", octopus_score=8.4):
        return {
            "configuration": {
                "run_id": "run-1",
                "suite": {"name": "test", "sha256": "a" * 64, "scenario_ids": ["case"]},
            },
            "costs": {"octopus_total_usd": 4, "opus_total_usd": 10},
            "judgements": [{"scenario_id": "case", "octopus_score": octopus_score, "opus_score": 8.5}],
            "runs": {"case": {
                "octopus": {"turns": [{
                    "difficulty": "high", "routed_difficulty": routed, "model": model,
                }]},
                "opus": {"turns": [{}]},
            }},
        }

    def write(self, value):
        handle = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
        json.dump(value, handle)
        handle.close()
        return Path(handle.name)

    def test_passing_evidence(self):
        report = evaluate([self.write(self.result())], GATES)
        self.assertTrue(report["routing_gate_passed"])

    def test_cheap_but_low_quality_tier_fails(self):
        report = evaluate([self.write(self.result(model="haiku", routed="low", octopus_score=7))], GATES)
        self.assertFalse(report["routing_gate_passed"])
        self.assertFalse(report["checks"]["high_tier_safety"])
        self.assertFalse(report["checks"]["high_difficulty_recall"])

    def test_repeat_runs_are_clustered_by_scenario(self):
        first = self.result()
        first["judgements"] = [
            {"scenario_id": "a", "octopus_score": 8.0, "opus_score": 8.5},
            {"scenario_id": "b", "octopus_score": 8.6, "opus_score": 8.5},
        ]
        first["configuration"]["suite"]["scenario_ids"] = ["a", "b"]
        first["runs"] = {
            scenario_id: {
                "octopus": {"turns": [{
                    "difficulty": "high", "routed_difficulty": "high", "model": "opus",
                }]},
                "opus": {"turns": [{}]},
            }
            for scenario_id in ("a", "b")
        }
        repeated = json.loads(json.dumps(first))
        repeated["configuration"]["run_id"] = "run-2"
        once = evaluate([self.write(first)], GATES)
        twice = evaluate([self.write(first), self.write(repeated)], GATES)
        self.assertEqual(once["judged_scenarios"], 2)
        self.assertEqual(twice["judged_scenarios"], 2)
        self.assertEqual(twice["judgements"], 4)
        self.assertEqual(
            once["quality_bootstrap_95pct_lower"],
            twice["quality_bootstrap_95pct_lower"],
        )

    def test_missing_scenario_id_is_rejected(self):
        value = self.result()
        del value["judgements"][0]["scenario_id"]
        with self.assertRaisesRegex(ValueError, "has no scenario_id"):
            evaluate([self.write(value)], GATES)

    def test_missing_routing_evidence_counts_against_recall(self):
        value = self.result()
        del value["runs"]["case"]["octopus"]["turns"][0]["routed_difficulty"]
        report = evaluate([self.write(value)], GATES)
        self.assertEqual(report["high_difficulty_recall"], 0)
        self.assertFalse(report["checks"]["high_difficulty_recall"])

    def test_judgements_must_match_run_ids(self):
        value = self.result()
        value["judgements"][0]["scenario_id"] = "invented"
        with self.assertRaisesRegex(ValueError, "do not match runs"):
            evaluate([self.write(value)], GATES)

    def test_suite_identity_must_match_across_repeats(self):
        first = self.result()
        second = self.result()
        second["configuration"]["run_id"] = "run-2"
        second["configuration"]["suite"]["sha256"] = "b" * 64
        with self.assertRaisesRegex(ValueError, "does not match"):
            evaluate([self.write(first), self.write(second)], GATES)

    def test_declared_evidence_minima_prevent_tiny_pass(self):
        strict = {**GATES, "minimum_runs": 5, "minimum_judged_scenarios": 50, "minimum_turns_per_run": 200}
        report = evaluate([self.write(self.result())], strict)
        self.assertFalse(report["routing_gate_passed"])
        self.assertFalse(report["checks"]["minimum_runs"])
        self.assertFalse(report["checks"]["minimum_judged_scenarios"])
        self.assertFalse(report["checks"]["minimum_turns_per_run"])

    def test_max_token_stop_fails_routing_gate(self):
        value = self.result()
        value["runs"]["case"]["octopus"]["turns"][0]["stop_reason"] = "max_tokens"
        report = evaluate([self.write(value)], GATES)
        self.assertEqual(report["max_token_truncations"], 1)
        self.assertFalse(report["checks"]["truncations"])
        self.assertFalse(report["routing_gate_passed"])

    def test_two_way_bootstrap_includes_repeat_variance(self):
        lower = hierarchical_bootstrap_lower({
            "run-a": {"scenario-a": 1.0, "scenario-b": 1.0},
            "run-b": {"scenario-a": -1.0, "scenario-b": -1.0},
        })
        self.assertLessEqual(lower, -0.9)

    def test_duplicate_run_id_is_rejected(self):
        first = self.result()
        second = self.result()
        with self.assertRaisesRegex(ValueError, "duplicate run_id"):
            evaluate([self.write(first), self.write(second)], GATES)

    def test_declared_baselines_objective_judges_and_concurrency_pass(self):
        value = self.result()
        value["configuration"]["arms"] = ["octopus", "haiku", "sonnet", "opus"]
        value["configuration"]["judge_models"] = ["judge-a", "judge-b"]
        value["runs"]["case"]["haiku"] = {"turns": [{}]}
        value["runs"]["case"]["sonnet"] = {"turns": [{}]}
        value["runs"]["case"]["octopus"]["objective_assertions"] = [
            {"id": "deterministic", "passed": True}
        ]
        value["judgements"] = [
            {"scenario_id": "case", "judge_model": judge, "octopus_score": 8.4, "opus_score": 8.5}
            for judge in ("judge-a", "judge-b")
        ]
        value["concurrent_workload"] = {
            "observed_max_concurrency": 2,
            "completed_streams": 2,
            "stream_ids": ["stream-a", "stream-b"],
            "canaries_echoed": 2, "crossovers": [], "isolation_verified": True,
            "results": [
                {"session_id": stream, "workflow_id": stream, "cache_namespace": stream,
                 "decision_mapping": [{}], "turns": [{}]}
                for stream in ("stream-a", "stream-b")
            ],
        }
        strict = {
            **GATES,
            "required_arms": ["octopus", "haiku", "sonnet", "opus"],
            "minimum_objective_assertions": 1,
            "objective_pass_rate_min": 1,
            "minimum_judge_models": 2,
            "minimum_concurrency": 2,
            "minimum_concurrent_streams": 2,
        }
        report = evaluate([self.write(value)], strict)
        self.assertTrue(report["routing_gate_passed"])
        self.assertEqual(report["judge_models"], ["judge-a", "judge-b"])


if __name__ == "__main__":
    unittest.main()
