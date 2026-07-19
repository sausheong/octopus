// Package config loads and validates the router's YAML configuration:
// server settings, the classifier model, scoring weights, provider
// credentials, and the model catalog.
package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Weights are the balanced-score knobs. They need not sum to 1; the scorer
// normalizes. All must be >= 0.
type Weights struct {
	Quality float64 `yaml:"quality"`
	Cost    float64 `yaml:"cost"`
	Speed   float64 `yaml:"speed"`
}

// Caps are a model's hard capabilities, used as filters by the scorer.
type Caps struct {
	Tools      bool `yaml:"tools"`
	Vision     bool `yaml:"vision"`
	Reasoning  bool `yaml:"reasoning"`
	MaxContext int  `yaml:"max_context"`
}

// CatalogEntry is one candidate model. ID is "provider/model".
type CatalogEntry struct {
	ID             string  `yaml:"id"`
	Quality        float64 `yaml:"quality"`
	CostPerMTokIn  float64 `yaml:"cost_per_mtok_in"`
	CostPerMTokOut float64 `yaml:"cost_per_mtok_out"`
	Speed          float64 `yaml:"speed"`
	Caps           Caps    `yaml:"caps"`
}

// ProviderCreds names the env var holding a provider's API key, plus an
// optional base URL override and an optional Kind selecting which harness
// client type to build. Kind lets several providers under distinct names
// share the same wire protocol — e.g. multiple Anthropic-compatible
// backends (DeepSeek, MiniMax, Qwen-coding) each with kind "anthropic" but
// their own base_url and key. When Kind is empty it defaults to the
// provider's map key (so "anthropic"/"openai"/"gemini"/"qwen" keep working
// unchanged).
type ProviderCreds struct {
	// APIKey, when set, is the literal API key. It takes precedence over
	// APIKeyEnv. Use it only in a config file kept out of version control
	// (config.yaml is gitignored); prefer APIKeyEnv for committed configs.
	APIKey    string `yaml:"api_key"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
	Kind      string `yaml:"kind"`
}

// Key returns the resolved API key: the inline APIKey if set, otherwise the
// value of the environment variable named by APIKeyEnv.
func (p ProviderCreds) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

// ClassifierCfg configures the fixed classifier model call.
type ClassifierCfg struct {
	Model     string        `yaml:"model"`
	MaxTokens int           `yaml:"max_tokens"`
	Timeout   time.Duration `yaml:"timeout"`
}

// Config is the full router configuration.
type Config struct {
	ServerAddr   string                   `yaml:"-"`
	Classifier   ClassifierCfg            `yaml:"classifier"`
	Weights      Weights                  `yaml:"weights"`
	DefaultModel string                   `yaml:"default_model"`
	Providers    map[string]ProviderCreds `yaml:"providers"`
	Catalog      []CatalogEntry           `yaml:"catalog"`
}

// yamlConfig mirrors Config but nests server.addr so YAML maps cleanly,
// then we flatten into Config.
type yamlConfig struct {
	Server struct {
		Addr string `yaml:"addr"`
	} `yaml:"server"`
	Classifier   ClassifierCfg            `yaml:"classifier"`
	Weights      Weights                  `yaml:"weights"`
	DefaultModel string                   `yaml:"default_model"`
	Providers    map[string]ProviderCreds `yaml:"providers"`
	Catalog      []CatalogEntry           `yaml:"catalog"`
}

// Load reads, parses, and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var yc yamlConfig
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&yc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c := &Config{
		ServerAddr:   yc.Server.Addr,
		Classifier:   yc.Classifier,
		Weights:      yc.Weights,
		DefaultModel: yc.DefaultModel,
		Providers:    yc.Providers,
		Catalog:      yc.Catalog,
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// providerOf returns the provider half of a "provider/model" id, and whether
// the id was well-formed (contained a slash with a non-empty provider).
func providerOf(id string) (string, bool) {
	p, m, ok := strings.Cut(id, "/")
	if !ok || p == "" || m == "" {
		return "", false
	}
	return p, true
}

// Validate checks structural and referential integrity. Fails fast with a
// clear message so misconfiguration never reaches request handling.
func (c *Config) Validate() error {
	if c.ServerAddr == "" {
		return fmt.Errorf("server.addr is required")
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(c.Weights.Quality) || !finite(c.Weights.Cost) || !finite(c.Weights.Speed) {
		return fmt.Errorf("weights must be finite numbers")
	}
	if c.Weights.Quality < 0 || c.Weights.Cost < 0 || c.Weights.Speed < 0 {
		return fmt.Errorf("weights must be non-negative")
	}
	if c.Weights.Quality+c.Weights.Cost+c.Weights.Speed == 0 {
		return fmt.Errorf("weights must not all be zero")
	}
	if len(c.Catalog) == 0 {
		return fmt.Errorf("catalog must have at least one entry")
	}
	// Each provider's kind (defaulting to its name) must be a known client
	// type, and it must carry a credential source (inline key or env var).
	for name, creds := range c.Providers {
		kind := creds.Kind
		if kind == "" {
			kind = name
		}
		switch kind {
		case "anthropic", "openai", "gemini", "qwen":
		default:
			return fmt.Errorf("provider %q has unknown kind %q (want anthropic|openai|gemini|qwen)", name, kind)
		}
		if creds.APIKey == "" && creds.APIKeyEnv == "" && creds.BaseURL == "" {
			return fmt.Errorf("provider %q must set api_key, api_key_env, or base_url", name)
		}
	}
	// Every catalog id must be provider/model, its provider configured, and
	// its numeric fields in range. Duplicate IDs are also rejected.
	seen := make(map[string]bool, len(c.Catalog))
	for _, e := range c.Catalog {
		p, ok := providerOf(e.ID)
		if !ok {
			return fmt.Errorf("catalog id %q must be in provider/model form", e.ID)
		}
		if _, ok := c.Providers[p]; !ok {
			return fmt.Errorf("catalog id %q references unconfigured provider %q", e.ID, p)
		}
		if seen[e.ID] {
			return fmt.Errorf("catalog id %q is duplicated", e.ID)
		}
		seen[e.ID] = true
		if !finite(e.Quality) || !finite(e.Speed) || !finite(e.CostPerMTokIn) || !finite(e.CostPerMTokOut) {
			return fmt.Errorf("catalog id %q: numeric fields must be finite", e.ID)
		}
		if e.Quality < 0 || e.Quality > 1 {
			return fmt.Errorf("catalog id %q: quality must be in [0,1], got %v", e.ID, e.Quality)
		}
		if e.Speed < 0 || e.Speed > 1 {
			return fmt.Errorf("catalog id %q: speed must be in [0,1], got %v", e.ID, e.Speed)
		}
		if e.CostPerMTokIn < 0 || e.CostPerMTokOut < 0 {
			return fmt.Errorf("catalog id %q: costs must be non-negative", e.ID)
		}
	}
	// default_model must resolve to a configured provider — it is the last-resort
	// fallback and must always be reachable.
	{
		p, ok := providerOf(c.DefaultModel)
		if !ok {
			return fmt.Errorf("default_model %q must be in provider/model form", c.DefaultModel)
		}
		if _, ok := c.Providers[p]; !ok {
			return fmt.Errorf("default_model %q references unconfigured provider %q", c.DefaultModel, p)
		}
	}
	// classifier.model is optional. When empty the router always uses
	// DefaultProfile (no LLM call), which is safe for pure-local setups.
	// When set it must be valid provider/model form; the provider need not be
	// configured — if it isn't, the router falls back to DefaultProfile at
	// runtime rather than refusing to start.
	if c.Classifier.Model != "" {
		if _, ok := providerOf(c.Classifier.Model); !ok {
			return fmt.Errorf("classifier.model %q must be in provider/model form", c.Classifier.Model)
		}
		if c.Classifier.MaxTokens <= 0 {
			return fmt.Errorf("classifier.max_tokens must be > 0")
		}
		if c.Classifier.Timeout <= 0 {
			return fmt.Errorf("classifier.timeout must be > 0")
		}
	}
	return nil
}
