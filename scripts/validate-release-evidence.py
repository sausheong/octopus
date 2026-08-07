#!/usr/bin/env python3
"""Validate a production evidence bundle against its exact candidate package."""

from __future__ import annotations

import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import re
import sys
import zipfile


REQUIRED_TYPES = {
    "routing": "routing_gate",
    "critical": "critical_review",
    "soak": "soak_test",
    "concurrency": "concurrency_test",
    "install": "installer_verification",
    "rollback": "rollback_test",
}
SHA256 = re.compile(r"[0-9a-f]{64}")
COMMIT = re.compile(r"[0-9a-f]{40}")


def fail(message: str) -> None:
    raise ValueError(message)


def read_json(path: Path, label: str) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot read {label}: {exc}")
    if not isinstance(value, dict):
        fail(f"{label} must be a JSON object")
    return value


def digest(path: Path) -> str:
    value = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                value.update(chunk)
    except OSError as exc:
        fail(f"cannot hash {path}: {exc}")
    return value.hexdigest()


def timestamp(value: object, field: str) -> datetime:
    if not isinstance(value, str):
        fail(f"{field} must be an ISO-8601 timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        fail(f"{field} must be an ISO-8601 timestamp")
    if parsed.tzinfo is None:
        fail(f"{field} must include a timezone")
    return parsed


def sha(value: object, field: str) -> str:
    if not isinstance(value, str) or not SHA256.fullmatch(value):
        fail(f"{field} must be a lowercase SHA-256 digest")
    return value


def reviewer(record: dict, label: str) -> None:
    name = record.get("reviewed_by")
    if not isinstance(name, str) or len(name.strip()) < 3:
        fail(f"{label}.reviewed_by must identify the human reviewer")
    timestamp(record.get("reviewed_at"), f"{label}.reviewed_at")


def common(record: dict, label: str, record_type: str, source_commit: str) -> None:
    if record.get("schema_version") != 1:
        fail(f"{label}.schema_version must be 1")
    if record.get("type") != record_type:
        fail(f"{label}.type must be {record_type}")
    if record.get("status") != "passed":
        fail(f"{label}.status must be passed")
    if record.get("source_commit") != source_commit:
        fail(f"{label}.source_commit must equal {source_commit}")


def validate_record(name: str, record: dict, source_commit: str, package_sha256: str,
                    binary_sha256: str, config_sha256: str, policy_sha256: str) -> None:
    label = f"evidence.{name}"
    common(record, label, REQUIRED_TYPES[name], source_commit)
    if name == "routing":
        if record.get("routing_gate_passed") is not True:
            fail(f"{label}.routing_gate_passed must be true")
        for field, expected in (("binary_sha256", binary_sha256), ("config_sha256", config_sha256),
                                ("policy_sha256", policy_sha256)):
            if record.get(field) != expected:
                fail(f"{label}.{field} does not match the manifest")
        for field, minimum in (("distinct_runs", 5), ("scenarios_per_run", 50), ("routed_turns_per_run", 200)):
            if not isinstance(record.get(field), int) or record[field] < minimum:
                fail(f"{label}.{field} must be at least {minimum}")
    elif name in ("critical", "rollback"):
        sha(record.get("artifact_sha256"), f"{label}.artifact_sha256")
        reviewer(record, label)
        if name == "rollback" and record.get("rollback_status") != "passed":
            fail(f"{label}.rollback_status must be passed")
    elif name == "soak":
        if record.get("binary_sha256") != binary_sha256:
            fail(f"{label}.binary_sha256 does not match the manifest")
        started = timestamp(record.get("started_at"), f"{label}.started_at")
        ended = timestamp(record.get("ended_at"), f"{label}.ended_at")
        if (ended - started).total_seconds() < 72 * 60 * 60:
            fail(f"{label} duration must be at least 72 hours")
        requests = record.get("requests")
        errors = record.get("router_errors")
        error_rate = record.get("error_rate")
        error_slo = record.get("error_rate_slo_max")
        availability = record.get("availability")
        availability_slo = record.get("availability_slo_min")
        if not isinstance(requests, int) or requests < 1:
            fail(f"{label}.requests must be a positive integer")
        if not isinstance(errors, int) or errors < 0:
            fail(f"{label}.router_errors must be a non-negative integer")
        for field, value in (("error_rate", error_rate), ("error_rate_slo_max", error_slo),
                             ("availability", availability), ("availability_slo_min", availability_slo)):
            if not isinstance(value, (int, float)) or isinstance(value, bool) or not 0 <= value <= 1:
                fail(f"{label}.{field} must be in [0,1]")
        if error_rate > error_slo or availability < availability_slo:
            fail(f"{label} does not meet its declared SLOs")
        if abs(error_rate - errors / requests) > 1e-9:
            fail(f"{label}.error_rate does not match router_errors / requests")
    elif name == "concurrency":
        if record.get("binary_sha256") != binary_sha256:
            fail(f"{label}.binary_sha256 does not match the manifest")
        if not isinstance(record.get("parallel_streams"), int) or record["parallel_streams"] < 50:
            fail(f"{label}.parallel_streams must be at least 50")
        if record.get("session_crossovers") != 0 or record.get("failures") != 0:
            fail(f"{label} must report zero session crossovers and failures")
    elif name == "install":
        if record.get("package_sha256") != package_sha256:
            fail(f"{label}.package_sha256 does not match the candidate package")
        if record.get("notarization_status") != "accepted" or record.get("staple_validated") is not True:
            fail(f"{label} must show accepted notarisation and a validated staple")
        if record.get("install_status") != "passed" or record.get("upgrade_status") != "passed":
            fail(f"{label} install and upgrade status must be passed")


def validate(path: Path, version: str, source_commit: str, package: Path) -> dict:
    document = read_json(path, "release evidence manifest")
    if document.get("schema_version") != 2:
        fail("schema_version must be 2")
    if document.get("release") != version:
        fail(f"release must equal {version}")
    if not COMMIT.fullmatch(source_commit) or document.get("source_commit") != source_commit:
        fail(f"source_commit must equal the reviewed commit {source_commit}")
    package_sha256 = digest(package)
    if document.get("candidate_pkg_sha256") != package_sha256:
        fail("candidate_pkg_sha256 does not match the candidate package")
    if document.get("decision") != "approved_for_production":
        fail("decision must be approved_for_production")
    reviewer(document, "manifest")
    binary_sha256 = sha(document.get("candidate_binary_sha256"), "candidate_binary_sha256")
    config_sha256 = sha(document.get("config_sha256"), "config_sha256")
    policy_sha256 = sha(document.get("policy_sha256"), "policy_sha256")

    evidence = document.get("evidence")
    if not isinstance(evidence, dict):
        fail("evidence must be an object")
    missing = [name for name in REQUIRED_TYPES if name not in evidence]
    if missing:
        fail("missing evidence artefacts: " + ", ".join(missing))
    base = path.resolve().parent
    for name, expected_type in REQUIRED_TYPES.items():
        descriptor = evidence[name]
        label = f"evidence.{name}"
        if not isinstance(descriptor, dict) or descriptor.get("type") != expected_type:
            fail(f"{label}.type must be {expected_type}")
        relative = descriptor.get("path")
        if not isinstance(relative, str) or not relative or Path(relative).is_absolute():
            fail(f"{label}.path must be a relative local path")
        artifact = (base / relative).resolve()
        try:
            artifact.relative_to(base)
        except ValueError:
            fail(f"{label}.path escapes the evidence bundle")
        expected_digest = sha(descriptor.get("sha256"), f"{label}.sha256")
        if digest(artifact) != expected_digest:
            fail(f"{label}.sha256 does not match {relative}")
        validate_record(name, read_json(artifact, label), source_commit, package_sha256,
                        binary_sha256, config_sha256, policy_sha256)
    return document


def write_bundle(manifest: Path, document: dict, output: Path) -> None:
    base = manifest.resolve().parent
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.write(manifest, "manifest.json")
        for name in REQUIRED_TYPES:
            relative = document["evidence"][name]["path"]
            archive.write((base / relative).resolve(), relative)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--bundle-output", type=Path)
    args = parser.parse_args()
    try:
        document = validate(args.manifest, args.version, args.source_commit, args.package)
        if args.bundle_output is not None:
            write_bundle(args.manifest, document, args.bundle_output)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"Production evidence approved for {args.version} at {args.source_commit}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
