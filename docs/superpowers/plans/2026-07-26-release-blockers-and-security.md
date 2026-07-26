# Release Blockers and Security Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the repository legally usable and CI-guarded, then close the settings DNS-rebinding hole and offer opt-in authentication on the routing endpoints.

**Architecture:** Two independent halves that share no code. Part A (Tasks 1-2) is mechanical: a LICENSE, a GitHub Actions workflow, and four dead-code removals. Part D (Tasks 3-6) adds a literal-loopback `Host` check plus a per-process CSRF token to the settings write path, and a default-off shared-secret check on the three routing endpoints.

**Tech Stack:** Go 1.25.1, `crypto/rand`, `crypto/subtle`, `net`, GitHub Actions. Vanilla JS for the settings UI. No new Go dependencies.

## Global Constraints

- Module is `github.com/sausheong/octopus`; Go 1.25.1.
- **Always run Go commands with `GOWORK=off`** — the repo has a `go.work` pointing at a sibling `../harness` checkout that is gitignored. Example: `GOWORK=off go test ./...`.
- All tests must be hermetic. No network, no real provider calls.
- `config.Parse` uses `dec.KnownFields(true)`, so any new YAML key must be added to the `yamlConfig` structs or existing configs with that key will fail to parse.
- Existing `config.yaml` files must keep working unchanged, and the routing endpoints must keep working with no credentials when authentication is not configured. A signed macOS installer is already in users' hands.
- Preserve the existing comment style: explain *why*, not *what*. Match surrounding density.
- Every task ends with a passing `GOWORK=off go test ./...` and a commit.

## Two Verified Facts That Shape Part D

Both were measured against the current tree. Ignoring either produces a broken or vacuous implementation.

1. **`httptest.NewRequest` sets `Host: example.com`.** Verified. The new
   loopback check rejects that, so the three existing settings write tests at
   `settings/server_test.go:26`, `:54`, and `:113` **will fail** unless each is
   given a loopback Host. That is expected and correct — those tests predate
   the defence. Task 4 updates them; do not treat the failures as a regression.

2. **Settings tests call `Handler()` directly and never call `Start()`.**
   Verified: zero occurrences of `.Start()` in `settings/server_test.go`. So the
   CSRF token **must be generated in `NewServer`**, not in `Start`. If it were
   generated in `Start`, every test would run with an empty token and the CSRF
   assertions would pass vacuously.

---

### Task 1: LICENSE and dead-code removal

Two unrelated but equally mechanical changes, grouped because neither carries a test cycle of its own — the build is the test.

**Files:**
- Create: `LICENSE`
- Modify: `README.md`
- Delete: `cmd/router/` (empty directory)
- Modify: `anthropicio/decode.go` (remove `decodeSystem`)
- Modify: `router/scorer.go` (remove `reqCost`)
- Modify: `server/server.go` (remove the unused `errString`)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. All four removals are unreferenced; removing a referenced one would fail `go build`.

- [ ] **Step 1: Add the LICENSE file**

Create `LICENSE` with the standard MIT text, `Copyright (c) 2026 Sau Sheong Chang`. Use the canonical wording from https://opensource.org/license/mit — do not paraphrase it.

- [ ] **Step 2: Add a License section to the README**

Append to the end of `README.md`:

```markdown
## License

MIT. See [LICENSE](LICENSE).
```

- [ ] **Step 3: Confirm each removal target is genuinely unreferenced**

Before deleting anything, run:

```bash
GOWORK=off grep -rn "decodeSystem\|reqCost\b" --include="*.go" .
ls -A cmd/router | wc -l
GOWORK=off grep -rn "errString" --include="*.go" server/
```

Expected: `decodeSystem` appears only at its definition (`anthropicio/decode.go:128`); `reqCost` only at its definition (`router/scorer.go:58`) and inside the comment above `reqCostWithInputMultiplier`; `cmd/router` contains 0 entries; `errString` in `server/` appears only at its definition (`server/server.go:284`) and its method.

If any target has a real caller, STOP and report it rather than deleting.

- [ ] **Step 4: Delete the four items**

