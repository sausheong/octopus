package settings

import (
	"fmt"
	"sort"
	"time"

	"github.com/sausheong/octopus/config"
)

type Document struct {
	ServerAddr string `json:"server_addr"`
	// AuthTokenEnv must round-trip even though no form control edits it.
	// config() rebuilds config.Config field by field, so a field the Document
	// does not carry is zeroed on every structured save — and zeroing this one
	// silently turns authentication off.
	AuthTokenEnv      string             `json:"auth_token_env"`
	ClassifierEnabled bool               `json:"classifier_enabled"`
	Classifier        ClassifierDocument `json:"classifier"`
	Weights           config.Weights     `json:"weights"`
	Routing           RoutingDocument    `json:"routing"`
	Providers         []ProviderDocument `json:"providers"`
	Catalog           []CatalogDocument  `json:"catalog"`
}

type ClassifierDocument struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Timeout   string `json:"timeout"`
}

type RoutingDocument struct {
	Strategy              string               `json:"strategy"`
	DataPolicy            string               `json:"data_policy"`
	SessionTTL            string               `json:"session_ttl"`
	CacheAware            bool                 `json:"cache_aware"`
	MaxAttempts           int                  `json:"max_attempts"`
	DefaultRemainingTurns int                  `json:"default_remaining_turns"`
	MinSwitchSavingsUSD   float64              `json:"min_switch_savings_usd"`
	MinSwitchSavingsPct   float64              `json:"min_switch_savings_pct"`
	SwitchConfidence      float64              `json:"switch_confidence"`
	CostMode              string               `json:"cost_mode"`
	CostReferenceUSD      float64              `json:"cost_reference_usd"`
	QualityFloors         map[string]float64   `json:"quality_floors"`
	HighQualityFloor      float64              `json:"high_quality_floor"`
	ReasoningBonus        float64              `json:"reasoning_bonus"`
	WorkflowAffinity      bool                 `json:"workflow_affinity"`
	Background            config.BackgroundCfg `json:"background"`
}

type ProviderDocument struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Location  string `json:"location"`
	APIKeyEnv string `json:"api_key_env"`
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url"`
}

type CatalogDocument struct {
	ID             string                `json:"id"`
	Quality        float64               `json:"quality"`
	CostPerMTokIn  float64               `json:"cost_per_mtok_in"`
	CostPerMTokOut float64               `json:"cost_per_mtok_out"`
	Speed          float64               `json:"speed"`
	Caps           config.Caps           `json:"caps"`
	TurnEfficiency config.TurnEfficiency `json:"turn_efficiency"`
}

func defaultDocument() Document {
	return Document{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		Routing: RoutingDocument{
			Strategy: config.RoutingStrategyAmortized, DataPolicy: config.DataPolicyAllowRemote,
			SessionTTL: "1h", CacheAware: true, MaxAttempts: 3,
			DefaultRemainingTurns: 4, MinSwitchSavingsUSD: 0.01, MinSwitchSavingsPct: 0.10, SwitchConfidence: 0.60,
			CostMode: config.CostModeAbsolute, CostReferenceUSD: 0.10,
			QualityFloors:    map[string]float64{"trivial": 0.70, "low": 0.70, "medium": 0.85, "high": 0.95},
			HighQualityFloor: 0.95, ReasoningBonus: 0.05,
			WorkflowAffinity: true,
		},
		Providers: []ProviderDocument{{
			Name: "anthropic", Kind: "anthropic", Location: config.ProviderLocationRemote, APIKeyEnv: "ANTHROPIC_API_KEY",
		}},
		Catalog: []CatalogDocument{{
			ID: "anthropic/claude-sonnet", Quality: 0.9, CostPerMTokIn: 3, CostPerMTokOut: 15, Speed: 0.75,
			Caps: config.Caps{Tools: true, Vision: true, Reasoning: true, MaxContext: 200000},
		}},
	}
}

