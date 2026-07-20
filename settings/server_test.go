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
}

func TestSettingsHandlerRejectsCrossOriginWrite(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "config.yaml"))
	server := NewServer(store, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/yaml", strings.NewReader(`{"yaml":"x"}`))
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
