#!/usr/bin/env python3
"""Generate and verify Octopus evaluation reports from one structured result set."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
from html.parser import HTMLParser
import json
from pathlib import Path
import re
import sys


VALID_REQUIRED = {"yes", "no"}
VALID_STATUS = {"PASS", "FAIL", "SKIP"}
FIELDS = ("id", "tier", "required", "status", "check", "method", "result")


def read_tsv(path: Path) -> list[dict[str, str]]:
    rows: list[dict[str, str]] = []
    for number, line in enumerate(path.read_text(errors="replace").splitlines(), 1):
        cells = line.split("\t")
        if len(cells) != len(FIELDS):
            raise ValueError(f"{path}:{number}: expected 7 tab-separated fields, got {len(cells)}")
        row = dict(zip(FIELDS, cells))
        if not row["id"]:
            raise ValueError(f"{path}:{number}: empty check ID")
        if row["required"] not in VALID_REQUIRED:
            raise ValueError(f"{path}:{number}: invalid mandatory value {row['required']!r}")
        if row["status"] not in VALID_STATUS:
            raise ValueError(f"{path}:{number}: invalid status {row['status']!r}")
        if row["required"] == "yes" and row["status"] == "SKIP":
            raise ValueError(f"{path}:{number}: mandatory check {row['id']} cannot be skipped")
        rows.append(row)
    ids = [row["id"] for row in rows]
    duplicates = sorted({ident for ident in ids if ids.count(ident) > 1})
    if duplicates:
        raise ValueError(f"duplicate check IDs: {', '.join(duplicates)}")
    return rows


def read_expected(path: Path) -> list[str]:
    ids = [line.strip() for line in path.read_text().splitlines() if line.strip()]
    duplicates = sorted({ident for ident in ids if ids.count(ident) > 1})
    if duplicates:
        raise ValueError(f"duplicate expected check IDs: {', '.join(duplicates)}")
    return ids


def escape_cell(value: object) -> str:
    return str(value).replace("|", "\\|").replace("`", "\\`")


def markdown_table(rows: list[dict[str, str]]) -> list[str]:
    result = [
        "| ID | Tier | Check | Method | Result | Status | Mandatory |",
        "|---|---|---|---|---|---:|---:|",
    ]
    order = ("id", "tier", "check", "method", "result", "status", "required")
    for row in rows:
        result.append("| " + " | ".join(escape_cell(row[key]) for key in order) + " |")
    return result


def routing_lines(routing: dict[str, object]) -> list[str]:
    labels = (
        ("scope", "Scope"),
        ("strategy", "Strategy"),
        ("cost_mode", "Cost mode"),
        ("cost_reference_usd", "Cost reference"),
        ("high_quality_floor", "High-quality floor"),
        ("reasoning_bonus", "Reasoning bonus"),
        ("default_remaining_turns", "Default remaining turns"),
        ("cache_aware", "Cache aware"),
    )
    lines = []
    for key, label in labels:
        value = routing.get(key, "not recorded")
        if key == "cost_reference_usd" and isinstance(value, (int, float)):
            value = f"${value:.2f}"
        elif isinstance(value, bool):
            value = str(value).lower()
        lines.append(f"- {label}: `{value}`")
    return lines


def snapshot_fingerprint(root: Path) -> str:
    """Hash the evaluated files, including injected tests but excluding Git metadata."""
    digest = hashlib.sha256()
    files = sorted(
        path for path in root.rglob("*")
        if path.is_file() and ".git" not in path.relative_to(root).parts
    )
    for path in files:
        relative = path.relative_to(root).as_posix().encode()
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        with path.open("rb") as source:
            while chunk := source.read(1024 * 1024):
                digest.update(chunk)
    return "sha256:" + digest.hexdigest()


def create_payload(args: argparse.Namespace) -> dict[str, object]:
    rows = read_tsv(Path(args.results))
    expected = read_expected(Path(args.expected))
    present = {row["id"] for row in rows}
    missing = [ident for ident in expected if ident not in present]
    if missing:
        raise ValueError(f"missing expected results: {', '.join(missing)}")
    unexpected_mandatory = [
        row["id"] for row in rows if row["required"] == "yes" and row["id"] not in expected
    ]
    if unexpected_mandatory:
        raise ValueError(
            "mandatory results were not registered before execution: "
            + ", ".join(unexpected_mandatory)
        )
    routing = json.loads(args.routing_config)
    if not isinstance(routing, dict):
        raise ValueError("routing configuration must be a JSON object")
    mandatory_failures = sum(
        row["required"] == "yes" and row["status"] == "FAIL" for row in rows
    )
    generated = dt.datetime.now().astimezone().strftime("%Y-%m-%d %H:%M:%S %Z")
    fingerprint = args.fingerprint
    if fingerprint == "auto":
        fingerprint = snapshot_fingerprint(Path(args.subject_root))
    payload: dict[str, object] = {
        "schema_version": 1,
        "generated": generated,
        "subject": args.subject,
        "subject_fingerprint": fingerprint,
        "base_commit": args.base_commit,
        "worktree_state": args.worktree_state,
        "requested_tiers": args.tiers,
        "exit_status": int(args.status),
        "mandatory_failure_count": mandatory_failures,
        "expected_check_ids": expected,
        "routing_configuration": routing,
        "checks": rows,
    }
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
    payload["result_set_sha256"] = hashlib.sha256(canonical).hexdigest()
    return payload


def render_markdown(payload: dict[str, object], log: str, *, checklist: bool = False) -> str:
    rows = payload["checks"]
    assert isinstance(rows, list)
    title = "# Verification checklist - current run" if checklist else "# Octopus evaluation run summary"
    status = int(payload["exit_status"])
    lines = [
        title,
        "",
        f"- Generated: {payload['generated']}",
        f"- Subject: {payload['subject']}",
        f"- Subject fingerprint: `{payload['subject_fingerprint']}`",
        f"- Base commit: `{payload['base_commit']}`",
        f"- Worktree: {payload['worktree_state']}",
        f"- Requested tiers: {payload['requested_tiers']}",
        f"- Exit status: {status} ({'passed' if status == 0 else 'failed'})",
        f"- Mandatory failures: {payload['mandatory_failure_count']}",
        f"- Result-set SHA-256: `{payload['result_set_sha256']}`",
        "",
        "## Routing configuration under test",
        "",
        *routing_lines(payload["routing_configuration"]),
        "",
        "Scenario-specific checks may override these production defaults; their method and result fields identify the exercised path.",
        "",
        "## Current-run checklist",
        "",
        *markdown_table(rows),
        "",
        "Every row above is generated from `current-run-results.json`.",
        "",
    ]
    if checklist:
        lines.append("This file is regenerated by `run.sh`. Historical results are kept under `historical/`.")
    else:
        lines.extend(("## Console output", "", "```text", log.rstrip(), "```"))
    lines.append("")
    return "\n".join(lines)


def generate(args: argparse.Namespace) -> None:
    payload = create_payload(args)
    json_path = Path(args.json)
    json_path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
    log = Path(args.log).read_text(errors="replace")
    Path(args.summary).write_text(render_markdown(payload, log))
    Path(args.checklist).write_text(render_markdown(payload, "", checklist=True))


def parse_markdown(path: Path) -> tuple[dict[str, str], list[dict[str, str]]]:
    text = path.read_text(errors="replace")
    metadata: dict[str, str] = {}
    for key in (
        "Generated", "Subject", "Subject fingerprint", "Base commit", "Worktree",
        "Requested tiers", "Exit status", "Mandatory failures", "Result-set SHA-256",
    ):
        match = re.search(rf"^- {re.escape(key)}: (.+)$", text, re.MULTILINE)
        if not match:
            raise ValueError(f"{path}: missing {key!r} metadata")
        metadata[key] = match.group(1).strip("`")
    rows: list[dict[str, str]] = []
    table_match = re.search(
        r"\| ID \| Tier \| Check \| Method \| Result \| Status \| Mandatory \|\n"
        r"\|[-:|]+\|\n(?P<body>(?:\|.*\|\n)*)",
        text,
    )
    if not table_match:
        raise ValueError(f"{path}: current-run checklist table not found")
    for line in table_match.group("body").splitlines():
        cells = [cell.strip().replace("\\|", "|").replace("\\`", "`")
                 for cell in re.split(r"(?<!\\)\|", line.strip().strip("|"))]
        if len(cells) != 7:
            raise ValueError(f"{path}: malformed checklist row: {line}")
        keys = ("id", "tier", "check", "method", "result", "status", "required")
        rows.append(dict(zip(keys, cells)))
    return metadata, rows


class ReportHTMLParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.text: list[str] = []
        self.tables: list[list[list[str]]] = []
        self._table: list[list[str]] | None = None
        self._row: list[str] | None = None
        self._cell: list[str] | None = None

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag == "table": self._table = []
        elif tag == "tr" and self._table is not None: self._row = []
        elif tag in {"th", "td"} and self._row is not None: self._cell = []
        elif tag == "code" and self._cell is not None: self._cell.append("`")

    def handle_data(self, data: str) -> None:
        self.text.append(data)
        if self._cell is not None: self._cell.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "code" and self._cell is not None:
            self._cell.append("`")
            return
        if tag in {"th", "td"} and self._cell is not None and self._row is not None:
            self._row.append("".join(self._cell).strip())
            self._cell = None
        elif tag == "tr" and self._row is not None and self._table is not None:
            self._table.append(self._row)
            self._row = None
        elif tag == "table" and self._table is not None:
            self.tables.append(self._table)
            self._table = None


def expected_metadata(payload: dict[str, object]) -> dict[str, str]:
    status = int(payload["exit_status"])
    return {
        "Generated": str(payload["generated"]),
        "Subject": str(payload["subject"]),
        "Subject fingerprint": str(payload["subject_fingerprint"]),
        "Base commit": str(payload["base_commit"]),
        "Worktree": str(payload["worktree_state"]),
        "Requested tiers": str(payload["requested_tiers"]),
        "Exit status": f"{status} ({'passed' if status == 0 else 'failed'})",
        "Mandatory failures": str(payload["mandatory_failure_count"]),
        "Result-set SHA-256": str(payload["result_set_sha256"]),
    }


def validate(args: argparse.Namespace) -> None:
    payload = json.loads(Path(args.json).read_text())
    expected_rows = payload["checks"]
    expected_ids = payload["expected_check_ids"]
    actual_ids = [row["id"] for row in expected_rows]
    missing = [ident for ident in expected_ids if ident not in actual_ids]
    if missing:
        raise ValueError(f"structured result set is missing expected checks: {', '.join(missing)}")
    calculated_failures = sum(
        row["required"] == "yes" and row["status"] == "FAIL" for row in expected_rows
    )
    if calculated_failures != payload["mandatory_failure_count"]:
        raise ValueError("structured mandatory-failure count does not match its checks")
    expected_meta = expected_metadata(payload)
    for report in (Path(args.summary), Path(args.checklist)):
        metadata, rows = parse_markdown(report)
        if metadata != expected_meta:
            raise ValueError(f"{report}: metadata does not match current-run-results.json")
        if rows != expected_rows:
            raise ValueError(f"{report}: checklist does not match current-run-results.json")
    parser = ReportHTMLParser()
    parser.feed(Path(args.html).read_text(errors="replace"))
    html_text = " ".join(" ".join(parser.text).split())
    for key, value in expected_meta.items():
        if f"{key}: {value}" not in html_text:
            raise ValueError(f"{args.html}: missing or mismatched {key!r}")
    wanted_table = [["ID", "Tier", "Check", "Method", "Result", "Status", "Mandatory"]]
    wanted_table.extend([
        [row[key] for key in ("id", "tier", "check", "method", "result", "status", "required")]
        for row in expected_rows
    ])
    if wanted_table not in parser.tables:
        raise ValueError(f"{args.html}: checklist does not match current-run-results.json")
    print(
        f"report integrity OK: {len(expected_rows)} checks, "
        f"{calculated_failures} mandatory failures, result set {payload['result_set_sha256'][:12]}"
    )


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    sub = root.add_subparsers(dest="command", required=True)
    gen = sub.add_parser("generate")
    for name in ("results", "expected", "log", "summary", "checklist", "json",
                 "subject", "tiers", "status", "fingerprint", "subject-root", "base-commit",
                 "worktree-state", "routing-config"):
        gen.add_argument(f"--{name}", required=True)
    gen.set_defaults(func=generate)
    check = sub.add_parser("validate")
    for name in ("json", "summary", "checklist", "html"):
        check.add_argument(f"--{name}", required=True)
    check.set_defaults(func=validate)
    return root


def main() -> int:
    args = parser().parse_args()
    try:
        args.func(args)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"report integrity error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
