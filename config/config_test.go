package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadValid(t *testing.T) {
	c, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ServerAddr != "127.0.0.1:8787" {
		t.Errorf("ServerAddr = %q", c.ServerAddr)
	}
	if c.Classifier.Model != "anthropic/claude-haiku-3-5-20241022" {
		t.Errorf("Classifier.Model = %q", c.Classifier.Model)
	}
	if c.Classifier.Timeout != 10*time.Second {
		t.Errorf("Classifier.Timeout = %v", c.Classifier.Timeout)
	}
	if len(c.Catalog) != 2 {
		t.Fatalf("len(Catalog) = %d, want 2", len(c.Catalog))
	}
	if c.Catalog[0].ID != "anthropic/claude-opus-4-0-20250514" {
		t.Errorf("Catalog[0].ID = %q", c.Catalog[0].ID)
	}
	if !c.Catalog[0].Caps.Vision {
		t.Errorf("Catalog[0] vision should be true")
	}
}

func TestValidateMissingCredsForCatalogProvider(t *testing.T) {
	c := &Config{
		ServerAddr:   "x",
		Classifier:   ClassifierCfg{Model: "anthropic/m", MaxTokens: 1, Timeout: time.Second},
		Weights:      Weights{Quality: 1},
		DefaultModel: "anthropic/m",
		Providers:    map[string]ProviderCreds{}, // no anthropic
		Catalog:      []CatalogEntry{{ID: "anthropic/m"}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing provider creds")
	}
}

func TestValidateNegativeWeight(t *testing.T) {
	c := baseValid()
	c.Weights.Cost = -0.1
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for negative weight")
	}
}

func TestValidateDefaultModelUnknownProvider(t *testing.T) {
	c := baseValid()
	c.DefaultModel = "ghost/m"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for default_model with unconfigured provider")
	}
}

func TestValidateBadProviderID(t *testing.T) {
	c := baseValid()
	c.Catalog = append(c.Catalog, CatalogEntry{ID: "noslash"})
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for catalog id without provider/model form")
	}
}

// baseValid returns a minimally valid Config (env set) for mutation in tests.
func baseValid() *Config {
	os.Setenv("TEST_KEY", "sk-test")
	return &Config{
		ServerAddr:   "127.0.0.1:8787",
		Classifier:   ClassifierCfg{Model: "anthropic/m", MaxTokens: 256, Timeout: time.Second},
		Weights:      Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		DefaultModel: "anthropic/m",
		Providers:    map[string]ProviderCreds{"anthropic": {APIKeyEnv: "TEST_KEY"}},
		Catalog:      []CatalogEntry{{ID: "anthropic/m", Quality: 0.7, Speed: 0.9, Caps: Caps{Tools: true, MaxContext: 200000}}},
	}
}
