package config

import (
	"bytes"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestMarshalRoundTrip(t *testing.T) {
	original := baseValid()
	// Every defaulted field is set explicitly: Marshal validates a copy, so an
	// unset field would come back defaulted and not match the original.
	original.Routing = RoutingCfg{
		Strategy: RoutingStrategySticky, DataPolicy: DataPolicyAllowRemote, SessionSticky: true, SessionTTL: 45 * time.Minute,
		CacheAware: false, MaxAttempts: 5, DefaultRemainingTurns: 7,
		MinSwitchSavingsUSD: 0.02, MinSwitchSavingsPct: 0.15, SwitchConfidence: 0.75,
		CostMode: CostModeAbsolute, CostReferenceUSD: 0.10, HighQualityFloor: 0.85, ReasoningBonus: 0.05,
		WorkflowAffinity: true,
	}
	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte("session_ttl: 45m0s")) {
		t.Fatalf("marshaled YAML missing duration: %s", data)
	}
	if bytes.Contains(data, []byte("session_sticky:")) || !bytes.Contains(data, []byte("strategy: sticky")) {
		t.Fatalf("marshaled YAML did not migrate strategy: %s", data)
	}
	decoded, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse marshaled config: %v\n%s", err, data)
	}
	if decoded.ServerAddr != original.ServerAddr || !reflect.DeepEqual(decoded.Routing, original.Routing) || len(decoded.Catalog) != 1 {
		t.Fatalf("round trip mismatch: %#v", decoded)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	if _, err := Parse([]byte("unknown: true\n")); err == nil {
		t.Fatal("expected unknown field to fail")
	}
}

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
	if c.Routing.Strategy != RoutingStrategyAmortized || c.Routing.SessionSticky || !c.Routing.CacheAware || c.Routing.SessionTTL != time.Hour ||
		c.Routing.DefaultRemainingTurns != 4 || c.Routing.MinSwitchSavingsUSD != 0.01 || c.Routing.MinSwitchSavingsPct != 0.10 || c.Routing.SwitchConfidence != 0.60 {
		t.Errorf("Routing defaults = %+v", c.Routing)
	}
	if c.Routing.DataPolicy != DataPolicyAllowRemote {
		t.Errorf("DataPolicy = %q, want default %q", c.Routing.DataPolicy, DataPolicyAllowRemote)
	}
}

