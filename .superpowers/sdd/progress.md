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
