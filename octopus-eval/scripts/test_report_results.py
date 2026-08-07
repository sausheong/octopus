#!/usr/bin/env python3
"""Regression tests for the canonical evaluation result/report validator."""

import html
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
import shutil
import subprocess
import sys

import report_results


class ReportResultsTests(unittest.TestCase):
    def fixture(self, root: Path):
        results = root / "results.tsv"
        expected = root / "expected.txt"
        log = root / "run.log"
        results.write_text("A\tA\tyes\tPASS\tCheck A\tmethod\tok\n")
        expected.write_text("A\n")
        log.write_text("fixture run\n")
        return SimpleNamespace(
            results=str(results), expected=str(expected), log=str(log),
            summary=str(root / "summary.md"), checklist=str(root / "checklist.md"),
            json=str(root / "results.json"), subject="fixture", tiers="A",
            status="0", fingerprint="sha256:fixture", subject_root=str(root),
            base_commit="abc1234", worktree_state="clean",
            routing_config=json.dumps({"strategy": "amortized", "cost_mode": "absolute"}),
        )

    def test_missing_mandatory_result_is_rejected(self):
        with tempfile.TemporaryDirectory() as name:
            args = self.fixture(Path(name))
            Path(args.expected).write_text("A\nMISSING\n")
            with self.assertRaisesRegex(ValueError, "missing expected results"):
                report_results.create_payload(args)

    def test_duplicate_check_id_is_rejected(self):
        with tempfile.TemporaryDirectory() as name:
            args = self.fixture(Path(name))
            row = Path(args.results).read_text()
            Path(args.results).write_text(row + row)
            with self.assertRaisesRegex(ValueError, "duplicate check IDs"):
                report_results.create_payload(args)

    def test_generated_markdown_matches_structured_results(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            args = self.fixture(root)
            report_results.generate(args)
            payload = json.loads(Path(args.json).read_text())
            for report in (Path(args.summary), Path(args.checklist)):
                metadata, rows = report_results.parse_markdown(report)
                self.assertEqual(metadata, report_results.expected_metadata(payload))
                self.assertEqual(rows, payload["checks"])

    def test_escaped_pipe_and_backtick_round_trip(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            args = self.fixture(root)
            Path(args.results).write_text(
                "A\tA\tyes\tPASS\tCheck A | B\tmethod `safe`\tleft | right\n"
            )
            report_results.generate(args)
            _, rows = report_results.parse_markdown(Path(args.summary))
            self.assertEqual(rows[0]["check"], "Check A | B")
            self.assertEqual(rows[0]["method"], "method `safe`")
            self.assertEqual(rows[0]["result"], "left | right")
            shutil.copy(Path(__file__).parents[1] / "render.py", root / "render.py")
            subprocess.run(
                [sys.executable, str(root / "render.py"), "--no-open"],
                cwd=root,
                check=True,
            )
            report_results.validate(SimpleNamespace(
                json=args.json,
                summary=args.summary,
                checklist=args.checklist,
                html=str(root / "report.html"),
            ))


if __name__ == "__main__":
    unittest.main()
