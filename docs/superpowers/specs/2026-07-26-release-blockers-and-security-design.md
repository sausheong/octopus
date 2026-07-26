# Release Blockers and Security Hardening

**Date:** 2026-07-26
**Status:** Approved
**Scope:** Sub-projects A and D of the post-review remediation work

## Problem

A code review of Octopus surfaced nineteen findings. The fallback-and-routing
milestone closed five of them. This spec covers two more groups, both blocking
a credible public release. Every item below was re-verified against the merged
tree at `e50b916`.

### Part A — release blockers

1. **No LICENSE file.** For a public repository with a signed installer and a
   release pipeline, the code is legally all-rights-reserved. Nobody can use it.
2. **No CI.** There is no `.github/` directory. Nothing guards a regression on a
   contributor's pull request, and the project already needed four rounds of
   review commits before this work began.
3. **Dead code**, four items, all confirmed present:
   - `cmd/router/` — an empty directory left by the rename to Octopus.
   - `anthropicio/decode.go:128` `decodeSystem` — no callers.
   - `router/scorer.go:58` `reqCost` — no callers;
     `reqCostWithInputMultiplier` superseded it.
   - `server/server.go:284` `errString` — unused in that package. The
     `anthropicio` and `openaiio` copies are used and stay.

### Part D — security hardening

4. **The settings server is vulnerable to DNS rebinding.** `validWriteRequest`
   (`settings/server.go:231-237`) compares the `Origin` header against
   `r.Host` — but under a rebinding attack both are attacker-controlled and
   self-consistent. A probe with `Host: evil.example.com:54321` and a matching
   `Origin` reached the handler and returned 422 on YAML validity, not 403.
   Because a successful write triggers a router reload, a malicious page could
   rewrite a provider `base_url` and exfiltrate every subsequent prompt and API
   key.
5. **The routing endpoints have no authentication.** `/v1/messages`,
   `/v1/chat/completions`, and `/v1/models` accept any request that reaches
   them. Loopback binding is the only control, so any local process — or any
   browser page that can reach loopback — can use the configured providers.

## Goals

- Make the repository legally usable and guard it with CI.
- Remove dead code the review identified.
- Close the rebinding path so a web page cannot rewrite the configuration.
- Offer authentication on the routing endpoints without breaking any existing
  installation.

## Non-goals

Deferred to later sub-projects, each with its own spec:

- **E (response-path performance):** asynchronous/batched Insights, ledger
  pruning, the redundant `SessionID` recomputation in `Observe`, and `/health`.
  `/health` belongs with E because it is an observability endpoint, not a
  security control.
- **B1 (harness parameters):** `TopP`, `TopK`, `StopSequences`, `ToolChoice` on
  `llm.ChatRequest` plus per-provider mapping, in the `harness` repository.
- **B2 (Octopus API fidelity):** decoding those parameters, the temperature
  pointer, the OpenAI `created` field, `message_start` usage, and
  `count_tokens`. Depends on B1.

## Design

Parts A and D share no code. A is mechanical and lands first because it
unblocks everything else; D carries the design content.

### 1. LICENSE

MIT, at the repository root, `Copyright (c) 2026 Sau Sheong Chang`. MIT matches
the prevailing norm for Go tooling and imposes nothing on users. Add a one-line
License section at the end of `README.md`.

### 2. Continuous integration

`.github/workflows/ci.yml`, triggered on `push` and `pull_request`:

```
go build ./...
go vet ./...
go test ./...
gofmt -l .          # fails the job when output is non-empty
```

Runs on `ubuntu-latest` with `actions/setup-go` pinned to Go 1.25.1 and module
caching enabled.

Two details that are easy to get wrong:

- **`GOWORK=off` is not needed.** `go.work` is gitignored, so a CI checkout has
  none. Set `GOFLAGS: -mod=readonly` instead, so a stray `go.mod` edit fails the
  job rather than being silently repaired.
- **The macOS application path is not compiled.** `cmd/octopus/main_darwin.go`
  and `menubar/` are cgo plus Objective-C. A Linux runner compiles the
  `!darwin` path only, which still covers all nine test-bearing packages. This
  is a deliberate trade — fast, free CI over a macOS runner that exists to
  compile a menu bar — and the workflow carries a comment saying so, so it is
  not mistaken for an oversight. `make app` still exercises that path locally
  before a release.

The dependency `github.com/sausheong/harness v0.3.4` is fetchable from
`proxy.golang.org` (verified: `@v/v0.3.4.info` returns HTTP 200), so a clean
runner with no module cache can build.

