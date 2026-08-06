package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sausheong/octopus/config"
	"github.com/sausheong/octopus/desktop"
	"github.com/sausheong/octopus/insights"
)

func TestSettingsHandlerCreatesConfigFromStructuredForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	store := NewStore(path)
	reloaded := false
	server := NewServer(store, func(_ context.Context) error { reloaded = true; return nil }, func() desktop.RouterStatus {
		return desktop.RouterStatus{Running: true, ConfigValid: true, ConfigPath: path}
	})
	doc := defaultDocument()
	body, _ := json.Marshal(doc)
	req := httptest.NewRequest(http.MethodPost, "/api/structured", bytes.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	req.Header.Set("X-Octopus-CSRF", server.csrf)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reloaded {
		t.Fatalf("status=%d reloaded=%v body=%s", rec.Code, reloaded, rec.Body.String())
	}
	if _, _, exists, err := store.Load(); err != nil || !exists {
		t.Fatalf("saved config exists=%v err=%v", exists, err)
	}
}

// A structured save must round-trip every routing and capability field the
// browser sends. Both fields below silently collapse to a default if dropped:
// max_attempts is re-defaulted to 3 by Validate, and max_output_tokens to 0
// (unconstrained), so neither loss surfaces as an error.
func TestSettingsStructuredSavePreservesAttemptsAndOutputCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path)
	server := NewServer(store, func(_ context.Context) error { return nil }, nil)
	doc := defaultDocument()
	doc.Routing.MaxAttempts = 7
	doc.Routing.DefaultRemainingTurns = 8
	doc.Routing.MinSwitchSavingsUSD = 0.025
	doc.Routing.MinSwitchSavingsPct = 0.2
	doc.Routing.SwitchConfidence = 0.75
	doc.Catalog[0].Caps.MaxOutputTokens = 8192
	doc.Catalog[0].TurnEfficiency = config.TurnEfficiency{Trivial: 0.8, Low: 0.9, Medium: 1.1, High: 1.3}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/structured", bytes.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	req.Header.Set("X-Octopus-CSRF", server.csrf)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	saved, _, exists, err := store.Load()
	if err != nil || !exists {
		t.Fatalf("reload saved config exists=%v err=%v", exists, err)
	}
	if saved.Routing.MaxAttempts != 7 {
		t.Errorf("routing.max_attempts = %d after save, want 7", saved.Routing.MaxAttempts)
	}
	if saved.Catalog[0].Caps.MaxOutputTokens != 8192 {
		t.Errorf("caps.max_output_tokens = %d after save, want 8192", saved.Catalog[0].Caps.MaxOutputTokens)
	}
	if saved.Routing.DefaultRemainingTurns != 8 || saved.Routing.MinSwitchSavingsUSD != 0.025 ||
		saved.Routing.MinSwitchSavingsPct != 0.2 || saved.Routing.SwitchConfidence != 0.75 {
		t.Errorf("amortized routing fields were not preserved: %+v", saved.Routing)
	}
	if saved.Catalog[0].TurnEfficiency != doc.Catalog[0].TurnEfficiency {
		t.Errorf("turn efficiency = %+v, want %+v", saved.Catalog[0].TurnEfficiency, doc.Catalog[0].TurnEfficiency)
	}
}