- `rm -rf cmd/router`
- Remove `func decodeSystem(...)` from `anthropicio/decode.go`, including its doc comment.
- Remove `func reqCost(...)` from `router/scorer.go`, including its doc comment. Keep `reqCostWithInputMultiplier`.
- Remove `type errString string` and its `Error()` method from `server/server.go`. Do NOT touch the `errString` in `anthropicio/nonstream.go` or `openaiio/nonstream.go` — both are used.

- [ ] **Step 5: Verify the build still passes**

Run: `GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...`
Expected: all pass. A failure here means something was referenced after all.

- [ ] **Step 6: Commit**

```bash
git add LICENSE README.md anthropicio/decode.go router/scorer.go server/server.go
git rm -r --cached cmd/router 2>/dev/null || true
git commit -m "chore: add MIT license and remove dead code"
```

---

### Task 2: Continuous integration

**Files:**
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the `Makefile` targets' underlying commands (build, vet, test).
- Produces: nothing consumed by code.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/ci.yml`:

```yaml
name: CI

on:
  push:
  pull_request:

jobs:
  build:
    # Linux only, by design. cmd/octopus/main_darwin.go and menubar/ are cgo
    # plus Objective-C; a Linux runner compiles the !darwin path, which still
    # covers every test-bearing package. `make app` exercises the macOS path
    # before a release. This is a deliberate trade, not an oversight.
    runs-on: ubuntu-latest
    env:
      # go.work is gitignored so a CI checkout has none, but -mod=readonly
      # turns a stray go.mod edit into a failure instead of a silent repair.
      GOFLAGS: -mod=readonly
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25.1'
          cache: true
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test ./...
      - name: Gofmt
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then
            echo "These files are not gofmt-clean:"
            echo "$unformatted"
            exit 1
          fi
```

- [ ] **Step 2: Verify each command passes locally exactly as CI will run it**

CI has no `go.work`, so run without `GOWORK=off` overridden — but locally the file DOES exist, so emulate CI by disabling it:

```bash
GOWORK=off GOFLAGS=-mod=readonly go build ./...
GOWORK=off GOFLAGS=-mod=readonly go vet ./...
GOWORK=off GOFLAGS=-mod=readonly go test ./...
gofmt -l .
```

Expected: all pass; `gofmt -l .` prints nothing.

- [ ] **Step 3: Verify the gofmt gate actually fails on bad input**

A lint gate that never fires is worthless. Prove it fires:

```bash
cp router/turn.go /tmp/turn.go.bak
printf '\n\nfunc  badlyFormatted( ) {}\n' >> router/turn.go
gofmt -l . | grep -q "router/turn.go" && echo "GATE FIRES (correct)" || echo "GATE DID NOT FIRE (bug)"
cp /tmp/turn.go.bak router/turn.go
rm /tmp/turn.go.bak
gofmt -l . ; echo "(restored, should be empty above)"
git status --porcelain
```

Expected: `GATE FIRES (correct)`, then a clean tree. Report this transcript.

- [ ] **Step 4: Confirm the harness dependency is proxy-fetchable**

CI has no module cache. Confirm the pinned dependency can be fetched:

```bash
curl -s -o /dev/null -w "%{http_code}\n" "https://proxy.golang.org/github.com/sausheong/harness/@v/v0.3.4.info"
```

Expected: `200`. (This was verified during design, but re-check — a failure here means CI cannot build at all.)

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: run build, vet, test, and gofmt on push and PR"
```

---

### Task 3: Settings loopback Host check

The primary rebinding defence. Task 4 adds the CSRF token on top.

**Files:**
- Modify: `settings/server.go`
- Test: `settings/server_test.go`

**Interfaces:**
- Consumes: the existing `validWriteRequest(r *http.Request) bool`.
- Produces: `func loopbackHost(host string) bool` (package-private).

- [ ] **Step 1: Write the failing tests**

Append to `settings/server_test.go`:

