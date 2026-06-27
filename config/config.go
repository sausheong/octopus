// Package config loads and validates the router's YAML configuration:
// server settings, the classifier model, scoring weights, provider
// credentials, and the model catalog.
package config

import (
	"fmt"
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
// optional base URL override.
type ProviderCreds struct {
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"`
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
	if err := yaml.Unmarshal(data, &yc); err != nil {
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
	if c.Weights.Quality < 0 || c.Weights.Cost < 0 || c.Weights.Speed < 0 {
		return fmt.Errorf("weights must be non-negative")
	}
	if c.Weights.Quality+c.Weights.Cost+c.Weights.Speed == 0 {
		return fmt.Errorf("weights must not all be zero")
	}
	if len(c.Catalog) == 0 {
		return fmt.Errorf("catalog must have at least one entry")
	}
	// Every catalog id must be provider/model and its provider configured.
	for _, e := range c.Catalog {
		p, ok := providerOf(e.ID)
		if !ok {
			return fmt.Errorf("catalog id %q must be in provider/model form", e.ID)
		}
		if _, ok := c.Providers[p]; !ok {
			return fmt.Errorf("catalog id %q references unconfigured provider %q", e.ID, p)
		}
	}
	// Classifier + default_model must resolve to a configured provider.
	for label, id := range map[string]string{"classifier.model": c.Classifier.Model, "default_model": c.DefaultModel} {
		p, ok := providerOf(id)
		if !ok {
			return fmt.Errorf("%s %q must be in provider/model form", label, id)
		}
		if _, ok := c.Providers[p]; !ok {
			return fmt.Errorf("%s %q references unconfigured provider %q", label, id, p)
		}
	}
	if c.Classifier.MaxTokens <= 0 {
		return fmt.Errorf("classifier.max_tokens must be > 0")
	}
	if c.Classifier.Timeout <= 0 {
		return fmt.Errorf("classifier.timeout must be > 0")
	}
	return nil
}