func TestSettingsStateUsesBrowserFieldNames(t *testing.T) {
	server := NewServer(NewStore(filepath.Join(t.TempDir(), "config.yaml")), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["version"] != "dev" {
		t.Fatalf("version = %#v", state["version"])
	}
	document := state["document"].(map[string]any)
	weights := document["weights"].(map[string]any)
	if weights["quality"] != 0.5 || weights["cost"] != 0.3 || weights["speed"] != 0.2 {
		t.Fatalf("unexpected browser weights: %#v", weights)
	}
	catalog := document["catalog"].([]any)
	caps := catalog[0].(map[string]any)["caps"].(map[string]any)
	if caps["tools"] != true || caps["max_context"] != float64(200000) {
		t.Fatalf("unexpected browser capabilities: %#v", caps)
	}
	// The form reads these two keys by name to populate its inputs; a rename
	// would leave both fields blank on load and then zero them on save.
	if _, ok := caps["max_output_tokens"]; !ok {
		t.Errorf("caps is missing max_output_tokens: %#v", caps)
	}
	routing := document["routing"].(map[string]any)
	if routing["max_attempts"] != float64(3) {
		t.Errorf("routing.max_attempts = %#v, want 3", routing["max_attempts"])
	}
	for key := range map[string]bool{
		"strategy": true, "data_policy": true, "default_remaining_turns": true, "min_switch_savings_usd": true,
		"min_switch_savings_pct": true, "switch_confidence": true,
	} {
		if _, ok := routing[key]; !ok {
			t.Errorf("routing is missing %s: %#v", key, routing)
		}
	}
	if _, ok := catalog[0].(map[string]any)["turn_efficiency"]; !ok {
		t.Errorf("catalog is missing turn_efficiency: %#v", catalog[0])
	}
}

func TestBrowserRoundTripsAmortizedRoutingFields(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, field := range []string{
		"strategy", "data_policy", "location", "default_remaining_turns",
		"min_switch_savings_usd", "min_switch_savings_pct", "switch_confidence", "turn_efficiency",
	} {
		if !strings.Contains(source, field) {
			t.Errorf("app.js does not round-trip %s", field)
		}
	}
}

func TestSettingsHandlerRejectsCrossOriginWrite(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.yaml"))
	server := NewServer(store, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/yaml", strings.NewReader(`{"yaml":"x"}`))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	req.Header.Set("X-Octopus-CSRF", server.csrf)
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestSettingsRootUsesSecurityHeaders(t *testing.T) {
	server := NewServer(NewStore(filepath.Join(t.TempDir(), "config.yaml")), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Octopus Settings") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP")
	}
}

func TestSettingsServesOctopusLogo(t *testing.T) {
	server := NewServer(NewStore(filepath.Join(t.TempDir(), "config.yaml")), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/assets/octopus.png", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "image/png" {
		t.Fatalf("content type = %q", contentType)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("settings logo is not a PNG")
	}
}

func TestSettingsServesInsightsRange(t *testing.T) {
	server := NewServer(NewStore(filepath.Join(t.TempDir(), "config.yaml")), nil, nil, func(days int) insights.Report {
		return insights.Report{RangeDays: days, Summary: insights.Summary{Requests: 12}}
	})
	req := httptest.NewRequest(http.MethodGet, "/api/insights?days=7", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report insights.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.RangeDays != 7 || report.Summary.Requests != 12 {
		t.Fatalf("report = %+v", report)
	}
}

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
//
// Built from writeReq and varying only Host and Origin. Hand-rolling the
// headers here once let this test silently stop exercising the Host check: a
// later layer (the CSRF token) short-circuited validWriteRequest before
// loopbackHost ran, so the whole rebinding defence could be deleted with the
// suite still green. A request that satisfies every other gate is the only
// kind that can pin this one.
func TestRebindingWriteIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)
	// The rebound page sends a config that would otherwise save cleanly, so a
	// failure here means the write really would have landed, not that some
	// unrelated validation happened to catch it.
	req := writeReq(t, server, "/api/yaml", validYAMLBody)
	req.Host = "evil.example.com:54321"
	req.Header.Set("Origin", "http://evil.example.com:54321")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (rebinding must be rejected)", rec.Code)
	}
	if _, _, exists, _ := server.store.Load(); exists {
		t.Fatal("a rebound write persisted config")
	}
}

// Reads are deliberately not gated: the page must load before it can hold a
// token. That is only safe because the response carries no secret — see
// TestStateWithholdsInlineAPIKey, which is the other half of this decision.
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

// The header and content-type gates were entirely unpinned: both could be
// deleted from validWriteRequest with the suite still green. They are the layer
// that stops a plain HTML form post, which cannot set a custom header and
// cannot send application/json, so a browser that ignores CORS preflight for
// simple requests still cannot reach a write.
func TestWriteRequiresSettingsHeaderAndJSONContentType(t *testing.T) {
	for _, c := range []struct {
		name        string
		header      string
		contentType string
		want        int
	}{
		{"both present", "1", "application/json", http.StatusOK},
		{"missing X-Octopus-Settings", "", "application/json", http.StatusForbidden},
		{"wrong X-Octopus-Settings", "0", "application/json", http.StatusForbidden},
		{"missing content type", "1", "", http.StatusForbidden},
		// The shape a plain <form> can produce without any scripting.
		{"form content type", "1", "application/x-www-form-urlencoded", http.StatusForbidden},
		{"text content type", "1", "text/plain", http.StatusForbidden},
		{"json with charset", "1", "application/json; charset=utf-8", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A fresh server per case so an accepted write cannot change what a
			// later case observes.
			server := NewServer(NewStore(filepath.Join(t.TempDir(), "config.yaml")), nil, nil)
			req := writeReq(t, server, "/api/yaml", validYAMLBody)
			// Deleted rather than set to "": http.Header treats a present empty
			// value differently from an absent one, and absent is what a form
			// post actually sends.
			if c.header == "" {
				req.Header.Del("X-Octopus-Settings")
			} else {
				req.Header.Set("X-Octopus-Settings", c.header)
			}
			if c.contentType == "" {
				req.Header.Del("Content-Type")
			} else {
				req.Header.Set("Content-Type", c.contentType)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusForbidden {
				if _, _, exists, _ := server.store.Load(); exists {
					t.Error("a rejected write still persisted config")
				}
			}
		})
	}
}

