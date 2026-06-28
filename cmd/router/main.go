// Command router runs the Anthropic-compatible LLM routing server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/sausheong/llmrouter/config"
	"github.com/sausheong/llmrouter/registry"
	"github.com/sausheong/llmrouter/router"
	"github.com/sausheong/llmrouter/server"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config YAML")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// Resolve every provider's key into its config (reading api_key_env now,
	// while the environment is still intact), then scrub the ambient
	// ANTHROPIC_* credentials. The Anthropic SDK otherwise picks up
	// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY from the process environment
	// and sends it as an Authorization: Bearer header alongside the explicit
	// per-provider x-api-key — Anthropic-compatible backends (DeepSeek,
	// MiniMax, Qwen) authenticate off that Bearer header, so the ambient
	// token would leak into and break every non-litellm backend. Pinning each
	// key inline and clearing the env guarantees each client uses only its own.
	for name, creds := range cfg.Providers {
		creds.APIKey = creds.Key()
		cfg.Providers[name] = creds
	}
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_API_KEY")

	reg, err := registry.New(context.Background(), cfg)
	if err != nil {
		slog.Error("registry init failed", "err", err)
		os.Exit(1)
	}

	rt := router.NewRouter(cfg, reg)
	srv := server.New(rt, reg)

	slog.Info("llmrouter listening", "addr", cfg.ServerAddr)
	if err := http.ListenAndServe(cfg.ServerAddr, srv.Handler()); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