func TestParseAndMarshalDataPolicyAndProviderLocation(t *testing.T) {
	cfg, err := Parse([]byte(`
server: {addr: "127.0.0.1:8787"}
weights: {quality: 1}
routing: {data_policy: local_only}
providers:
  ollama:
    kind: openai
    location: local
    base_url: http://127.0.0.1:11434/v1
catalog:
  - id: ollama/qwen
    quality: 0.7
    speed: 0.8
    caps: {max_context: 32768}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Routing.DataPolicy != DataPolicyLocalOnly || !cfg.IsLocalModel("ollama/qwen") {
		t.Fatalf("parsed placement = policy %q local %v", cfg.Routing.DataPolicy, cfg.IsLocalModel("ollama/qwen"))
	}
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte("data_policy: local_only")) || !bytes.Contains(data, []byte("location: local")) {
		t.Fatalf("marshaled YAML omitted placement fields:\n%s", data)
	}
}

func TestValidateRejectsInvalidDataPolicy(t *testing.T) {
	c := baseValid()
	c.Routing.DataPolicy = "sometimes_local"
	if err := c.Validate(); err == nil {
		t.Fatal("expected invalid data policy to fail")
	}
}

func TestValidateLocalProviderRequiresLoopbackBaseURL(t *testing.T) {
	for _, baseURL := range []string{"", "https://api.example.com/v1", "localhost:11434/v1", "ftp://127.0.0.1/model"} {
		c := baseValid()
		c.Providers["local"] = ProviderCreds{Kind: "openai", Location: ProviderLocationLocal, BaseURL: baseURL, APIKey: "x"}
		if err := c.Validate(); err == nil {
			t.Errorf("expected local base_url %q to fail", baseURL)
		}
	}
	for _, baseURL := range []string{"http://localhost:11434/v1", "http://127.0.0.1:11434/v1", "http://[::1]:11434/v1"} {
		c := baseValid()
		c.Providers["local"] = ProviderCreds{Kind: "openai", Location: ProviderLocationLocal, BaseURL: baseURL}
		if err := c.Validate(); err != nil {
			t.Errorf("loopback local base_url %q rejected: %v", baseURL, err)
		}
	}
}

func TestProviderLocationDefaultsRemote(t *testing.T) {
	c := baseValid()
	if c.IsLocalModel("anthropic/m") {
		t.Fatal("legacy provider without location must default to remote")
	}
	c.Providers["anthropic"] = ProviderCreds{Kind: "anthropic", Location: ProviderLocationLocal, BaseURL: "http://localhost:1"}
	if !c.IsLocalModel("anthropic/m") {
		t.Fatal("explicit local provider should classify catalog model as local")
	}
}

func TestParseMigratesLegacySessionSticky(t *testing.T) {
	for value, want := range map[string]string{"true": RoutingStrategySticky, "false": RoutingStrategyPerTurn} {
		yaml := []byte("server:\n  addr: 127.0.0.1:8787\nweights:\n  quality: 1\nrouting:\n  session_sticky: " + value + "\nproviders:\n  p:\n    base_url: http://localhost:1\n    kind: openai\ncatalog:\n  - id: p/m\n    quality: 1\n    speed: 1\n    caps:\n      max_context: 1000\n")
		cfg, err := Parse(yaml)
		if err != nil {
			t.Fatalf("session_sticky %s: %v", value, err)
		}
		if cfg.Routing.Strategy != want {
			t.Errorf("session_sticky %s strategy = %q, want %q", value, cfg.Routing.Strategy, want)
		}
	}
}

func TestValidateRejectsInvalidAmortizedSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"strategy", func(c *Config) { c.Routing.Strategy = "clairvoyant" }},
		{"turn horizon", func(c *Config) { c.Routing.DefaultRemainingTurns = 51 }},
		{"dollar threshold", func(c *Config) { c.Routing.MinSwitchSavingsUSD = -0.01 }},
		{"percentage threshold", func(c *Config) { c.Routing.MinSwitchSavingsPct = 1.01 }},
		{"confidence", func(c *Config) { c.Routing.SwitchConfidence = -0.1 }},
		{"efficiency", func(c *Config) { c.Catalog[0].TurnEfficiency.High = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseValid()
			cfg.Routing.Strategy = RoutingStrategyAmortized
			test.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
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

func TestParseDefaultsMaxAttempts(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
weights:
  quality: 1
catalog:
  - id: "p/m"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
providers:
  p:
    kind: "anthropic"
    api_key_env: "K"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want default 3", cfg.Routing.MaxAttempts)
	}
}

func TestParseHonoursExplicitMaxAttempts(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
weights:
  quality: 1
routing:
  max_attempts: 1
catalog:
  - id: "p/m"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
providers:
  p:
    kind: "anthropic"
    api_key_env: "K"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Routing.MaxAttempts != 1 {
		t.Errorf("MaxAttempts = %d, want 1", cfg.Routing.MaxAttempts)
	}
}

// Zero means "unset, use the default" because settings builds Config values by
// hand and would otherwise trip on the zero value. Negative is the error case.
func TestValidateRejectsNegativeMaxAttempts(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Routing:    RoutingCfg{MaxAttempts: -1, SessionTTL: time.Hour},
		Providers:  map[string]ProviderCreds{"p": {Kind: "anthropic", APIKeyEnv: "K"}},
		Catalog:    []CatalogEntry{{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000}}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want error for max_attempts = -1, got nil")
	}
}

func TestValidateDefaultsZeroMaxAttempts(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Routing:    RoutingCfg{MaxAttempts: 0, SessionTTL: time.Hour},
		Providers:  map[string]ProviderCreds{"p": {Kind: "anthropic", APIKeyEnv: "K"}},
		Catalog:    []CatalogEntry{{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero max_attempts should default, got error: %v", err)
	}
	if cfg.Routing.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want defaulted to 3", cfg.Routing.MaxAttempts)
	}
}

// Zero (or an absent key) means "unconstrained" so configs written before this
// field keep working; negative is the only invalid value.
func TestValidateRejectsNegativeMaxOutputTokens(t *testing.T) {
	cfg := &Config{
		ServerAddr: "127.0.0.1:8787",
		Weights:    Weights{Quality: 1},
		Providers:  map[string]ProviderCreds{"p": {Kind: "anthropic", APIKeyEnv: "K"}},
		Catalog: []CatalogEntry{
			{ID: "p/m", Quality: 0.5, Speed: 0.5, Caps: Caps{MaxContext: 1000, MaxOutputTokens: -1}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want error for negative max_output_tokens, got nil")
	}
}

// A config file that omits max_output_tokens must still parse and leave the
// field zero — the installed base ships configs without this key.
func TestParseAcceptsAbsentAndExplicitMaxOutputTokens(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  addr: "127.0.0.1:8787"
weights:
  quality: 1
catalog:
  - id: "p/omitted"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000 }
  - id: "p/explicit"
    quality: 0.5
    speed: 0.5
    caps: { max_context: 1000, max_output_tokens: 8192 }
providers:
  p:
    kind: "anthropic"
    api_key_env: "K"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Catalog[0].Caps.MaxOutputTokens; got != 0 {
		t.Errorf("omitted max_output_tokens = %d, want 0 (unconstrained)", got)
	}
	if got := cfg.Catalog[1].Caps.MaxOutputTokens; got != 8192 {
		t.Errorf("explicit max_output_tokens = %d, want 8192", got)
	}
}

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

// A typo in the variable name, or a GUI launch that never sourced the user's
// shell profile, silently disables authentication. Callers warn on this, so the
// predicate has to distinguish it from "no token was ever configured".
func TestAuthTokenMisconfigured(t *testing.T) {
	t.Setenv("OCTOPUS_MISCONF_SET", "s3cret")
	for _, c := range []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"not configured at all", &Config{}, false},
		{"configured and set", &Config{AuthTokenEnv: "OCTOPUS_MISCONF_SET"}, false},
		{"configured but variable unset", &Config{AuthTokenEnv: "OCTOPUS_MISCONF_ABSENT"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.AuthTokenMisconfigured(); got != c.want {
				t.Errorf("AuthTokenMisconfigured() = %v, want %v", got, c.want)
			}
		})
	}
}