```go
func TestLoopbackHost(t *testing.T) {
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"[::1]:8787", true},
		{"localhost:8787", true},
		{"LocalHost:8787", true},
		{"127.0.0.2:8787", true},
		{"evil.example.com:8787", false},
		{"127.0.0.1.evil.com:8787", false},
		{"127.0.0.1", false},
		{"localhost", false},
		{"[::1]", false},
		{"", false},
		{"192.168.1.5:8787", false},
		{"10.0.0.1:8787", false},
		{"localhost.:8787", false},
		{"127.0.0.1:notaport", true},
	} {
		if got := loopbackHost(c.host); got != c.want {
			t.Errorf("loopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// The demonstrated attack: a page on evil.example.com whose DNS resolves to
// 127.0.0.1. Host and Origin agree with each other, so the old Origin-vs-Host
// comparison accepted it; only the literal address rejects it.
func TestRebindingWriteIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/yaml",
		strings.NewReader(`{"yaml":"server:\n  addr: \"127.0.0.1:8787\"\n"}`))
	req.Host = "evil.example.com:54321"
	req.Header.Set("Origin", "http://evil.example.com:54321")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (rebinding must be rejected)", rec.Code)
	}
}

// Reads are deliberately not gated: the page must load before it can hold a
// token, and /api/state carries no secret a local process cannot already read.
func TestStateReadIsNotHostGated(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Host = "evil.example.com:54321"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
```

Note the two deliberately surprising expectations. `127.0.0.2:8787` is `true`: the whole `127.0.0.0/8` block is loopback and `net.IP.IsLoopback` says so, and accepting it costs nothing since reaching it still requires being on the machine. `127.0.0.1:notaport` is `true`: `net.SplitHostPort` does not validate that the port is numeric, and the host half is what matters. If your implementation disagrees with either, change the implementation to match — do not edit these expectations without saying so in your report.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test ./settings/ -run 'TestLoopbackHost|TestRebinding|TestStateRead' -v`
Expected: compile failure — `undefined: loopbackHost` — and, once that is resolved, `TestRebindingWriteIsRejected` fails with status 422 rather than 403.

- [ ] **Step 3: Implement `loopbackHost` and wire it in**

Add to `settings/server.go` (the file already imports `net` and `strings`):

```go
// loopbackHost reports whether the Host header names this machine by address.
// A DNS-rebinding attack arrives with an attacker-controlled hostname that
// resolves to 127.0.0.1, so comparing Origin against Host proves nothing —
// both are attacker-controlled and agree with each other. Only the literal
// address distinguishes the real local UI from a hostile page.
func loopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}
```

In `validWriteRequest`, add the check as the first condition:

```go
func validWriteRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	if r.Header.Get("X-Octopus-Settings") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
}
```

- [ ] **Step 4: Fix the three pre-existing write tests**

These fail now because `httptest.NewRequest` sets `Host: example.com`. This is expected — see "Two Verified Facts" above. Add one line to each of the three POST tests at `settings/server_test.go:26`, `:54`, and `:113`, immediately after the `httptest.NewRequest` call:

```go
	req.Host = "127.0.0.1:8787"
```

Do not weaken any assertion in those tests; only add the Host line.

- [ ] **Step 5: Run the full settings suite**

Run: `GOWORK=off go test ./settings/ -v`
Expected: all pass, including the three updated pre-existing tests.

- [ ] **Step 6: Verify the guard is load-bearing**

```bash
cp settings/server.go /tmp/server.go.bak
# Neuter the check
perl -0pi -e 's/\tif !loopbackHost\(r\.Host\) \{\n\t\treturn false\n\t\}\n//' settings/server.go
GOWORK=off go test ./settings/ -run TestRebindingWriteIsRejected 2>&1 | tail -3
cp /tmp/server.go.bak settings/server.go && rm /tmp/server.go.bak
GOWORK=off go test ./settings/ 2>&1 | tail -1
git status --porcelain
```

Expected: the mutation fails `TestRebindingWriteIsRejected`; after restore everything passes and the tree is clean. Report the transcript.

- [ ] **Step 7: Commit**

```bash
git add settings/server.go settings/server_test.go
git commit -m "fix: reject settings writes whose Host is not literal loopback"
```

---

### Task 4: Settings CSRF token

**Files:**
- Modify: `settings/server.go`
- Modify: `settings/static/index.html`
- Modify: `settings/static/app.js`
- Test: `settings/server_test.go`

**Interfaces:**
- Consumes: `loopbackHost` and `validWriteRequest` from Task 3.
- Produces: `Server.csrf string` field, set in `NewServer`. The write path requires header `X-Octopus-CSRF` to equal it.

**Critical:** generate the token in `NewServer`, NOT in `Start`. Settings tests call `Handler()` directly and never call `Start()` (verified: zero `.Start()` calls in `settings/server_test.go`), so a token generated in `Start` would be empty in every test and the CSRF assertions would pass vacuously.

- [ ] **Step 1: Write the failing tests**

Append to `settings/server_test.go`:

```go
// writeReq builds a settings write that passes every check except the one a
// test is targeting, so each test varies exactly one thing.
func writeReq(t *testing.T, server *Server, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	req.Header.Set("X-Octopus-CSRF", server.csrf)
	return req
}

