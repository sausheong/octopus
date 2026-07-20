package config

import (
	"math"
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
	if !c.Routing.SessionSticky || !c.Routing.CacheAware || c.Routing.SessionTTL != time.Hour {
		t.Errorf("Routing defaults = %+v", c.Routing)
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

func TestValidateRejectsNonLoopbackServerAddr(t *testing.T) {
	c := baseValid()
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "192.168.1.20:8787", "example.com:8787"} {
		c.ServerAddr = addr
		if err := c.Validate(); err == nil {
			t.Errorf("expected non-loopback server.addr %q to be rejected", addr)
		}
	}
}

func TestValidateAcceptsLoopbackServerAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		c := baseValid()
		c.ServerAddr = addr
		if err := c.Validate(); err != nil {
			t.Errorf("loopback server.addr %q rejected: %v", addr, err)
		}
	}
}

func TestValidateDeprecatedDefaultModelIsIgnored(t *testing.T) {
	c := baseValid()
	c.DefaultModel = "ghost/m"
	if err := c.Validate(); err != nil {
		t.Fatalf("deprecated default_model should be ignored: %v", err)
	}
}

func TestValidateDefaultModelMayBeOmitted(t *testing.T) {
	c := baseValid()
	c.DefaultModel = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("deprecated default_model should be optional: %v", err)
	}
}

func TestValidateRequiresMaxContext(t *testing.T) {
	c := baseValid()
	c.Catalog[0].Caps.MaxContext = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing caps.max_context")
	}
}

func TestValidateBadProviderID(t *testing.T) {
	c := baseValid()
	c.Catalog = append(c.Catalog, CatalogEntry{ID: "noslash"})
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for catalog id without provider/model form")
	}
}

func TestValidateUnknownKind(t *testing.T) {
	c := baseValid()
	c.Providers["weird"] = ProviderCreds{Kind: "mystery", APIKey: "x"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for unknown provider kind")
	}
}

func TestValidateCustomKindAccepted(t *testing.T) {
	// A custom-named provider with an explicit known kind + inline key is valid,
	// and its catalog id (provider/model) resolves to it.
	c := baseValid()
	c.Providers["deepseek"] = ProviderCreds{Kind: "anthropic", APIKey: "sk-x", BaseURL: "https://api.deepseek.com/anthropic"}
	c.Catalog = append(c.Catalog, CatalogEntry{ID: "deepseek/deepseek-v4-pro[1m]", Quality: 0.88, Speed: 0.6, Caps: Caps{Tools: true, MaxContext: 1000000}})
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidateProviderMissingCredentialSource(t *testing.T) {
	c := baseValid()
	c.Providers["nokey"] = ProviderCreds{Kind: "openai"} // no api_key, no api_key_env, no base_url
	c.Catalog = append(c.Catalog, CatalogEntry{ID: "nokey/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000}})
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for provider with no api_key, api_key_env, or base_url")
	}
}

func TestValidateLocalProviderBaseURLOnly(t *testing.T) {
	c := baseValid()
	c.Providers["local"] = ProviderCreds{Kind: "openai", BaseURL: "http://localhost:8080/v1"}
	c.Catalog = append(c.Catalog, CatalogEntry{ID: "local/my-model", Quality: 0.6, Speed: 0.9, Caps: Caps{MaxContext: 32768}})
	if err := c.Validate(); err != nil {
		t.Fatalf("expected base_url-only provider to be valid, got: %v", err)
	}
}

func TestValidateClassifierModelEmptyIsValid(t *testing.T) {
	// Pure-local setup: no classifier model, router always uses DefaultProfile.
	c := baseValid()
	c.Classifier.Model = ""
	c.Classifier.MaxTokens = 0
	c.Classifier.Timeout = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("empty classifier.model should be valid, got: %v", err)
	}
}

func TestValidateClassifierModelUnconfiguredProviderIsValid(t *testing.T) {
	// classifier.model may reference a provider not in cfg.Providers — the
	// router falls back to DefaultProfile at runtime rather than failing.
	c := baseValid()
	c.Classifier.Model = "cloud/some-model" // "cloud" is not in Providers
	if err := c.Validate(); err != nil {
		t.Fatalf("classifier referencing unconfigured provider should be valid, got: %v", err)
	}
}

func TestValidateClassifierModelBadFormIsError(t *testing.T) {
	c := baseValid()
	c.Classifier.Model = "noslash"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for classifier.model not in provider/model form")
	}
}

func TestValidateNaNWeightIsError(t *testing.T) {
	c := baseValid()
	c.Weights.Quality = math.NaN()
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for NaN weight")
	}
}

func TestValidateInfWeightIsError(t *testing.T) {
	c := baseValid()
	c.Weights.Cost = math.Inf(1)
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for infinite weight")
	}
}

func TestValidateNaNCatalogQualityIsError(t *testing.T) {
	c := baseValid()
	c.Catalog[0].Quality = math.NaN()
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for NaN catalog quality")
	}
}

func TestValidateDuplicateCatalogIDIsError(t *testing.T) {
	c := baseValid()
	c.Catalog = append(c.Catalog, c.Catalog[0]) // duplicate
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for duplicate catalog id")
	}
}

func TestLoadUnknownFieldIsError(t *testing.T) {
	// Write a temp YAML with an unknown key and verify Load rejects it.
	f, err := os.CreateTemp("", "octopus-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(`
server:
  addr: "127.0.0.1:9999"
unknown_key: "should fail"
weights: {quality: 1, cost: 1, speed: 1}
default_model: "anthropic/m"
providers:
  anthropic: {api_key: "x"}
catalog:
  - id: "anthropic/m"
    quality: 0.7
    speed: 0.9
    caps: {max_context: 100000}
`)
	f.Close()
	if _, err := Load(f.Name()); err == nil {
		t.Fatal("expected error for unknown YAML field")
	}
}

func TestKeyPrefersInlineThenEnv(t *testing.T) {
	os.Setenv("KEY_ENV_ONLY", "from-env")
	if got := (ProviderCreds{APIKey: "inline"}).Key(); got != "inline" {
		t.Errorf("inline key = %q, want inline", got)
	}
	if got := (ProviderCreds{APIKeyEnv: "KEY_ENV_ONLY"}).Key(); got != "from-env" {
		t.Errorf("env key = %q, want from-env", got)
	}
	if got := (ProviderCreds{}).Key(); got != "" {
		t.Errorf("empty creds key = %q, want empty", got)
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
