package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	doc.Catalog[0].Caps.MaxOutputTokens = 8192
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/structured", bytes.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
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
}

func TestSettingsHandlerRejectsCrossOriginWrite(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.yaml"))
	server := NewServer(store, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/yaml", strings.NewReader(`{"yaml":"x"}`))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Octopus-Settings", "1")
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