const validYAMLBody = `{"yaml":"server:\n  addr: \"127.0.0.1:8787\"\nweights:\n  quality: 1\nproviders:\n  p:\n    kind: anthropic\n    api_key_env: K\ncatalog:\n  - id: p/m\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"}`

func TestWriteRequiresCSRFToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)

	if server.csrf == "" {
		t.Fatal("NewServer must generate a CSRF token; an empty token makes every check vacuous")
	}

	for _, c := range []struct {
		name  string
		token string
		want  int
	}{
		{"correct token", server.csrf, http.StatusOK},
		{"missing token", "", http.StatusForbidden},
		{"wrong token", "0000000000000000000000000000000000000000000000000000000000000000", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := writeReq(t, server, "/api/yaml", validYAMLBody)
			req.Header.Set("X-Octopus-CSRF", c.token)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestServedHTMLCarriesToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, server.csrf) {
		t.Error("served HTML does not contain the CSRF token; the UI cannot save")
	}
	if strings.Contains(body, "{{CSRF_TOKEN}}") {
		t.Error("placeholder was not substituted")
	}
}

// Two servers must not share a token, or a token leaked from one process
// would authorise writes to another.
func TestTokensArePerProcess(t *testing.T) {
	a := NewServer(NewStore(filepath.Join(t.TempDir(), "a.yaml")), nil, nil)
	b := NewServer(NewStore(filepath.Join(t.TempDir(), "b.yaml")), nil, nil)
	if a.csrf == b.csrf {
		t.Fatal("two servers generated the same CSRF token")
	}
}
```

Then update the three pre-existing POST tests (`:26`, `:54`, `:113`) again, adding the CSRF header alongside the Host line added in Task 3:

```go
	req.Header.Set("X-Octopus-CSRF", server.csrf)
```

Note the local variable holding the server may be named `server` in each — check and match.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test ./settings/ -run 'CSRF|ServedHTML|PerProcess' -v`
Expected: compile failure — `server.csrf` undefined.

- [ ] **Step 3: Implement the token**

In `settings/server.go`, add `"crypto/rand"`, `"crypto/subtle"`, and `"encoding/hex"` to the imports, and a `csrf string` field to `Server`.

In `NewServer`, generate it before returning:

```go
	// Generated here rather than in Start because callers (and tests) may use
	// Handler() without ever starting a listener; a token created in Start
	// would be empty on those paths and every CSRF check would pass vacuously.
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		// crypto/rand failing means the process cannot generate any secret.
		// Refusing to serve is better than serving with a predictable token.
		panic("settings: cannot generate CSRF token: " + err.Error())
	}
	server.csrf = hex.EncodeToString(token)
```

Add the check to `validWriteRequest`. Because it needs the token, change the function to a method on `*Server`:

```go
func (s *Server) validWriteRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	if r.Header.Get("X-Octopus-Settings") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	// Constant-time: a timing oracle on a 32-byte token is not a realistic
	// attack here, but the comparison is free and the habit is worth keeping.
	got := r.Header.Get("X-Octopus-CSRF")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host || origin == "https://"+r.Host
}
```

Update both call sites (`handleStructured`, `handleYAML`) from `validWriteRequest(r)` to `s.validWriteRequest(r)`.