func documentFromConfig(cfg *config.Config) Document {
	doc := Document{
		ServerAddr:        cfg.ServerAddr,
		AuthTokenEnv:      cfg.AuthTokenEnv,
		ClassifierEnabled: cfg.Classifier.Model != "",
		Classifier: ClassifierDocument{
			Model: cfg.Classifier.Model, MaxTokens: cfg.Classifier.MaxTokens, Timeout: cfg.Classifier.Timeout.String(),
		},
		Weights: cfg.Weights,
		Routing: RoutingDocument{
			Strategy: cfg.Routing.Strategy, DataPolicy: cfg.Routing.DataPolicy, SessionTTL: cfg.Routing.SessionTTL.String(),
			CacheAware: cfg.Routing.CacheAware, MaxAttempts: cfg.Routing.MaxAttempts,
			DefaultRemainingTurns: cfg.Routing.DefaultRemainingTurns,
			MinSwitchSavingsUSD:   cfg.Routing.MinSwitchSavingsUSD,
			MinSwitchSavingsPct:   cfg.Routing.MinSwitchSavingsPct,
			SwitchConfidence:      cfg.Routing.SwitchConfidence,
			CostMode:              cfg.Routing.CostMode,
			CostReferenceUSD:      cfg.Routing.CostReferenceUSD,
			QualityFloors:         cloneFloatMap(cfg.Routing.QualityFloors),
			HighQualityFloor:      cfg.Routing.HighQualityFloor,
			ReasoningBonus:        cfg.Routing.ReasoningBonus,
			WorkflowAffinity:      cfg.Routing.WorkflowAffinity,
			Background:            cfg.Routing.Background,
		},
		Catalog: make([]CatalogDocument, 0, len(cfg.Catalog)),
	}
	for name, provider := range cfg.Providers {
		doc.Providers = append(doc.Providers, ProviderDocument{
			Name: name, Kind: provider.Kind, Location: provider.Location,
			APIKeyEnv: provider.APIKeyEnv, APIKey: provider.APIKey, BaseURL: provider.BaseURL,
		})
	}
	sort.Slice(doc.Providers, func(i, j int) bool { return doc.Providers[i].Name < doc.Providers[j].Name })
	for _, entry := range cfg.Catalog {
		doc.Catalog = append(doc.Catalog, CatalogDocument{
			ID: entry.ID, Quality: entry.Quality, CostPerMTokIn: entry.CostPerMTokIn,
			CostPerMTokOut: entry.CostPerMTokOut, Speed: entry.Speed, Caps: entry.Caps, TurnEfficiency: entry.TurnEfficiency,
		})
	}
	return doc
}

func (d Document) config() (*config.Config, error) {
	ttl, err := time.ParseDuration(d.Routing.SessionTTL)
	if err != nil {
		return nil, fmt.Errorf("routing session TTL: %w", err)
	}
	cfg := &config.Config{
		ServerAddr:   d.ServerAddr,
		AuthTokenEnv: d.AuthTokenEnv,
		Weights:      d.Weights,
		Routing: config.RoutingCfg{
			Strategy: d.Routing.Strategy, DataPolicy: d.Routing.DataPolicy,
			SessionTTL: ttl, CacheAware: d.Routing.CacheAware,
			MaxAttempts: d.Routing.MaxAttempts, DefaultRemainingTurns: d.Routing.DefaultRemainingTurns,
			MinSwitchSavingsUSD: d.Routing.MinSwitchSavingsUSD, MinSwitchSavingsPct: d.Routing.MinSwitchSavingsPct,
			SwitchConfidence: d.Routing.SwitchConfidence,
			CostMode:         d.Routing.CostMode, CostReferenceUSD: d.Routing.CostReferenceUSD,
			QualityFloors:    cloneFloatMap(d.Routing.QualityFloors),
			HighQualityFloor: d.Routing.HighQualityFloor,
			ReasoningBonus:   d.Routing.ReasoningBonus,
			WorkflowAffinity: d.Routing.WorkflowAffinity, Background: d.Routing.Background,
		},
		Providers: make(map[string]config.ProviderCreds, len(d.Providers)),
		Catalog:   make([]config.CatalogEntry, 0, len(d.Catalog)),
	}
	if d.ClassifierEnabled {
		timeout, err := time.ParseDuration(d.Classifier.Timeout)
		if err != nil {
			return nil, fmt.Errorf("classifier timeout: %w", err)
		}
		cfg.Classifier = config.ClassifierCfg{Model: d.Classifier.Model, MaxTokens: d.Classifier.MaxTokens, Timeout: timeout}
	}
	for _, provider := range d.Providers {
		if provider.Name == "" {
			return nil, fmt.Errorf("provider name is required")
		}
		if _, exists := cfg.Providers[provider.Name]; exists {
			return nil, fmt.Errorf("provider %q is duplicated", provider.Name)
		}
		cfg.Providers[provider.Name] = config.ProviderCreds{
			Kind: provider.Kind, Location: provider.Location,
			APIKeyEnv: provider.APIKeyEnv, APIKey: provider.APIKey, BaseURL: provider.BaseURL,
		}
	}
	for _, entry := range d.Catalog {
		cfg.Catalog = append(cfg.Catalog, config.CatalogEntry{
			ID: entry.ID, Quality: entry.Quality, CostPerMTokIn: entry.CostPerMTokIn,
			CostPerMTokOut: entry.CostPerMTokOut, Speed: entry.Speed, Caps: entry.Caps, TurnEfficiency: entry.TurnEfficiency,
		})
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func cloneFloatMap(source map[string]float64) map[string]float64 {
	if source == nil {
		return nil
	}
	result := make(map[string]float64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
