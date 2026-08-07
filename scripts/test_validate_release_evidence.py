import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import zipfile


SCRIPT_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("release_evidence", SCRIPT_DIR / "validate-release-evidence.py")
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ReleaseEvidenceTest(unittest.TestCase):
    version = "v9.8.7"
    commit = "a" * 40
    binary = "b" * 64

    def bundle(self, root: Path):
        package = root / "Octopus-9.8.7.pkg"
        package.write_bytes(b"signed-notarized-candidate")
        package_hash = sha256(package)
        records = {
            "routing": {
                "schema_version": 1, "type": "routing_gate", "status": "passed", "source_commit": self.commit,
                "routing_gate_passed": True, "binary_sha256": self.binary,
                "config_sha256": "c" * 64, "policy_sha256": "d" * 64,
                "distinct_runs": 5, "scenarios_per_run": 50, "routed_turns_per_run": 200,
            },
            "critical": {
                "schema_version": 1, "type": "critical_review", "status": "passed", "source_commit": self.commit,
                "artifact_sha256": "e" * 64, "reviewed_by": "Critical Reviewer", "reviewed_at": "2026-08-07T12:00:00+08:00",
            },
            "soak": {
                "schema_version": 1, "type": "soak_test", "status": "passed", "source_commit": self.commit,
                "binary_sha256": self.binary, "started_at": "2026-08-01T00:00:00Z", "ended_at": "2026-08-04T00:00:00Z",
                "requests": 1000, "router_errors": 0, "error_rate": 0.0, "error_rate_slo_max": 0.001,
                "availability": 1.0, "availability_slo_min": 0.999,
            },
            "concurrency": {
                "schema_version": 1, "type": "concurrency_test", "status": "passed", "source_commit": self.commit,
                "binary_sha256": self.binary, "parallel_streams": 50, "session_crossovers": 0, "failures": 0,
            },
            "install": {
                "schema_version": 1, "type": "installer_verification", "status": "passed", "source_commit": self.commit,
                "package_sha256": package_hash, "notarization_status": "accepted", "staple_validated": True,
                "install_status": "passed", "upgrade_status": "passed",
            },
            "rollback": {
                "schema_version": 1, "type": "rollback_test", "status": "passed", "source_commit": self.commit,
                "artifact_sha256": "f" * 64, "rollback_status": "passed", "reviewed_by": "Rollback Reviewer",
                "reviewed_at": "2026-08-07T13:00:00+08:00",
            },
        }
        descriptors = {}
        for name, record in records.items():
            artifact = root / f"{name}.json"
            artifact.write_text(json.dumps(record, sort_keys=True), encoding="utf-8")
            descriptors[name] = {"type": MODULE.REQUIRED_TYPES[name], "path": artifact.name, "sha256": sha256(artifact)}
        manifest = root / "manifest.json"
        manifest.write_text(json.dumps({
            "schema_version": 2, "release": self.version, "source_commit": self.commit,
            "candidate_pkg_sha256": package_hash, "decision": "approved_for_production",
            "candidate_binary_sha256": self.binary, "config_sha256": "c" * 64, "policy_sha256": "d" * 64,
            "reviewed_by": "Release Reviewer", "reviewed_at": "2026-08-07T14:00:00+08:00",
            "evidence": descriptors,
        }), encoding="utf-8")
        return manifest, package, records

    def test_accepts_complete_digest_bound_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest, package, _ = self.bundle(root)
            loaded = MODULE.validate(manifest, self.version, self.commit, package)
            self.assertEqual(loaded["candidate_pkg_sha256"], sha256(package))
            archive = root / "verified.zip"
            MODULE.write_bundle(manifest, loaded, archive)
            with zipfile.ZipFile(archive) as value:
                self.assertEqual(set(value.namelist()), {"manifest.json", *(f"{name}.json" for name in MODULE.REQUIRED_TYPES)})

    def test_rejects_tampered_artifact_and_package(self):
        for target in ("routing", "package"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                manifest, package, _ = self.bundle(root)
                if target == "routing":
                    (root / "routing.json").write_text("{}", encoding="utf-8")
                else:
                    package.write_bytes(b"different package")
                with self.assertRaises(ValueError):
                    MODULE.validate(manifest, self.version, self.commit, package)

    def test_rejects_short_soak_concurrency_crossover_and_failed_install(self):
        mutations = {
            "short_soak": ("soak", "ended_at", "2026-08-03T23:59:59Z"),
            "crossover": ("concurrency", "session_crossovers", 1),
            "install": ("install", "notarization_status", "pending"),
        }
        for name, (record_name, field, value) in mutations.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                manifest, package, records = self.bundle(root)
                records[record_name][field] = value
                artifact = root / f"{record_name}.json"
                artifact.write_text(json.dumps(records[record_name], sort_keys=True), encoding="utf-8")
                document = json.loads(manifest.read_text(encoding="utf-8"))
                document["evidence"][record_name]["sha256"] = sha256(artifact)
                manifest.write_text(json.dumps(document), encoding="utf-8")
                with self.assertRaises(ValueError):
                    MODULE.validate(manifest, self.version, self.commit, package)

    def test_release_shell_exposes_offline_evidence_check(self):
        with tempfile.TemporaryDirectory() as directory:
            manifest, package, _ = self.bundle(Path(directory))
            result = subprocess.run(
                [str(SCRIPT_DIR / "release.sh"), "--check-evidence", self.version, str(manifest), self.commit, str(package)],
                cwd=SCRIPT_DIR.parent, text=True, capture_output=True, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Production evidence approved", result.stdout)


if __name__ == "__main__":
    unittest.main()