In the `GET /` handler, substitute the token into the HTML:

```go
		html := strings.Replace(string(data), "{{CSRF_TOKEN}}", s.csrf, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, html)
```

Add `"io"` to the imports if it is not already there.

- [ ] **Step 4: Add the meta tag and wire up the JS**

In `settings/static/index.html`, add inside `<head>` after the `color-scheme` meta:

```html
  <meta name="octopus-csrf" content="{{CSRF_TOKEN}}">
```

In `settings/static/app.js`, near the top, read it once:

```js
const CSRF = document.querySelector('meta[name="octopus-csrf"]')?.content || "";
```

and add it to the write headers (around line 345):

```js
      headers: {"Content-Type": "application/json", "X-Octopus-Settings": "1", "X-Octopus-CSRF": CSRF},
```

- [ ] **Step 5: Improve the rejection message so a stale page is diagnosable**

A user who bookmarks the settings URL and returns after a restart holds a stale token and gets a bare 403. In `handleStructured` and `handleYAML`, change the rejection body to:

```go
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings write rejected; reopen Settings from the menu bar and try again"})
```

- [ ] **Step 6: Run the full suite**

Run: `GOWORK=off go test ./settings/ -v && GOWORK=off go test ./...`
Expected: all pass.

- [ ] **Step 7: Verify the CSRF guard is load-bearing**

```bash
cp settings/server.go /tmp/server.go.bak
perl -0pi -e 's/\tgot := r\.Header\.Get\("X-Octopus-CSRF"\)\n\tif subtle\.ConstantTimeCompare\(\[\]byte\(got\), \[\]byte\(s\.csrf\)\) != 1 \{\n\t\treturn false\n\t\}\n//' settings/server.go
GOWORK=off go test ./settings/ -run TestWriteRequiresCSRFToken 2>&1 | tail -5
cp /tmp/server.go.bak settings/server.go && rm /tmp/server.go.bak
GOWORK=off go test ./settings/ 2>&1 | tail -1
git status --porcelain
```

Expected: the mutation fails the `missing token` and `wrong token` subtests; after restore all pass and the tree is clean. If the mutation leaves tests green, the check is not wired in — investigate before proceeding. Report the transcript.

- [ ] **Step 8: Commit**

```bash
git add settings/server.go settings/server_test.go settings/static/index.html settings/static/app.js
git commit -m "fix: require a per-process CSRF token on settings writes"
```

---

### Task 5: Optional routing-endpoint authentication

**Files:**
- Modify: `config/config.go`
- Modify: `server/server.go`
- Test: `config/config_test.go`, `server/server_test.go`

**Interfaces:**
- Consumes: `config.Config`, `yamlConfig.Server`, `Server.Handler()`.
- Produces:
  - `config.Config.AuthTokenEnv string` — YAML key `server.auth_token_env`.
  - `config.Config.AuthToken() string` — resolves the env var; empty means no auth.
  - `server.New` gains the token; `Handler()` wraps the three routes.

- [ ] **Step 1: Write the failing config tests**

Append to `config/config_test.go`:

