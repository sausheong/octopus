// Package registry builds and holds one harness LLMProvider per configured
// provider, and resolves "provider/model" ids to the provider plus bare
// model name for execution.
package registry

import (
	"context"
	"fmt"
	"os"

	"github.com/sausheong/harness/llm"
	"github.com/sausheong/harness/providers/anthropic"
	"github.com/sausheong/harness/providers/gemini"
	"github.com/sausheong/harness/providers/openai"
	"github.com/sausheong/harness/providers/qwen"
	"github.com/sausheong/llmrouter/config"
)

// Registry maps provider name -> constructed llm.LLMProvider.
type Registry struct {
	providers map[string]llm.LLMProvider
}

// New constructs a provider per cfg.Providers entry. The API key is read from
// the env var named by APIKeyEnv; an empty value is a fatal misconfiguration.
// Unknown provider names are an error (we only know the four harness backends).
func New(ctx context.Context, cfg *config.Config) (*Registry, error) {
	r := &Registry{providers: make(map[string]llm.LLMProvider, len(cfg.Providers))}
	for name, creds := range cfg.Providers {
		key := os.Getenv(creds.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("provider %q: env %q is empty", name, creds.APIKeyEnv)
		}
		switch name {
		case "anthropic":
			r.providers[name] = anthropic.NewAnthropicProvider(key, creds.BaseURL)
		case "openai":
			r.providers[name] = openai.NewOpenAIProvider(key, creds.BaseURL)
		case "qwen":
			r.providers[name] = qwen.NewQwenProvider(key, creds.BaseURL)
		case "gemini":
			g, err := gemini.NewGeminiProvider(ctx, key)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			r.providers[name] = g
		default:
			return nil, fmt.Errorf("unknown provider %q (want anthropic|openai|gemini|qwen)", name)
		}
	}
	return r, nil
}

// Resolve splits a "provider/model" id and returns the provider object plus
// the bare model name to set on llm.ChatRequest.Model.
func (r *Registry) Resolve(id string) (llm.LLMProvider, string, error) {
	provName, model := llm.ParseProviderModel(id)
	if provName == "" {
		return nil, "", fmt.Errorf("id %q is not in provider/model form", id)
	}
	prov, ok := r.providers[provName]
	if !ok {
		return nil, "", fmt.Errorf("no provider configured for %q", provName)
	}
	return prov, model, nil
}