### 3. Dead-code removal

Delete the four items listed under Problem. No behaviour change: if any were
still referenced, `go build ./...` would fail, which is the verification.

### 4. Settings DNS-rebinding defence

Three layers, because no single one is sufficient.

**Layer 1 — the `Host` header must be literal loopback.** This is the primary
defence and the one that closes the demonstrated probe.

```go
// loopbackHost reports whether the Host header names the local machine by
// address. A rebinding attack arrives with an attacker-controlled hostname
// that resolves to 127.0.0.1, so comparing Origin against Host proves
// nothing — only the literal address does.
func loopbackHost(host string) bool
```

Accepts `127.0.0.1:PORT`, `[::1]:PORT`, and `localhost:PORT`. Rejects
everything else, including a bare hostname with no port and any name that
merely resolves to loopback. Use `net.SplitHostPort`, then `net.ParseIP` with
`IsLoopback`, with an explicit case for `localhost`. This mirrors the existing
validation in `config.Validate` for `server.addr`.

**Layer 2 — a per-process CSRF token.** `Server.Start` generates 32 bytes from
`crypto/rand`, hex-encoded. It is injected into the served HTML as a
`<meta name="octopus-csrf" content="...">` tag in `<head>` — a meta tag rather
than inline script because the existing Content-Security-Policy is
`script-src 'self'`. `app.js` reads it and sends it as `X-Octopus-CSRF` on
every write. The server compares with `subtle.ConstantTimeCompare`.

Because `index.html` is currently served verbatim from an embedded filesystem
(`settings/server.go:94`, `staticFiles.ReadFile`), the handler must render it —
the simplest correct approach is a single string replacement of a placeholder
token in the embedded HTML at serve time.

The load order makes this safe: `app.js` fetches `/api/state` at line 37,
before any write at line 343. Since reads are not gated, the page loads
normally, and by the time a write is possible the token is already in the DOM.
No bootstrap problem exists — but a token-less write must still fail closed
rather than being special-cased as "first request".

**Layer 3 — keep the existing checks.** `X-Octopus-Settings: 1` and the JSON
content-type requirement stay. They cost nothing and stop simple form posts.

All three apply to writes (`POST /api/structured`, `POST /api/yaml`). Reads are
not gated, because gating them would break the initial page load, which must
fetch state before it has a token.

This means a DNS-rebinding page can read `/api/state`, including any inline
`api_key`, which it could not otherwise obtain — it cannot read `config.yaml`.

**As implemented, this milestone tried to close that read and the fix was
withdrawn.** `/api/state` substituted a sentinel for every inline `api_key` and
withheld the Advanced YAML tab entirely while one was present. The owner's
ruling: Octopus is a local tool, and a settings editor that hides part of the
file cannot show the user what they are about to change — the YAML tab in
particular is worthless when blank. Confidentiality of a file the user owns,
against a browser-based attacker, does not justify making the editor lie about
its contents.

So reads are ungated and unredacted, and the boundary is writes alone: the
loopback `Host` check plus the CSRF token mean a rebinding page can read the
configuration but cannot change it or repoint a provider, which was the
escalation that motivated this section. The residual exposure — a hostile page
reading an inlined key — is accepted and documented in the README.

### 5. Optional routing-endpoint authentication

```yaml
server:
  addr: "127.0.0.1:8787"
  auth_token_env: "OCTOPUS_AUTH_TOKEN"   # optional; unset means no auth
```

`config.Config` gains `AuthTokenEnv string`, and `yamlConfig.Server` gains the
matching field. The token itself is never stored in the configuration file —
only the name of the environment variable holding it, following the existing
`ProviderCreds.APIKeyEnv` convention.

**Default off.** When `auth_token_env` is unset, behaviour is byte-identical to
today. This is non-negotiable: the README documents Claude Code pointing at
`http://127.0.0.1:8787`, and a signed installer is already in users' hands.
Requiring authentication by default would break every existing installation.

When set, `/v1/messages`, `/v1/chat/completions`, and `/v1/models` require a
match on **either** `x-api-key` or `Authorization: Bearer <token>` — Anthropic
and OpenAI clients each send their own, and Octopus serves both. Comparison
uses `subtle.ConstantTimeCompare`. A mismatch or absence returns 401 in the
endpoint's native error shape: Anthropic-shaped for `/v1/messages`,
OpenAI-shaped for the other two.