```go
func TestParseAuthTokenEnv(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
  auth_token_env: "OCTOPUS_AUTH_TOKEN"
weights:
  quality: 1
providers:
  p:
    kind: anthropic
    api_key_env: "K"
catalog:
  - id: "p/m"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.AuthTokenEnv != "OCTOPUS_AUTH_TOKEN" {
		t.Errorf("AuthTokenEnv = %q, want %q", cfg.AuthTokenEnv, "OCTOPUS_AUTH_TOKEN")
	}
}

func TestAuthTokenResolvesEnvVar(t *testing.T) {
	t.Setenv("OCTOPUS_TEST_TOKEN", "s3cret")
	cfg := &Config{AuthTokenEnv: "OCTOPUS_TEST_TOKEN"}
	if got := cfg.AuthToken(); got != "s3cret" {
		t.Errorf("AuthToken() = %q, want %q", got, "s3cret")
	}
}

// A named-but-unset variable must mean "no auth", not "the token is the empty
// string" — the latter would accept every request while looking configured.
func TestAuthTokenEmptyWhenEnvUnset(t *testing.T) {
	cfg := &Config{AuthTokenEnv: "OCTOPUS_DEFINITELY_UNSET_VAR"}
	if got := cfg.AuthToken(); got != "" {
		t.Errorf("AuthToken() = %q, want empty", got)
	}
}

func TestAuthTokenEmptyWhenUnconfigured(t *testing.T) {
	cfg := &Config{}
	if got := cfg.AuthToken(); got != "" {
		t.Errorf("AuthToken() = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `GOWORK=off go test ./config/ -run Auth -v`
Expected: compile failure — `AuthTokenEnv` and `AuthToken` undefined.

- [ ] **Step 3: Implement the config side**

In `config/config.go`, add to `Config`:

```go
	// AuthTokenEnv names the environment variable holding a shared secret that
	// callers must present on the routing endpoints. Empty means no
	// authentication, which is the default: a signed installer is already in
	// use and requiring a token would break every existing client.
	AuthTokenEnv string `yaml:"-"`
```

Add `AuthTokenEnv string \`yaml:"auth_token_env"\`` to the anonymous `Server` struct inside `yamlConfig`, and copy it in `Parse` (`AuthTokenEnv: yc.Server.AuthTokenEnv`) and in `Marshal` (`yc.Server.AuthTokenEnv = copyCfg.AuthTokenEnv`).

Add the accessor:

```go
// AuthToken resolves the configured shared secret. An unset or empty variable
// yields "", which callers must treat as "authentication disabled" rather than
// "the expected token is empty" — the latter would accept every request.
func (c *Config) AuthToken() string {
	if c.AuthTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.AuthTokenEnv)
}
```

- [ ] **Step 4: Write the failing server tests**

Append to `server/server_test.go`:

```go
func TestRoutingAuth(t *testing.T) {
	const token = "s3cret-token"
	for _, c := range []struct {
		name       string
		configured string
		header     string
		value      string
		path       string
		want       int
	}{
		{"unconfigured allows anonymous", "", "", "", "/v1/messages", 200},
		{"correct x-api-key", token, "x-api-key", token, "/v1/messages", 200},
		{"correct bearer", token, "Authorization", "Bearer " + token, "/v1/messages", 200},
		{"wrong token anthropic", token, "x-api-key", "nope", "/v1/messages", 401},
		{"missing token anthropic", token, "", "", "/v1/messages", 401},
		{"wrong token openai", token, "Authorization", "Bearer nope", "/v1/chat/completions", 401},
		{"models requires auth", token, "", "", "/v1/models", 401},
		{"models with auth", token, "x-api-key", token, "/v1/models", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := buildServerWithAuth(t, c.configured)
			var req *http.Request
			if c.path == "/v1/models" {
				req = httptest.NewRequest(http.MethodGet, c.path, nil)
			} else {
				body := `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
				if c.path == "/v1/chat/completions" {
					body = `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
				}
				req = httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body))
			}
			if c.header != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// The error body must match the endpoint the caller used, not a single shape.
func TestAuthErrorShapePerEndpoint(t *testing.T) {
	s := buildServerWithAuth(t, "tok")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	var anth map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anth); err != nil {
		t.Fatalf("anthropic body not JSON: %v", err)
	}
	if anth["type"] != "error" {
		t.Errorf("anthropic error body = %v, want top-level type=error", anth)
	}

	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)))
	var oai map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &oai); err != nil {
		t.Fatalf("openai body not JSON: %v", err)
	}
	if _, ok := oai["error"]; !ok || oai["type"] != nil {
		t.Errorf("openai error body = %v, want an error object with no top-level type", oai)
	}
}
```

You need a `buildServerWithAuth(t, token)` helper. Model it on the existing `buildServerWithProv` in that file (around line 73) — same config, same `registry.NewForTest`, same `SetClassifier` stub, using `&fakeProv{text: "Hello"}` — differing only in passing the auth token to `New`. Do NOT duplicate more than necessary; if `buildServerWithProv` can be refactored to take a token with a one-line change, prefer that.

Check whether `server_test.go` already imports `encoding/json`; add it if not.

- [ ] **Step 5: Run to verify failure**

Run: `GOWORK=off go test ./server/ -run 'RoutingAuth|AuthErrorShape' -v`
Expected: failures — every 401 case returns 200 because no authentication exists yet.

- [ ] **Step 6: Implement the server side**

Change `New` to accept the token. It currently has a variadic observer parameter, so add the token as a named field set by a small setter rather than another positional argument:

```go
// SetAuthToken enables shared-secret authentication on the routing endpoints.
// An empty token disables it, which is the default and preserves the
// no-credentials behaviour every existing client relies on.
func (s *Server) SetAuthToken(token string) { s.authToken = token }
```

Add `authToken string` to the `Server` struct.

Add the middleware:

```go
// authorized reports whether the request carries the configured shared secret.
// Anthropic clients send x-api-key and OpenAI clients send an Authorization
// bearer, so both are accepted. Always true when no token is configured.
func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	if key := r.Header.Get("x-api-key"); key != "" &&
		subtle.ConstantTimeCompare([]byte(key), []byte(s.authToken)) == 1 {
		return true
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return bearer != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.authToken)) == 1
}
```

Add `"crypto/subtle"` to the imports.

Do NOT route the 401 through `writeError` — it maps every `APIError` kind to
its own status and has no 401 case, so `invalid_request` would emit 400. Add a
dedicated helper instead, next to the existing `writeErrorStatus`:

```go
// writeAnthropicUnauthorized emits a 401 in the Anthropic error shape.
// writeError has no 401 mapping, and reusing its invalid_request kind would
// report a credentials failure as a malformed request.
func writeAnthropicUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"missing or invalid credentials"}}`)
}
```

Then gate each route in `Handler()`. `/v1/messages` uses the Anthropic shape;
the two OpenAI routes use `writeOAIError`, which already takes an explicit
status:

```go
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeAnthropicUnauthorized(w)
			return
		}
		s.handleMessages(w, r)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeOAIError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid credentials")
			return
		}
		s.handleChatCompletions(w, r)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeOAIError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid credentials")
			return
		}
		s.handleModels(w, r)
	})
