package registry

import (
	"context"
	"os"
	"testing"

	"github.com/sausheong/llmrouter/config"
)

func TestNewAndResolve(t *testing.T) {
	os.Setenv("RT_ANTHROPIC", "sk-ant-test")
	cfg := &config.Config{
		Providers: map[string]config.ProviderCreds{
			"anthropic": {APIKeyEnv: "RT_ANTHROPIC"},
		},
	}
	r, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prov, model, err := r.Resolve("anthropic/claude-haiku-3-5-20241022")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prov == nil {
		t.Fatal("provider is nil")
	}
	if model != "claude-haiku-3-5-20241022" {
		t.Errorf("model = %q", model)
	}
}

func TestNewMissingEnv(t *testing.T) {
	os.Unsetenv("RT_MISSING")
	cfg := &config.Config{
		Providers: map[string]config.ProviderCreds{
			"anthropic": {APIKeyEnv: "RT_MISSING"},
		},
	}
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("expected error for empty key env")
	}
}

func TestResolveUnknownProvider(t *testing.T) {
	os.Setenv("RT_ANTHROPIC", "sk-ant-test")
	cfg := &config.Config{
		Providers: map[string]config.ProviderCreds{"anthropic": {APIKeyEnv: "RT_ANTHROPIC"}},
	}
	r, _ := New(context.Background(), cfg)
	if _, _, err := r.Resolve("ghost/m"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
