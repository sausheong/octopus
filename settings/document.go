package settings

import (
	"fmt"
	"sort"
	"time"

	"github.com/sausheong/octopus/config"
)

type Document struct {
	ServerAddr        string             `json:"server_addr"`
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
	SessionSticky bool   `json:"session_sticky"`
	SessionTTL    string `json:"session_ttl"`
	CacheAware    bool   `json:"cache_aware"`
	MaxAttempts   int    `json:"max_attempts"`
}

type ProviderDocument struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	APIKeyEnv string `json:"api_key_env"`
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url"`
}

type CatalogDocument struct {
	ID             string      `json:"id"`
	Quality        float64     `json:"quality"`
	CostPerMTokIn  float64     `json:"cost_per_mtok_in"`
	CostPerMTokOut float64     `json:"cost_per_mtok_out"`
	Speed          float64     `json:"speed"`
	Caps           config.Caps `json:"caps"`
}

func defaultDocument() Document {
	return Document{
		ServerAddr: "127.0.0.1:8787",
		Weights:    config.Weights{Quality: 0.5, Cost: 0.3, Speed: 0.2},
		Routing:    RoutingDocument{SessionSticky: true, SessionTTL: "1h", CacheAware: true, MaxAttempts: 3},
		Providers: []ProviderDocument{{
			Name: "anthropic", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY",
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
		ClassifierEnabled: cfg.Classifier.Model != "",
		Classifier: ClassifierDocument{
			Model: cfg.Classifier.Model, MaxTokens: cfg.Classifier.MaxTokens, Timeout: cfg.Classifier.Timeout.String(),
		},
		Weights: cfg.Weights,
		Routing: RoutingDocument{
			SessionSticky: cfg.Routing.SessionSticky,
			SessionTTL:    cfg.Routing.SessionTTL.String(),
			CacheAware:    cfg.Routing.CacheAware,
			MaxAttempts:   cfg.Routing.MaxAttempts,
		},
		Catalog: make([]CatalogDocument, 0, len(cfg.Catalog)),
	}
	for name, provider := range cfg.Providers {
		doc.Providers = append(doc.Providers, ProviderDocument{
			Name: name, Kind: provider.Kind, APIKeyEnv: provider.APIKeyEnv, APIKey: provider.APIKey, BaseURL: provider.BaseURL,
		})
	}
	sort.Slice(doc.Providers, func(i, j int) bool { return doc.Providers[i].Name < doc.Providers[j].Name })
	for _, entry := range cfg.Catalog {
		doc.Catalog = append(doc.Catalog, CatalogDocument{
			ID: entry.ID, Quality: entry.Quality, CostPerMTokIn: entry.CostPerMTokIn,
			CostPerMTokOut: entry.CostPerMTokOut, Speed: entry.Speed, Caps: entry.Caps,
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
		ServerAddr: d.ServerAddr,
		Weights:    d.Weights,
		Routing: config.RoutingCfg{
			SessionSticky: d.Routing.SessionSticky,
			SessionTTL:    ttl,
			CacheAware:    d.Routing.CacheAware,
			MaxAttempts:   d.Routing.MaxAttempts,
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
			Kind: provider.Kind, APIKeyEnv: provider.APIKeyEnv, APIKey: provider.APIKey, BaseURL: provider.BaseURL,
		}
	}
	for _, entry := range d.Catalog {
		cfg.Catalog = append(cfg.Catalog, config.CatalogEntry{
			ID: entry.ID, Quality: entry.Quality, CostPerMTokIn: entry.CostPerMTokIn,
			CostPerMTokOut: entry.CostPerMTokOut, Speed: entry.Speed, Caps: entry.Caps,
		})
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