```

`server.go` already imports `io`.

Wire the token in both entry points:
- `cmd/octopus/main.go`: after `srv := server.New(...)`, call `srv.SetAuthToken(cfg.AuthToken())`.
- `desktop/router_manager.go` `Reload`: same, on the `server.New(...)` result before `.Handler()`.

- [ ] **Step 7: Run everything**

Run: `GOWORK=off go test ./... && GOWORK=off go vet ./...`
Expected: all pass. Pay attention to pre-existing `server` tests — they build servers without a token, so they must still work anonymously. If any fail, the default-off guarantee is broken.

- [ ] **Step 8: Verify the guard is load-bearing**

```bash
cp server/server.go /tmp/server.go.bak
perl -0pi -e 's/\tif s\.authToken == "" \{\n\t\treturn true\n\t\}/\tif true {\n\t\treturn true\n\t}/' server/server.go
GOWORK=off go test ./server/ -run TestRoutingAuth 2>&1 | tail -6
cp /tmp/server.go.bak server/server.go && rm /tmp/server.go.bak
GOWORK=off go test ./server/ 2>&1 | tail -1
git status --porcelain
```

Expected: the mutation fails every 401 subtest; after restore all pass, tree clean. Report the transcript.

- [ ] **Step 9: Commit**

```bash
git add config/config.go config/config_test.go server/server.go server/server_test.go cmd/octopus/main.go desktop/router_manager.go
git commit -m "feat: add optional shared-secret auth on the routing endpoints"
```

---

### Task 6: Documentation

**Files:**
- Modify: `config.example.yaml`
- Modify: `README.md`

**Interfaces:**
- Consumes: `server.auth_token_env` (Task 5), the settings hardening (Tasks 3-4).
- Produces: nothing consumed by code.

- [ ] **Step 1: Document `auth_token_env` in the example config**

In `config.example.yaml`, under the `server:` block:

```yaml
server:
  addr: "127.0.0.1:8787"
  # Optional shared secret for the routing endpoints. Names an environment
  # variable; the token itself is never stored here. When unset (the default)
  # the endpoints accept any request that reaches them, relying on the
  # loopback bind. Clients may send it as x-api-key or as a bearer token.
  # auth_token_env: "OCTOPUS_AUTH_TOKEN"