**An empty environment variable is treated as unset**, with a startup warning.
If a user names a variable that does not exist, enforcing `token == ""` would
accept every request while appearing secure. Failing open loudly is the honest
behaviour and matches how `ProviderCreds.Key()` already handles an unset
variable.

## Testing

All tests hermetic; no network. Use `httptest` and the existing
`registry.NewForTest` seam.

**Part A.** The build is the test for dead-code removal. Add one deliberate
check that the `gofmt -l` gate actually fails CI on a misformatted file — a
lint gate that never fires is theatre.

**`loopbackHost`.** Its own table test, including malformed input, because it
is the primary defence: `127.0.0.1:8787`, `[::1]:8787`, `localhost:8787`,
`evil.example.com:8787`, `127.0.0.1` (no port), `localhost.` (trailing dot),
`` (empty), `[::1]` (no port), `192.168.1.5:8787`, and `127.0.0.1:notaport`.

**Settings writes** — the attack cases, not the happy path:

| Scenario | Expected |
|---|---|
| `Host: evil.example.com`, matching `Origin`, valid CSRF | 403 (the demonstrated probe; currently 422) |
| `Host: 127.0.0.1:PORT`, no CSRF header | 403 |
| `Host: 127.0.0.1:PORT`, wrong CSRF | 403 |
| `Host: localhost:PORT`, valid CSRF | 200 |
| `Host: [::1]:PORT`, valid CSRF | 200 |
| Valid CSRF, missing `X-Octopus-Settings` | 403 |
| Valid CSRF, non-JSON `Content-Type` | 403 |
| `GET /api/state` with attacker `Host` | 200 — reads are not gated |
| `GET /api/state` with an inlined key, either channel | key present — the editor shows the whole file |
| Structured save leaving the key field as served | stored key preserved |
| Structured save with a new or an emptied key | key replaced, or cleared |
| Rename, name swap, delete-and-add in one save | every surviving key stays on its own row |

Plus one asserting the served HTML contains a non-placeholder token, since the
UI is broken if it does not.

**Routing authentication:**

| Scenario | Expected |
|---|---|
| `auth_token_env` unset, no credentials | 200 — the backward-compatibility guarantee |
| Token set, correct `x-api-key` | 200 |
| Token set, correct `Authorization: Bearer` | 200 |
| Token set, wrong token | 401, Anthropic shape on `/v1/messages` |
| Token set, wrong token | 401, OpenAI shape on `/v1/chat/completions` |
| Token set, no credentials | 401 |
| Token set, `GET /v1/models` without credentials | 401 |
| Env var named but empty | 200 — treated as unset, warning logged |

## Risks

- **A stale settings page fails writes.** The CSRF token is per-process, so a
  user who bookmarks the settings URL and returns after a restart gets a page
  whose token no longer matches, and writes fail with 403. The write handlers
  must return a distinguishable error message so the UI can tell the user to
  reopen Settings from the menu bar, rather than showing a bare permission
  failure.
- **Authentication is opt-in, so most users get none.** That is the deliberate
  cost of not breaking the installed base. The README must state plainly that
  the default posture relies on loopback binding alone.
- **CI does not compile the macOS path.** A break in `main_darwin.go` or
  `menubar/` reaches a release build without CI catching it. `make app` before
  a release is the compensating control.
- **`loopbackHost` parsing is the whole defence.** IPv6 literals, a missing
  port, and trailing dots are all easy to mishandle, and a false positive
  reopens the hole. Hence its dedicated malformed-input test table.

## Files affected

**Part A**
- `LICENSE` — new
- `.github/workflows/ci.yml` — new
- `README.md` — License section
- `cmd/router/` — remove (empty directory)
- `anthropicio/decode.go` — remove `decodeSystem`
- `router/scorer.go` — remove `reqCost`
- `server/server.go` — remove the unused `errString`

**Part D**
- `settings/server.go` — `loopbackHost`, CSRF generation and validation,
  HTML rendering with the token
- `settings/static/index.html` — CSRF meta tag placeholder
- `settings/static/app.js` — read the token, send `X-Octopus-CSRF` on writes
- `settings/server_test.go` — rebinding and CSRF tests
- `config/config.go` — `AuthTokenEnv`, validation
- `config/config_test.go` — parsing and validation tests
- `server/server.go` — authentication middleware on the three routes
- `server/server_test.go` — authentication tests
- `config.example.yaml`, `README.md` — document `auth_token_env` and the
  default security posture
