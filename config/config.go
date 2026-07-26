// Package config loads and validates the router's YAML configuration:
// server settings, the classifier model, scoring weights, provider
// credentials, and the model catalog.
package config

import (
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Weights are the balanced-score knobs. They need not sum to 1; the scorer
// normalizes. All must be >= 0.
type Weights struct {
	Quality float64 `yaml:"quality" json:"quality"`
	Cost    float64 `yaml:"cost" json:"cost"`
	Speed   float64 `yaml:"speed" json:"speed"`
}

// Caps describe model capabilities. Tools, vision, and context are hard
// constraints; reasoning support is a scoring preference.
type Caps struct {
	Tools      bool `yaml:"tools" json:"tools"`
	Vision     bool `yaml:"vision" json:"vision"`
	Reasoning  bool `yaml:"reasoning" json:"reasoning"`
	MaxContext int  `yaml:"max_context" json:"max_context"`
	// MaxOutputTokens is the model's output limit. Zero means unconstrained,
	// so configs written before this field keep working unchanged.
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
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

// RoutingCfg controls conversation affinity and cache-aware request pricing.
type RoutingCfg struct {
	SessionSticky bool          `yaml:"session_sticky"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
	CacheAware    bool          `yaml:"cache_aware"`
	// MaxAttempts bounds how many backends one request may try. Without it,
	// worst-case latency and spend grow with catalog size.
	MaxAttempts int `yaml:"max_attempts"`
}

// Config is the full router configuration.
type Config struct {
	ServerAddr string `yaml:"-"`
	// AuthTokenEnv names the environment variable holding a shared secret that
	// callers must present on the routing endpoints. Empty means no
	// authentication, which is the default: a signed installer is already in
	// use and requiring a token would break every existing client.
	AuthTokenEnv string        `yaml:"-"`
	Classifier   ClassifierCfg `yaml:"classifier"`
	Weights      Weights       `yaml:"weights"`
	Routing      RoutingCfg    `yaml:"routing"`
	// DefaultModel is accepted for compatibility with older configuration
	// files. It is deprecated and ignored; an empty eligible set is an error.
	DefaultModel string                   `yaml:"default_model"`
	Providers    map[string]ProviderCreds `yaml:"providers"`
	Catalog      []CatalogEntry           `yaml:"catalog"`
}

// AuthToken resolves the configured shared secret. An unset or empty variable
// yields "", which callers must treat as "authentication disabled" rather than
// "the expected token is empty" — the latter would accept every request.
func (c *Config) AuthToken() string {
	if c.AuthTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.AuthTokenEnv)
}

// yamlConfig mirrors Config but nests server.addr so YAML maps cleanly,
// then we flatten into Config.
type yamlConfig struct {
	Server struct {
		Addr         string `yaml:"addr"`
		AuthTokenEnv string `yaml:"auth_token_env"`
	} `yaml:"server"`
	Classifier   ClassifierCfg            `yaml:"classifier"`
	Weights      Weights                  `yaml:"weights"`
	Routing      yamlRoutingCfg           `yaml:"routing"`
	DefaultModel string                   `yaml:"default_model"`
	Providers    map[string]ProviderCreds `yaml:"providers"`
	Catalog      []CatalogEntry           `yaml:"catalog"`
}

type yamlRoutingCfg struct {
	SessionSticky *bool         `yaml:"session_sticky"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
	CacheAware    *bool         `yaml:"cache_aware"`
	MaxAttempts   *int          `yaml:"max_attempts"`
}

// Load reads, parses, and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes and validates an Octopus YAML configuration.
func Parse(data []byte) (*Config, error) {
	var yc yamlConfig
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&yc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	sticky, cacheAware := true, true
	if yc.Routing.SessionSticky != nil {
		sticky = *yc.Routing.SessionSticky
	}
	if yc.Routing.CacheAware != nil {
		cacheAware = *yc.Routing.CacheAware
	}
	sessionTTL := yc.Routing.SessionTTL
	if sessionTTL == 0 {
		sessionTTL = time.Hour
	}
	maxAttempts := 3
	if yc.Routing.MaxAttempts != nil {
		maxAttempts = *yc.Routing.MaxAttempts
	}
	c := &Config{
		ServerAddr:   yc.Server.Addr,
		AuthTokenEnv: yc.Server.AuthTokenEnv,
		Classifier:   yc.Classifier,
		Weights:      yc.Weights,
		Routing: RoutingCfg{
			SessionSticky: sticky,
			SessionTTL:    sessionTTL,
			CacheAware:    cacheAware,
			MaxAttempts:   maxAttempts,
		},
		DefaultModel: yc.DefaultModel,
		Providers:    yc.Providers,
		Catalog:      yc.Catalog,
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Marshal validates and encodes a Config using Octopus's nested YAML shape.
func Marshal(c *Config) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("config is required")
	}
	copyCfg := *c
	if err := copyCfg.Validate(); err != nil {
		return nil, err
	}
	sticky := copyCfg.Routing.SessionSticky
	cacheAware := copyCfg.Routing.CacheAware
	yc := yamlConfig{
		Classifier:   copyCfg.Classifier,
		Weights:      copyCfg.Weights,
		Routing:      yamlRoutingCfg{SessionSticky: &sticky, SessionTTL: copyCfg.Routing.SessionTTL, CacheAware: &cacheAware, MaxAttempts: &copyCfg.Routing.MaxAttempts},
		DefaultModel: copyCfg.DefaultModel,
		Providers:    copyCfg.Providers,
		Catalog:      copyCfg.Catalog,
	}
	yc.Server.Addr = copyCfg.ServerAddr
	yc.Server.AuthTokenEnv = copyCfg.AuthTokenEnv
	data, err := yaml.Marshal(&yc)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return data, nil
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
	host, _, err := net.SplitHostPort(c.ServerAddr)
	if err != nil {
		return fmt.Errorf("server.addr must be in host:port form: %w", err)
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("server.addr must use a loopback host because inbound requests may be unauthenticated")
		}
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
	if c.Routing.SessionTTL < 0 {
		return fmt.Errorf("routing.session_ttl must not be negative")
	}
	if c.Routing.SessionSticky && c.Routing.SessionTTL == 0 {
		c.Routing.SessionTTL = time.Hour
	}
	if c.Routing.MaxAttempts < 0 {
		return fmt.Errorf("routing.max_attempts must not be negative")
	}
	if c.Routing.MaxAttempts == 0 {
		c.Routing.MaxAttempts = 3
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
		if e.Caps.MaxContext <= 0 {
			return fmt.Errorf("catalog id %q: caps.max_context must be > 0", e.ID)
		}
		// Zero is legal and means unconstrained; only a negative limit is a
		// configuration mistake.
		if e.Caps.MaxOutputTokens < 0 {
			return fmt.Errorf("catalog id %q: caps.max_output_tokens must not be negative", e.ID)
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