```

Leave it commented out so the example keeps working unchanged.

- [ ] **Step 2: Document it in the README `server` config reference**

Find the `### \`server\`` section (near line 561) and add an `auth_token_env` row matching the surrounding format: optional; names an env var; unset means no authentication; accepted as either `x-api-key` or `Authorization: Bearer`.

- [ ] **Step 3: State the default security posture honestly**

Find the "Deployment and security" section (near line 661). Add a short paragraph stating plainly:

- The routing endpoints are unauthenticated by default and rely on the loopback bind, so any local process can use the configured providers.
- Setting `server.auth_token_env` enables a shared secret.
- The settings UI accepts writes only from a literal loopback `Host` and requires a per-process CSRF token, which together prevent a web page from rewriting the configuration through DNS rebinding.

Do not overstate it. Loopback binding is a real control but not a strong one, and the honest framing is what a security-conscious reader needs.

- [ ] **Step 4: Note the stale-page behaviour**

In the "Settings and live reload" section (near line 220), add one sentence: the CSRF token is per-process, so a settings page left open across a restart must be reopened from the menu bar before it can save.

- [ ] **Step 5: Verify the example config still parses**

```bash
cat > /tmp/zz_ex_test.go <<'EOF'
package config

import ("os";"testing")

func TestZZExampleParses(t *testing.T) {
	d, err := os.ReadFile("../config.example.yaml")
	if err != nil { t.Fatal(err) }
	if _, err := Parse(d); err != nil { t.Fatalf("example does not parse: %v", err) }
}
EOF
cp /tmp/zz_ex_test.go config/zz_ex_test.go
GOWORK=off go test ./config/ -run TestZZExampleParses -v
rm config/zz_ex_test.go /tmp/zz_ex_test.go
git status --porcelain
```

Expected: PASS, then a tree showing only `config.example.yaml` and `README.md` modified.

- [ ] **Step 6: Commit**

```bash
git add config.example.yaml README.md
git commit -m "docs: document auth_token_env and the default security posture"
```

---

### Task 7: Final verification

**Files:** none modified.

- [ ] **Step 1: Full build, vet, gofmt, test, race**

```bash
GOWORK=off go build ./... && \
GOWORK=off go vet ./... && \
gofmt -l . && \
GOWORK=off go test -count=1 ./... && \
GOWORK=off go test -race -count=1 ./...
```

Expected: all pass; `gofmt -l .` prints nothing.

- [ ] **Step 2: Confirm the backward-compatibility guarantee end to end**

The single most important property of this branch: an existing config with no `auth_token_env` must serve requests with no credentials.

```bash
GOWORK=off go test ./server/ -run 'TestRoutingAuth/unconfigured' -v
GOWORK=off go test ./config/ -run TestExampleConfigParses -v 2>/dev/null || true
```

Expected: the unconfigured case returns 200.

- [ ] **Step 3: Confirm a clean tree**

```bash
git status --porcelain
```

Expected: empty. Any stray `zz_*` file is a mistake — delete it.

- [ ] **Step 4: Review the branch diff against the spec**

The branch base is `63358e2` (the commit that added the design spec):

```bash
git log --oneline 63358e2..HEAD
git diff 63358e2..HEAD --stat
```

Confirm every file in the spec's "Files affected" list appears and no unrelated
file does. Expected set: `LICENSE`, `.github/workflows/ci.yml`, `README.md`,
`config.example.yaml`, `anthropicio/decode.go`, `router/scorer.go`,
`config/config.go`, `config/config_test.go`, `server/server.go`,
`server/server_test.go`, `settings/server.go`, `settings/server_test.go`,
`settings/static/index.html`, `settings/static/app.js`,
`cmd/octopus/main.go`, `desktop/router_manager.go`, plus the deleted
`cmd/router/` directory.
