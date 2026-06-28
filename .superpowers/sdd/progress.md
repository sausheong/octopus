# llmrouter SDD Progress Ledger

Plan: docs/superpowers/plans/2026-06-27-llmrouter.md
Base: fresh repo (git init in Task 1)

Task 1: complete (commit 03c9f29, review clean — Minors: harness require pruned by go mod tidy [self-corrects on first harness import in Task 2]; dead os.Setenv in baseValid; nondeterministic error-msg order in Validate map loop)
Task 2: complete (commit 9118411, review clean — restored harness require v0.3.2; Minor: no empty-slice test case for LastUserTurn [behavior correct])
Task 3: complete (commit 70a4ac1, review clean — Minors: zero-cost model scores worst on cost axis [no real catalog entry hits it]; `max` shadows builtin; test comments mislabel filter as "penalty"; normalize/wsum guards untested)
Task 4: complete (commit 9e3d306, review clean — controller confirmed BaseURL field + ParseProviderModel no-slash contract; Minors: New default/gemini branches untested, malformed-id path untested, map-iteration error order nondeterministic)
Task 5: complete (commit 66a072f, review clean — tool_result ordering verified correct; Minors: silent-drop of non-base64/nil image source, unknown block types, malformed system/tool_result→""; coverage gaps on those edges)
Task 6: complete (commit 79d6fe4, review clean after 1 fix iteration — PLAN BUG FIXED: toolDone guard dropped `&& fullInput != "{}"` so every tool block emits exactly one input_json_delta [was failing TestEncodeTextThenTool]. Minors: empty-input Done emits zero deltas; channel-closed/Done-without-Start paths untested; hardcoded msg id)
Task 7: complete (commit ea8b7a9, review clean — errString reconciliation correct; Minors: hardcoded msg_router id [package-wide], multi-Done overwrite untested, empty-input/{}/empty-content coverage gaps)
Task 8: complete (commit d3de63e, controller inline review [tiny diff] — verbatim from brief, vet clean, TestMapErrorKinds passes; Minor: MapError uses type assertion not errors.As, so wrapped APIError unmatched [all call sites construct directly, OK])
Task 9: complete (commit e89fb5c, review clean — all 4 failure modes return DefaultProfile; Minors: EventError branch untested, partial-JSON edge untested, {} alone→zero profile not default [defensible])
Task 10: complete (commit 4a77077, review clean — all 4 named checks pass [no-turn skips classifier, cross-checks override post-classify, timeout ctx defer-cancelled, unresolved provider→DefaultProfile]; Minors: fallback/timeout-propagation untested, stale comment in router_test.go from brief)
Task 11: complete (commit ff67a86, review clean — all 5 named checks pass [routed bare model set before ChatStream, SSE headers+200 before encode, pre-stream errors→HTTP error no half-200, ctx flows to Route+ChatStream, seams minimal]; Minors: Resolve/ChatStream error paths untested, mid-stream EncodeSSE error only logged, writeErrorStatus string-concat)
Task 12: complete (commit 1106fca, review clean — main.go wiring verbatim, slog-only, os.Exit on fatal; build+vet+test all green, 44 tests across 5 packages; Minor: ListenAndServe treats ErrServerClosed as fatal [no graceful shutdown wired, unreachable])

=== ALL 12 TASKS COMPLETE ===

=== FINAL WHOLE-BRANCH REVIEW (verdict: With fixes) ===
Final review found 2 Important (spec-contract) + minors. Fixed in commit 6cecdba:
- FIX1: anthropicio.MapBackendError maps backend 429->429, 529/5xx->503, 400->400 (was all->502); MapError now uses errors.As. Tests: TestMapBackendError, TestHandlerBackendRateLimit(429), TestHandlerBackendOverloaded(503).
- FIX2: streaming message_delta now emits input_tokens + cache tokens from EventDone usage. Test: TestHandlerStreamingUsage asserts input_tokens:3.
- FIX3: unique per-response message id (crypto/rand) replacing hardcoded msg_router, in sse.go + nonstream.go (new id.go).
Verified by controller: go build ./... (exit 0), go vet ./... (exit 0), go test -count=1 ./... ALL PASS (now 48 tests across 5 pkgs).
DEFERRED to v1 backlog (conscious): SSE ping keepalives (spec mentioned; trusted-local OK without); decode silent-drop of unknown/non-base64 blocks; errors.As in MapError now done. Remaining minors cosmetic (max shadow, stale comments, map-iteration error order).
=== PROJECT COMPLETE ===

=== LIVE INTEGRATION: Claude Code wired through router (post-completion) ===
Found+fixed real integration bug via live test: Claude Code sends no-arg tools as input_schema {"type":"object"} with NO properties; Vertex-via-proxy backend rejects with "tools.N.custom.input_schema: Field required". Fix: anthropicio/decode.go normalizeToolSchema() injects {"type":"object","properties":{}} when missing (passthrough when present). Tests added: TestDecodeToolSchemaInjectsProperties, TestDecodeToolSchemaPreservesExistingProperties, TestNormalizeToolSchemaEmpty.
Verified live: `claude -p` (full default toolset) → ROUTER OK; multi-turn Read-tool task → correct answer, 3 turns all routed to same model (stickiness holds live), zero errors. Commit below.
