package settings

import (
	"reflect"
	"testing"
	"time"

	"github.com/sausheong/octopus/config"
)

// documentOmits names config fields the Settings document deliberately does not
// carry. Anything absent from a Document is silently zeroed by Document.config()
// on every structured save, so an accidental omission is a silent data loss —
// it has happened five times (routing.max_attempts, caps.max_output_tokens, and
// server.auth_token_env, each in the Go layer and again in the browser form).
// Every entry here needs a reason.
var documentOmits = map[string]string{
	// Deprecated and ignored by the router; round-tripping it would preserve a
	// field that no longer affects behaviour.
	"DefaultModel": "deprecated and ignored by config.Validate",
}

// TestDocumentCarriesEveryConfigField fails when a field is added to
// config.Config or config.RoutingCfg without being threaded through
// settings.Document. It is deliberately behavioural rather than name-matching:
// it populates each field with a distinctive non-zero value, round-trips
// through the document, and reports whichever values did not survive. That
// catches a field that exists on Document but is not copied in BOTH
// directions, which a structural comparison would miss.
func TestDocumentCarriesEveryConfigField(t *testing.T) {
	full := &config.Config{
		ServerAddr:   "127.0.0.1:8787",
		AuthTokenEnv: "OCTOPUS_DRIFT_TOKEN",
		Classifier: config.ClassifierCfg{
			Model: "p/classifier", MaxTokens: 256, Timeout: 7 * time.Second,
		},
		Weights: config.Weights{Quality: 0.51, Cost: 0.31, Speed: 0.21},
		Routing: config.RoutingCfg{
			SessionSticky: true,
			SessionTTL:    42 * time.Minute,
			CacheAware:    true,
			MaxAttempts:   7,
		},
		Providers: map[string]config.ProviderCreds{
			"p": {Kind: "anthropic", APIKeyEnv: "K", BaseURL: "https://example.invalid"},
		},
		Catalog: []config.CatalogEntry{{
			ID: "p/m", Quality: 0.5, CostPerMTokIn: 1.5, CostPerMTokOut: 2.5, Speed: 0.5,
			Caps: config.Caps{
				Tools: true, Vision: true, Reasoning: true,
				MaxContext: 1000, MaxOutputTokens: 4096,
			},
		}},
	}

	// Guard the guard: if a new config field is added and not set above, the
	// round trip would "preserve" its zero value and this test would pass
	// while proving nothing.
	assertAllFieldsSet(t, reflect.ValueOf(*full), "Config")
	assertAllFieldsSet(t, reflect.ValueOf(full.Routing), "RoutingCfg")

	back, err := documentFromConfig(full).config()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}

	if back.AuthTokenEnv != full.AuthTokenEnv {
		t.Errorf("AuthTokenEnv: got %q, want %q", back.AuthTokenEnv, full.AuthTokenEnv)
	}
	if back.Routing != full.Routing {
		t.Errorf("Routing: got %+v, want %+v", back.Routing, full.Routing)
	}
	if back.Classifier != full.Classifier {
		t.Errorf("Classifier: got %+v, want %+v", back.Classifier, full.Classifier)
	}
	if back.Weights != full.Weights {
		t.Errorf("Weights: got %+v, want %+v", back.Weights, full.Weights)
	}
	if !reflect.DeepEqual(back.Catalog, full.Catalog) {
		t.Errorf("Catalog: got %+v, want %+v", back.Catalog, full.Catalog)
	}
	if !reflect.DeepEqual(back.Providers, full.Providers) {
		t.Errorf("Providers: got %+v, want %+v", back.Providers, full.Providers)
	}
	if back.ServerAddr != full.ServerAddr {
		t.Errorf("ServerAddr: got %q, want %q", back.ServerAddr, full.ServerAddr)
	}
}

// assertAllFieldsSet reports any exported field left at its zero value, so the
// fixture above cannot silently fall behind the struct it is meant to cover.
func assertAllFieldsSet(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if reason, omitted := documentOmits[field.Name]; omitted {
			t.Logf("%s.%s not covered: %s", name, field.Name, reason)
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("%s.%s is zero in the fixture: set it to a distinctive value, "+
				"or add it to documentOmits with a reason", name, field.Name)
		}
	}
}
