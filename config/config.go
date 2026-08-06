// Package config loads and validates the router's YAML configuration:
// server settings, the classifier model, scoring weights, provider
// credentials, and the model catalog.
package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
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

// TurnEfficiency estimates how much task progress a model makes per turn at
// each difficulty. A value of 1 is the neutral prior; 2 means the model is
// expected to finish the same work in half as many turns. Zero in YAML means
// "unspecified" and is treated as 1 for backwards compatibility.
type TurnEfficiency struct {
	Trivial float64 `yaml:"trivial,omitempty" json:"trivial"`
	Low     float64 `yaml:"low,omitempty" json:"low"`
	Medium  float64 `yaml:"medium,omitempty" json:"medium"`
	High    float64 `yaml:"high,omitempty" json:"high"`
}

// ForDifficulty returns the configured prior, defaulting to the neutral 1.0.
func (e TurnEfficiency) ForDifficulty(difficulty string) float64 {
	value := 0.0
	switch difficulty {
	case "trivial":
		value = e.Trivial
	case "low":
		value = e.Low
	case "medium":
		value = e.Medium
	case "high":
		value = e.High
	}
	if value <= 0 {
		return 1
	}
	return value
}

// CatalogEntry is one candidate model. ID is "provider/model".
type CatalogEntry struct {
	ID             string         `yaml:"id"`
	Quality        float64        `yaml:"quality"`
	CostPerMTokIn  float64        `yaml:"cost_per_mtok_in"`
	CostPerMTokOut float64        `yaml:"cost_per_mtok_out"`
	Speed          float64        `yaml:"speed"`
	Caps           Caps           `yaml:"caps"`
	TurnEfficiency TurnEfficiency `yaml:"turn_efficiency,omitempty"`
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
	// Location declares whether requests stay on this machine. Providers are
	// remote by default; local providers must use a loopback base URL.
	Location string `yaml:"location,omitempty"`
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

// IsLocalModel reports whether the provider named by a provider/model catalog
// ID is explicitly configured as local. Missing and legacy locations are
// deliberately remote: local-only routing must fail closed.
func (c *Config) IsLocalModel(id string) bool {
	provider, ok := providerOf(id)
	if !ok {
		return false
	}
	creds, ok := c.Providers[provider]
	return ok && creds.Location == ProviderLocationLocal
}

// ClassifierCfg configures the fixed classifier model call.
type ClassifierCfg struct {
	Model     string        `yaml:"model"`
	MaxTokens int           `yaml:"max_tokens"`
	Timeout   time.Duration `yaml:"timeout"`
}

const (
	RoutingStrategyAmortized = "amortized"
	RoutingStrategySticky    = "sticky"
	RoutingStrategyPerTurn   = "per_turn"
)

const (
	DataPolicyAllowRemote  = "allow_remote"
	DataPolicyPreferLocal  = "prefer_local"
	DataPolicyLocalOnly    = "local_only"
	ProviderLocationLocal  = "local"
	ProviderLocationRemote = "remote"
)

// RoutingCfg controls conversation affinity and cache-aware request pricing.
type RoutingCfg struct {
	// Strategy selects hard affinity, greedy per-turn scoring, or a
	// cost-to-completion comparison that amortises the first cold cache write.
	Strategy string `yaml:"strategy"`
	// DataPolicy controls whether remote providers may receive request data.
	DataPolicy string `yaml:"data_policy"`
	// SessionSticky is retained as an in-memory compatibility field for code
	// constructing Config directly. Parsed legacy YAML is translated to
	// Strategy and newly marshalled YAML writes strategy instead.
	SessionSticky         bool          `yaml:"session_sticky"`
	SessionTTL            time.Duration `yaml:"session_ttl"`
	CacheAware            bool          `yaml:"cache_aware"`
	DefaultRemainingTurns int           `yaml:"default_remaining_turns"`
	MinSwitchSavingsUSD   float64       `yaml:"min_switch_savings_usd"`
	MinSwitchSavingsPct   float64       `yaml:"min_switch_savings_pct"`
	SwitchConfidence      float64       `yaml:"switch_confidence"`
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

// AuthTokenMisconfigured reports that a shared secret was configured but the
// named variable is empty, so authentication is silently off. Callers warn on
// this: a typo in the variable name, or a GUI launch that never sourced the
// user's shell profile, otherwise disables the control with no signal at all.
func (c *Config) AuthTokenMisconfigured() bool {
	return c.AuthTokenEnv != "" && c.AuthToken() == ""
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
	Strategy              string        `yaml:"strategy,omitempty"`
	DataPolicy            string        `yaml:"data_policy,omitempty"`
	SessionSticky         *bool         `yaml:"session_sticky,omitempty"`
	SessionTTL            time.Duration `yaml:"session_ttl"`
	CacheAware            *bool         `yaml:"cache_aware"`
	MaxAttempts           *int          `yaml:"max_attempts"`
	DefaultRemainingTurns *int          `yaml:"default_remaining_turns"`
	MinSwitchSavingsUSD   *float64      `yaml:"min_switch_savings_usd"`
	MinSwitchSavingsPct   *float64      `yaml:"min_switch_savings_pct"`
	SwitchConfidence      *float64      `yaml:"switch_confidence"`
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
	cacheAware := true
	strategy := yc.Routing.Strategy
	if strategy == "" {
		strategy = RoutingStrategyAmortized
		if yc.Routing.SessionSticky != nil {
			if *yc.Routing.SessionSticky {
				strategy = RoutingStrategySticky
			} else {
				strategy = RoutingStrategyPerTurn
			}
		}
	}
	dataPolicy := yc.Routing.DataPolicy
	if dataPolicy == "" {
		dataPolicy = DataPolicyAllowRemote
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
	defaultRemainingTurns := 4
	if yc.Routing.DefaultRemainingTurns != nil {
		defaultRemainingTurns = *yc.Routing.DefaultRemainingTurns
	}
	minSwitchSavingsUSD := 0.01
	if yc.Routing.MinSwitchSavingsUSD != nil {
		minSwitchSavingsUSD = *yc.Routing.MinSwitchSavingsUSD
	}
	minSwitchSavingsPct := 0.10
	if yc.Routing.MinSwitchSavingsPct != nil {
		minSwitchSavingsPct = *yc.Routing.MinSwitchSavingsPct
	}
	switchConfidence := 0.60
	if yc.Routing.SwitchConfidence != nil {
		switchConfidence = *yc.Routing.SwitchConfidence
	}
	c := &Config{
		ServerAddr:   yc.Server.Addr,
		AuthTokenEnv: yc.Server.AuthTokenEnv,
		Classifier:   yc.Classifier,
		Weights:      yc.Weights,
		Routing: RoutingCfg{
			Strategy:              strategy,
			DataPolicy:            dataPolicy,
			SessionSticky:         strategy == RoutingStrategySticky,
			SessionTTL:            sessionTTL,
			CacheAware:            cacheAware,
			MaxAttempts:           maxAttempts,
			DefaultRemainingTurns: defaultRemainingTurns,
			MinSwitchSavingsUSD:   minSwitchSavingsUSD,
			MinSwitchSavingsPct:   minSwitchSavingsPct,
			SwitchConfidence:      switchConfidence,
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
	cacheAware := copyCfg.Routing.CacheAware
	yc := yamlConfig{
		Classifier: copyCfg.Classifier,
		Weights:    copyCfg.Weights,
		Routing: yamlRoutingCfg{
			Strategy:   copyCfg.Routing.Strategy,
			DataPolicy: copyCfg.Routing.DataPolicy,
			SessionTTL: copyCfg.Routing.SessionTTL, CacheAware: &cacheAware, MaxAttempts: &copyCfg.Routing.MaxAttempts,
			DefaultRemainingTurns: &copyCfg.Routing.DefaultRemainingTurns,
			MinSwitchSavingsUSD:   &copyCfg.Routing.MinSwitchSavingsUSD,
			MinSwitchSavingsPct:   &copyCfg.Routing.MinSwitchSavingsPct,
			SwitchConfidence:      &copyCfg.Routing.SwitchConfidence,
		},
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
	if c.Routing.Strategy == "" {
		if c.Routing.SessionSticky {
			c.Routing.Strategy = RoutingStrategySticky
		} else {
			c.Routing.Strategy = RoutingStrategyAmortized
		}
	}
	switch c.Routing.Strategy {
	case RoutingStrategyAmortized, RoutingStrategySticky, RoutingStrategyPerTurn:
	default:
		return fmt.Errorf("routing.strategy must be amortized, sticky, or per_turn")
	}
	if c.Routing.DataPolicy == "" {
		c.Routing.DataPolicy = DataPolicyAllowRemote
	}
	switch c.Routing.DataPolicy {
	case DataPolicyAllowRemote, DataPolicyPreferLocal, DataPolicyLocalOnly:
	default:
		return fmt.Errorf("routing.data_policy must be allow_remote, prefer_local, or local_only")
	}
	c.Routing.SessionSticky = c.Routing.Strategy == RoutingStrategySticky
	if c.Routing.SessionTTL == 0 {
		c.Routing.SessionTTL = time.Hour
	}
	if c.Routing.DefaultRemainingTurns == 0 {
		c.Routing.DefaultRemainingTurns = 4
	}
	if c.Routing.DefaultRemainingTurns < 1 || c.Routing.DefaultRemainingTurns > 50 {
		return fmt.Errorf("routing.default_remaining_turns must be in [1,50]")
	}
	if !finite(c.Routing.MinSwitchSavingsUSD) || c.Routing.MinSwitchSavingsUSD < 0 {
		return fmt.Errorf("routing.min_switch_savings_usd must be a finite non-negative number")
	}
	if !finite(c.Routing.MinSwitchSavingsPct) || c.Routing.MinSwitchSavingsPct < 0 || c.Routing.MinSwitchSavingsPct > 1 {
		return fmt.Errorf("routing.min_switch_savings_pct must be in [0,1]")
	}
	if !finite(c.Routing.SwitchConfidence) || c.Routing.SwitchConfidence < 0 || c.Routing.SwitchConfidence > 1 {
		return fmt.Errorf("routing.switch_confidence must be in [0,1]")
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
		location := creds.Location
		if location == "" {
			location = ProviderLocationRemote
		}
		switch location {
		case ProviderLocationRemote:
		case ProviderLocationLocal:
			if creds.BaseURL == "" {
				return fmt.Errorf("provider %q: local location requires a loopback base_url", name)
			}
			u, err := url.Parse(creds.BaseURL)
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("provider %q: local base_url must be an absolute loopback URL", name)
			}
			host := u.Hostname()
			if !strings.EqualFold(host, "localhost") {
				ip := net.ParseIP(host)
				if ip == nil || !ip.IsLoopback() {
					return fmt.Errorf("provider %q: local base_url must use a loopback host", name)
				}
			}
		default:
			return fmt.Errorf("provider %q has unknown location %q (want local|remote)", name, location)
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
		for difficulty, efficiency := range map[string]float64{
			"trivial": e.TurnEfficiency.Trivial, "low": e.TurnEfficiency.Low,
			"medium": e.TurnEfficiency.Medium, "high": e.TurnEfficiency.High,
		} {
			if !finite(efficiency) || efficiency < 0 {
				return fmt.Errorf("catalog id %q: turn_efficiency.%s must be a finite non-negative number", e.ID, difficulty)
			}
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
