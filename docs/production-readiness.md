# Production readiness

Octopus is production-ready only when it reduces measured cost without crossing
the agreed quality, safety, reliability, and privacy boundaries. Passing unit
tests or showing a lower token bill is necessary, but is not sufficient.

The first supported production target is the signed, single-user macOS menu-bar
application bound to a loopback address. Remote, shared-host, and multi-tenant
operation are outside this release boundary.

## Release gates

These thresholds are fixed before policy tuning begins.

| Area | Required result |
|---|---|
| Mean quality | No more than 0.25 points below the measured all-Opus baseline |
| Quality uncertainty | Bootstrap 95% lower bound no worse than -0.50 |
| Critical tasks | No severe correctness, security, data-loss, or instruction-following failure |
| Savings | At least 30% against measured all-Opus cost, including classifier and retry spend |
| Cost accounting | Within 5% of provider-reported usage or invoice data |
| High-difficulty recall | At least 95% on the labelled evaluation set |
| Tier safety | No labelled high or critical task routed below its configured quality floor |
| Router reliability | Fewer than 0.1% router-induced failures during the soak |
| Isolation | No session, workflow, or cache-state crossover under 50 parallel streams |
| Stability | A 72-hour soak without a leak, deadlock, ledger corruption, or unexplained exit |

Results must include confidence intervals and scenario-level failures. Aggregate
savings cannot hide a critical regression.

## Evidence required

- A reproducible evaluation pinned to the exact Octopus commit and configuration.
- Paired all-Opus, all-Sonnet, fixed-tier, and Octopus runs.
- At least 50 scenarios and 200 multi-turn workload requests, with repeated runs.
- Objective graders where possible, plus blinded and randomised model judging.
- Provider-reported input, output, cache-write, and cache-read tokens.
- Classifier, failed-attempt, retry, and fallback cost and latency.
- Difficulty confusion matrix, model allocation, switch reasons, and cache outcomes.
- Full unit, race, vet, Linux, and macOS application build results.
- A signed and notarised release-candidate installation, upgrade, reload, and
  uninstall test on a clean macOS account.

## Rollout

1. **Shadow:** send every request to Opus while recording the decision Octopus
   would have made. Collect at least 500 representative turns.
2. **Low-risk canary:** enable routing only for verified trivial and low work.
3. **Medium canary:** admit Sonnet after the quality gates pass.
4. **Full quality policy:** enable high-tier escalation and amortised switching.
5. **Soak:** complete the concurrency and 72-hour stability gates.
6. **Release candidate:** sign, notarise, install, upgrade, reload, and roll back.
7. **Production:** release only when every gate above has recorded evidence.

Every stage must retain an immediate fixed-model rollback. The default emergency
policy is to route all eligible traffic to the highest-quality configured model.

## Explicit non-goals

The first production release does not claim that Octopus produces better raw
answers than Opus. Its claim is narrower: it meets a declared quality floor at a
lower measured cost for the evaluated workload. It also does not support public
network exposure, per-user quotas, tenant-isolated state, or a shared service.
