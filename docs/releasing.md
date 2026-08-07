# Releasing Octopus

`make release vX.Y.Z` creates a signed and notarised candidate. GitHub marks it
as a prerelease, so it remains downloadable without replacing the latest
production release. Run installation, soak, concurrency, routing, critical and
rollback checks against that exact candidate before promotion.

Production promotion requires a local evidence bundle and the candidate
package already attached to the GitHub release:

```sh
RELEASE_EVIDENCE=/absolute/path/to/evidence/manifest.json \
make production-release v0.5.0
```

The release script downloads `Octopus-0.5.0.pkg` into a new temporary
directory, hashes it locally, and validates the evidence bundle before changing
the GitHub release. It never creates or fills evidence records itself.

## Evidence bundle schema

The schema-version-2 manifest is illustrated in
[`release-evidence.example.json`](release-evidence.example.json), with complete
record shapes in
[`release-evidence-records.example.json`](release-evidence-records.example.json).
Artefact paths
must be relative to the manifest and cannot escape its directory. Every
descriptor contains the expected artefact `type`, `path`, and lowercase
SHA-256. The validator hashes and parses each local JSON artefact.

The manifest binds the release to:

- the exact 40-character candidate tag commit;
- the exact candidate `.pkg` SHA-256;
- the candidate application binary SHA-256;
- the evaluated configuration and routing-policy SHA-256 values;
- a human reviewer and timezone-qualified approval timestamp.

The required typed artefacts are:

- `routing_gate`: passing machine-readable gate data for at least five runs,
  50 scenarios and 200 routed turns per run, bound to the same source, binary,
  configuration and policy digests;
- `critical_review`: a passed record with the reviewed artefact digest, human
  reviewer and timestamp;
- `soak_test`: start/end timestamps spanning at least 72 hours, request and
  router-error counts, observed error rate and availability, and their SLOs;
- `concurrency_test`: at least 50 parallel streams with zero session crossovers
  and zero failures;
- `installer_verification`: the exact package digest, accepted notarisation,
  validated staple, successful clean install and successful upgrade;
- `rollback_test`: passed rollback plus its artefact digest, reviewer and
  timestamp.

All records must carry the exact candidate source commit and `status: passed`.
Binary-bearing records must carry the manifest's candidate binary digest.

Validate the complete bundle offline:

```sh
./scripts/release.sh --check-evidence \
  v0.5.0 /path/evidence/manifest.json "$(git rev-parse v0.5.0^{commit})" \
  /path/Octopus-0.5.0.pkg
```

The promoted release retains the installer, checksum, Go module inventory and
CycloneDX 1.6 SBOM. It uploads both the reviewed manifest and a ZIP containing
that manifest plus the six digest-verified JSON records, so the release evidence
remains independently inspectable. The installer build requires
`cyclonedx-gomod` v1.10.0 or a deliberately reviewed compatible version on
`PATH`.