// The spec's matrix lists localhost and [::1] as accepted, but only
// TestLoopbackHost covered them and that tests the predicate in isolation. A
// predicate can be correct while the handler never consults it, so drive a real
// write through the full handler for each accepted spelling of "this machine".
func TestWriteAcceptsEveryLoopbackHostSpelling(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		t.Run(host, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			server := NewServer(NewStore(path), nil, nil)
			req := writeReq(t, server, "/api/yaml", validYAMLBody)
			req.Host = host
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			// A 200 alone would also be returned if the handler short-circuited
			// before writing, so confirm the config actually landed.
			if _, _, exists, err := server.store.Load(); err != nil || !exists {
				t.Errorf("write from %s did not persist: exists=%v err=%v", host, exists, err)
			}
		})
	}
}

func TestServedHTMLCarriesToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := NewServer(NewStore(path), nil, nil)
	// strings.Contains(body, "") is always true, so without this the assertion
	// below would pass against any page whatsoever.
	if server.csrf == "" {
		t.Fatal("NewServer must generate a CSRF token; an empty token makes the containment check vacuous")
	}
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

// A Server whose token was never generated must reject writes rather than
// accept unauthenticated ones. ConstantTimeCompare returns 1 for two empty
// slices, so without an explicit guard the check fails open on exactly the
// state the token exists to rule out. Not reachable through NewServer today;
// pinned so that a second constructor cannot introduce it silently.
func TestEmptyTokenFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".octopus", "config.yaml")
	server := &Server{store: NewStore(path)}
	req := httptest.NewRequest(http.MethodPost, "/api/yaml", strings.NewReader(validYAMLBody))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (an ungenerated token must not authorise writes)", rec.Code)
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

// The browser form has no control for the auth token env name, so app.js echoes
// it back from the state payload. That only works if /api/state exposes the key
// and a structured save round-trips it; if either breaks, a save silently
// disables authentication with no error shown to the user.
func TestSettingsStructuredSavePreservesAuthTokenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "server:\n  addr: \"127.0.0.1:8787\"\n  auth_token_env: \"OCTOPUS_AUTH_TOKEN\"\n" +
		"weights:\n  quality: 1\nproviders:\n  p:\n    kind: anthropic\n    api_key_env: \"K\"\n" +
		"catalog:\n  - id: \"p/m\"\n    quality: 0.5\n    speed: 0.5\n    caps: { max_context: 1000 }\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	server := NewServer(store, func(_ context.Context) error { return nil }, nil)

	stateReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	stateRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(stateRec, stateReq)
	var state map[string]any
	if err := json.Unmarshal(stateRec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	document := state["document"].(map[string]any)
	if document["auth_token_env"] != "OCTOPUS_AUTH_TOKEN" {
		t.Fatalf("state document auth_token_env = %#v, want OCTOPUS_AUTH_TOKEN (app.js cannot echo what it is not sent)",
			document["auth_token_env"])
	}

	// Post the document straight back, as the form does after an unrelated edit.
	document["server_addr"] = "127.0.0.1:9999"
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/structured", bytes.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
	req.Header.Set("X-Octopus-CSRF", server.csrf)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "auth_token_env: OCTOPUS_AUTH_TOKEN") {
		t.Errorf("a form save stripped auth_token_env, disabling authentication; written config:\n%s", written)
	}
}

// The Go round-trip fix is necessary but not sufficient: app.js builds the POST
// body key by key, so a key it omits arrives as "" and is written back as "",
// disabling authentication. This is the third field in this codebase to hit
// that root cause (after routing.max_attempts and caps.max_output_tokens), so
// pin the echo-back explicitly — no Go test can otherwise reach it.
func TestBrowserFormEchoesAuthTokenEnv(t *testing.T) {
	data, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "auth_token_env: state.document.auth_token_env") {
		t.Error("app.js collectDocument() does not echo auth_token_env; a form save will silently disable authentication")
	}
}
